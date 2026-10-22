#!/usr/bin/env node
// Exercise the real MAIN/SW bridge with deterministic responses; no network.
import assert from 'node:assert/strict'
import fs from 'node:fs'
import vm from 'node:vm'
import { webcrypto } from 'node:crypto'

const read = (file) => fs.readFileSync(new URL(`../extension/${file}`, import.meta.url), 'utf8')
const source = { shared: read('shared.js'), sw: read('background.js'), main: read('bridge-main.js'), webchat: read('webchat-protocols.js') }
const waitForMessages = () => new Promise((resolve) => setTimeout(resolve, 20))

function storage(initial = {}) {
  const values = { ...initial }
  return {
    async get(keys) { return keys == null ? { ...values } : Object.fromEntries((Array.isArray(keys) ? keys : [keys]).filter((key) => key in values).map((key) => [key, values[key]])) },
    async set(patch) { Object.assign(values, patch) },
    async remove(keys) { for (const key of keys) delete values[key] },
  }
}

function browser({ noMatch = false, legacyField = false, maskTransform = (text) => text.replaceAll('alice@example.com', '{{EMAIL_0}}'), responseType = 'application/json', failure = null,
  restoreFailure = () => false, restoreHook = async () => {}, upstream = null, hostname = 'chatgpt.com', enabled = true, search = '' } = {}) {
  const panelCalls = [], upstreamCalls = []
  let maskSequence = 0
  let listener
  const worker = vm.createContext({
    URL, AbortController, setTimeout, clearTimeout, crypto: webcrypto,
    chrome: {
      storage: { local: storage({ token: 'test-token', panelUrl: 'http://127.0.0.1:8080', enabledSites: [hostname], enabled }), session: storage() },
      runtime: {
        id: 'test-extension', onMessage: { addListener(fn) { listener = fn } },
        onInstalled: { addListener() {} }, onStartup: { addListener() {} },
      },
      action: { async setBadgeText() {}, async setBadgeBackgroundColor() {} },
    },
    async fetch(url, init) {
      const path = new URL(url).pathname
      const body = init.body ? JSON.parse(init.body) : {}
      panelCalls.push({ path, body })
      if (path.endsWith('/mask')) maskSequence++
      if (path.endsWith('/restore')) {
        await restoreHook(body)
        if (restoreFailure(body)) return { status: 502, async json() { return { ok: false, error: 'engine_unavailable' } } }
      }
      const data = path.endsWith('/ping') ? { ok: true, ext_protocol: 3, block_when_down: true }
        : path.endsWith('/mask') ? { ok: true, [legacyField ? 'masked_text' : 'text']: noMatch ? body.text : maskTransform(body.text), sid: 'ext:' + maskSequence.toString(16).padStart(32, '0'), request_id: 'b'.repeat(32) }
          : { ok: true, text: body.text.replaceAll('{{EMAIL_0}}', 'alice@example.com') }
      return { status: 200, async json() { return data } }
    },
  })
  worker.self = worker
  vm.runInContext(source.shared, worker)
  vm.runInContext(source.sw, worker)

  class NativeXHR {
    constructor() { this.listeners = new Map(); this.readyState = 0; this.status = 0; this.responseType = ''; this.raw = '' }
    open() { this.readyState = 1 }
    send(body) { this.sent = true; this.sentBody = body }
    abort() { if (this.sent) this.complete('abort') }
    setRequestHeader() {}
    addEventListener(type, callback, options) {
      const list = this.listeners.get(type) || []
      list.push({ callback, once: options?.once }); this.listeners.set(type, list)
    }
    removeEventListener(type, callback) { this.listeners.set(type, (this.listeners.get(type) || []).filter((item) => item.callback !== callback)) }
    dispatchEvent(event) { this.emit(event.type); return true }
    emit(type) {
      for (const item of [...(this.listeners.get(type) || [])]) {
        if (item.once) this.removeEventListener(type, item.callback)
        item.callback.call(this, { type })
      }
      if (Object.hasOwn(this, 'on' + type)) this['on' + type]?.({ type })
    }
    complete(outcome = 'load') {
      this.readyState = 4; this.status = outcome === 'load' ? 201 : 0
      this.raw = outcome === 'load' ? '{"reply":"{{EMAIL_0}}"}' : ''
      this.emit('readystatechange'); this.emit(outcome); this.emit('loadend')
    }
    getResponseHeader() { return 'application/json' }
    get responseText() { return this.raw }
    get response() { return this.raw }
  }
  const callbacks = []
  const location = { hostname, search, origin: 'https://' + hostname, href: 'https://' + hostname + '/' + search }
  const window = {
    XMLHttpRequest: NativeXHR,
    addEventListener(type, callback) { if (type === 'message') callbacks.push(callback) },
    postMessage(message) {
      if (message.type !== 'SILENCER_BRIDGE_REQ') return
      queueMicrotask(() => {
        listener({ action: message.action, payload: message.payload }, {
          id: 'test-extension', tab: { id: 1, url: location.href }, url: location.href,
        }, (result) => {
          for (const callback of callbacks) callback({ origin: location.origin, data: { type: 'SILENCER_BRIDGE_RESP', nonce: message.nonce, result } })
        })
      })
    },
    async fetch(...args) {
      upstreamCalls.push(args)
      if (failure) throw failure
      if (upstream) return upstream(...args)
      return new Response('{"reply":"{{EMAIL_0}}"}', { status: 201, headers: { 'content-type': responseType } })
    },
  }
  let diagnosticMeta = null
  const document = {
    documentElement: { appendChild(node) { diagnosticMeta = node } },
    querySelector() { return diagnosticMeta },
    createElement() { return { name: '', content: '' } },
    addEventListener() {},
  }
  const main = vm.createContext({
    window, location, document, crypto: webcrypto, URL, Request, Response, Headers, FormData, Blob, URLSearchParams,
    ReadableStream, TextDecoder, TextEncoder, ArrayBuffer, Uint8Array, setTimeout, clearTimeout,
    ProgressEvent: class { constructor(type) { this.type = type } },
  })
  vm.runInContext(source.webchat, main)
  vm.runInContext(source.main, main)
  return { window, panelCalls, upstreamCalls, diagnostic: () => diagnosticMeta?.content,
    sendRuntime: (message, sender) => new Promise((resolve) => listener(message, sender, resolve)) }
}

const target = 'https://chatgpt.com/backend-api/conversation'
const input = { method: 'POST', body: '{"message":"alice@example.com"}' }
const normal = browser()
const response = await normal.window.fetch(target, input)
assert.match(normal.upstreamCalls[0][1].body, /\{\{EMAIL_0\}\}/)
assert.doesNotMatch(normal.upstreamCalls[0][1].body, /alice@example\.com/)
assert.equal(await response.text(), '{"reply":"alice@example.com"}')
const mask = normal.panelCalls.find((call) => call.path.endsWith('/mask')).body
assert.equal(mask.url, target)
assert.equal(mask.method, 'POST')
assert.equal(JSON.parse(normal.diagnostic()).chatgptResponseMode, undefined)
await waitForMessages()
const frames = normal.panelCalls.filter((call) => call.path.endsWith('/restore')).map((call) => call.body)
assert.equal(frames.at(-1).final, true)
assert.equal(frames.at(-1).outcome, 'completed')
assert.equal(frames.at(-1).response_status, 201)
assert.equal(new Set(frames.map((frame) => frame.stream_id)).size, 1)
assert.equal(frames.filter((frame) => frame.final && frame.await_ack).length, 1)
assert.equal(frames.at(-1).await_ack, false)

const prepareCredential = 'synthetic-transport-credential'
const preparing = browser({ upstream: async () => new Response(JSON.stringify({ conduit_token: prepareCredential }), { headers: { 'content-type': 'application/json' } }) })
const prepareReply = await preparing.window.fetch('/backend-api/f/conversation/prepare', {
  method: 'POST', body: JSON.stringify({ partial_query: { content: { parts: ['alice@example.com'] } }, model: 'preserve-model' }),
})
assert.equal((await prepareReply.json()).conduit_token, prepareCredential)
await waitForMessages()
assert.equal(preparing.panelCalls.find((call) => call.path.endsWith('/mask')).body.text, JSON.stringify(['alice@example.com']))
assert.equal(preparing.panelCalls.find((call) => call.path.endsWith('/mask')).body.url, 'https://chatgpt.com/backend-api/f/conversation/prepare')
assert.equal(preparing.panelCalls.some((call) => JSON.stringify(call.body).includes(prepareCredential)), false)
for (const path of ['/backend-api/sentinel/chat-requirements/prepare', '/backend-api/composer/items/interactions', '/backend-api/conversation/init']) {
  const auxiliary = browser()
  await auxiliary.window.fetch(path, { method: 'POST', body: JSON.stringify({ text: 'alice@example.com' }) })
  assert.equal(auxiliary.panelCalls.filter((call) => call.path.endsWith('/mask')).length, 0)
}
const relativeTarget = browser({ hostname: 'chat.deepseek.com' })
await (await relativeTarget.window.fetch('/api/v0/chat/completion', input)).text()
assert.equal(relativeTarget.panelCalls.find((call) => call.path.endsWith('/mask')).body.url, 'https://chat.deepseek.com/api/v0/chat/completion')

const unchanged = browser({ noMatch: true })
await (await unchanged.window.fetch(target, input)).text()
assert.equal(unchanged.upstreamCalls[0][1], input)
assert.equal(unchanged.panelCalls.filter((call) => call.path.endsWith('/restore')).at(-1).body.final, true)

const cancelled = browser()
await (await cancelled.window.fetch(target, input)).body.cancel('stop')
await waitForMessages()
assert.equal(cancelled.panelCalls.filter((call) => call.path.endsWith('/restore')).at(-1).body.outcome, 'cancelled')

const chatgptStreamCancelled = browser({ responseType: 'text/event-stream', upstream: async () => new Response(
  'data: ' + JSON.stringify({ message: { id: 'synthetic-private-message-id', content: { parts: ['{{EMAIL_0}}'] } } }) + '\n\n',
  { status: 200, headers: { 'content-type': 'text/event-stream' } },
) })
const chatgptStreamResponse = await chatgptStreamCancelled.window.fetch(target, input)
const chatgptStreamReader = chatgptStreamResponse.body.getReader()
const firstChatgptChunk = await chatgptStreamReader.read()
assert.equal(new TextDecoder().decode(firstChatgptChunk.value).includes('alice@example.com'), true)
await chatgptStreamReader.cancel('synthetic user cancellation')
await waitForMessages()
const chatgptStreamDiagnostic = JSON.parse(chatgptStreamCancelled.diagnostic())
assert.equal(chatgptStreamDiagnostic.build, 'webchat-1.1.2')
assert.equal(chatgptStreamDiagnostic.chatgptStream.state, 'consumer_cancelled')
assert.equal(chatgptStreamDiagnostic.chatgptStream.upstreamChunks, 1)
assert.equal(chatgptStreamDiagnostic.chatgptStream.upstreamEOF, false)
assert.ok(chatgptStreamDiagnostic.chatgptStream.outputBytes > 0)
assert.equal(chatgptStreamDiagnostic.chatgptStream.codec.provider, 'chatgpt')
assert.equal(chatgptStreamDiagnostic.chatgptStream.codec.records, 1)
assert.equal(chatgptStreamDiagnostic.chatgptStream.codec.sse.frames, 1)
assert.equal(chatgptStreamDiagnostic.chatgptStream.codec.sse.doneFrames, 0)
assert.equal(chatgptStreamDiagnostic.chatgptStream.codec.sse.shapeChanges, 0)
assert.equal(chatgptStreamDiagnostic.chatgptStream.codec.sse.recent[0].tag, 'other')
assert.deepEqual(chatgptStreamDiagnostic.chatgptStream.codec.sse.recent[0].operations, [])
assert.doesNotMatch(JSON.stringify(chatgptStreamDiagnostic.chatgptStream), /alice@example\.com|EMAIL_0|synthetic-private-message-id/)

let chatgptTerminalUpstreamCancelled = false
const chatgptTerminalFrame = 'data: ' + JSON.stringify({ message: { id: 'synthetic-terminal-message', content: { parts: ['{{EMAIL_0}}'] } } }) + '\n\ndata: [DONE]\n\n'
const chatgptTerminal = browser({ responseType: 'text/event-stream', upstream: async () => new Response(new ReadableStream({
  start(controller) { controller.enqueue(new TextEncoder().encode(chatgptTerminalFrame)) },
  cancel() { chatgptTerminalUpstreamCancelled = true },
}), { status: 200, headers: { 'content-type': 'text/event-stream' } }) })
const chatgptTerminalResponse = await chatgptTerminal.window.fetch(target, input)
const chatgptTerminalReader = chatgptTerminalResponse.body.getReader()
let chatgptTerminalOutput = ''
for (;;) {
  const { value, done } = await chatgptTerminalReader.read()
  if (done) break
  chatgptTerminalOutput += new TextDecoder().decode(value)
}
assert.match(chatgptTerminalOutput, /alice@example\.com/)
assert.match(chatgptTerminalOutput, /data: \[DONE\]/)
await waitForMessages()
const chatgptTerminalRestore = chatgptTerminal.panelCalls.filter((call) => call.path.endsWith('/restore')).map((call) => call.body)
const chatgptTerminalFinalize = chatgptTerminalRestore.find((body) => body.final && body.await_ack)
assert.ok(chatgptTerminalFinalize)
assert.equal(chatgptTerminalFinalize.outcome, 'completed')
assert.equal(chatgptTerminalUpstreamCancelled, true)
const chatgptTerminalDiagnostic = JSON.parse(chatgptTerminal.diagnostic())
assert.equal(chatgptTerminalDiagnostic.chatgptStream.state, 'completed_after_done')
assert.equal(chatgptTerminalDiagnostic.chatgptStream.codec.sse.doneFrames, 1)
assert.doesNotMatch(JSON.stringify(chatgptTerminalDiagnostic.chatgptStream), /alice@example\.com|EMAIL_0|synthetic-terminal-message/)

const debugResponseWire = 'data: ' + JSON.stringify({ message: { author: { role: 'assistant' }, content: { parts: ['{{EMAIL_0}}'] } } }) + '\n\ndata: [DONE]\n\n';
const chatgptQueryBypass = browser({
  search: '?__silencer_chatgpt_response=passthrough',
  responseType: 'text/event-stream',
  upstream: async () => new Response(debugResponseWire, { status: 200, headers: { 'content-type': 'text/event-stream' } }),
})
const securedResponse = await chatgptQueryBypass.window.fetch(target, input)
const securedText = await securedResponse.text()
assert.match(securedText, /alice@example\.com/, 'query parameters cannot bypass ChatGPT response restoration')
assert.doesNotMatch(securedText, /\{\{EMAIL_0\}\}/)
assert.match(chatgptQueryBypass.upstreamCalls[0][1].body, /\{\{EMAIL_0\}\}/, 'ChatGPT request masking remains active')
assert.doesNotMatch(chatgptQueryBypass.upstreamCalls[0][1].body, /alice@example\.com/)
await waitForMessages()
const securedFinal = chatgptQueryBypass.panelCalls.filter((call) => call.path.endsWith('/restore')).at(-1).body
assert.equal(securedFinal.outcome, 'completed')
const securedDiagnostic = JSON.parse(chatgptQueryBypass.diagnostic())
assert.equal(securedDiagnostic.build, 'webchat-1.1.2')
assert.equal(securedDiagnostic.chatgptResponseMode, undefined)
assert.equal(securedDiagnostic.chatgptStream.state, 'completed_after_done')

for (const name of ['TypeError', 'AbortError']) {
  const failure = Object.assign(new Error('upstream failed'), { name })
  const failed = browser({ failure })
  await assert.rejects(failed.window.fetch(target, input), (error) => error === failure)
  await waitForMessages()
  const end = failed.panelCalls.filter((call) => call.path.endsWith('/restore')).at(-1).body
  assert.equal(end.outcome, name === 'AbortError' ? 'cancelled' : 'network_error')
  assert.equal(end.final, true)
}

const unsupported = browser({ responseType: 'text/html' })
const untouched = await unsupported.window.fetch(target, input)
assert.equal(await untouched.text(), '{"reply":"{{EMAIL_0}}"}')
await waitForMessages()
assert.equal(unsupported.panelCalls.filter((call) => call.path.endsWith('/restore')).at(-1).body.outcome, 'unsupported_response')

const restoredFrames = (client) => client.panelCalls.filter((call) => call.path.endsWith('/restore')).map((call) => call.body)
const legacy = browser({ legacyField: true })
assert.equal(await (await legacy.window.fetch(target, input)).text(), '{"reply":"alice@example.com"}')

for (const finalOnly of [true, false]) {
  const failed = browser({ restoreFailure: (body) => finalOnly ? body.final && body.await_ack : !body.final })
  await assert.rejects((await failed.window.fetch(target, input)).text(), /webchat_restore_failed/)
  await waitForMessages()
  assert.equal(restoredFrames(failed).at(-1).outcome, 'restore_failed')
  assert.equal(restoredFrames(failed).at(-1).await_ack, false)
}

const broken = browser({ upstream: () => new Response(new ReadableStream({
  pull(controller) { controller.error(new Error('broken stream')) },
}), { headers: { 'content-type': 'application/json' } }) })
await assert.rejects((await broken.window.fetch(target, input)).text(), /broken stream/)
await waitForMessages()
assert.equal(restoredFrames(broken).at(-1).outcome, 'response_stream_error')

let releaseRestore, enteredRestore
const restoreEntered = new Promise((resolve) => { enteredRestore = resolve })
const restorePaused = new Promise((resolve) => { releaseRestore = resolve })
const duringRestore = browser({ restoreHook: async (body) => {
  if (!body.final) { enteredRestore(); await restorePaused }
} })
const restoreReader = (await duringRestore.window.fetch(target, input)).body.getReader()
const pendingRestoreRead = restoreReader.read()
await restoreEntered
await restoreReader.cancel('stop in flight')
releaseRestore()
await pendingRestoreRead
await waitForMessages()
assert.equal(restoredFrames(duringRestore).at(-1).outcome, 'cancelled')
assert.equal(restoredFrames(duringRestore).filter((body) => body.final && body.outcome === 'completed').length, 0)

let releaseRead
const readPaused = new Promise((resolve) => { releaseRead = resolve })
const duringRead = browser({ upstream: () => new Response(new ReadableStream({
  async pull(controller) { await readPaused; try { controller.close() } catch {} },
}), { headers: { 'content-type': 'application/json' } }) })
const readReader = (await duringRead.window.fetch(target, input)).body.getReader()
const pendingRead = readReader.read()
await readReader.cancel('stop while reading')
releaseRead()
await pendingRead
await waitForMessages()
assert.equal(restoredFrames(duringRead).at(-1).outcome, 'cancelled')

let enteredFinal, releaseFinal
const finalEntered = new Promise((resolve) => { enteredFinal = resolve })
const finalPaused = new Promise((resolve) => { releaseFinal = resolve })
const duringFinal = browser({ restoreHook: async (body) => {
  if (body.await_ack) { enteredFinal(); await finalPaused }
} })
const finalReader = (await duringFinal.window.fetch(target, input)).body.getReader()
await finalReader.read()
const finalRead = finalReader.read()
await finalEntered
await finalReader.cancel('stop during flush')
releaseFinal()
await finalRead
await waitForMessages()
assert.equal(restoredFrames(duringFinal).at(-1).outcome, 'cancelled')
assert.equal(restoredFrames(duringFinal).filter((body) => body.final && !body.await_ack && body.outcome === 'completed').length, 0)

for (const kind of ['multipart', 'blob', 'request-blob']) {
  const client = browser()
  const blob = new Blob(['alice@example.com'], { type: 'text/plain' })
  let upload = { method: 'PUT', body: blob }
  if (kind === 'multipart') {
    const form = new FormData()
    form.append('prompt', 'alice@example.com'); form.append('file', blob, 'note.txt')
    upload = { method: 'PATCH', body: form }
  }
  const uploadTarget = 'https://chatgpt.com/backend-api/files/upload'
  const request = kind === 'request-blob' ? new Request(uploadTarget, upload) : uploadTarget
  const uploadResponse = await client.window.fetch(request, kind === 'request-blob' ? undefined : upload)
  assert.equal(await uploadResponse.text(), '{"reply":"alice@example.com"}')
  await waitForMessages()
  const uploadedMask = client.panelCalls.find((call) => call.path.endsWith('/mask')).body
  assert.equal(uploadedMask.url, uploadTarget); assert.equal(uploadedMask.method, upload.method)
  assert.equal(restoredFrames(client).at(-1).outcome, 'completed')
  const sent = client.upstreamCalls[0][0] instanceof Request ? client.upstreamCalls[0][0] : client.upstreamCalls[0][1]
  const body = sent.body
  if (body instanceof FormData) assert.match(body.get('prompt'), /EMAIL_0/)
  else if (body instanceof Blob) { assert.equal(body.size, blob.size); assert.match(await body.text(), /EMAIL_0/) }
  else assert.match(await sent.clone().text(), /EMAIL_0/)
}

for (const kind of ['multipart', 'blob']) {
  const blob = new Blob(['plain text file'], { type: 'text/plain' })
  const body = kind === 'blob' ? blob : new FormData()
  if (body instanceof FormData) body.append('file', blob, 'note.txt')
  const originalInit = { method: 'POST', body }
  const client = browser({ noMatch: true })
  await (await client.window.fetch(target, originalInit)).text()
  await waitForMessages()
  assert.equal(client.upstreamCalls[0][1], originalInit)
  assert.equal(restoredFrames(client).at(-1).outcome, 'completed')
  const failed = browser({ failure: new TypeError('upload failed') })
  await assert.rejects(failed.window.fetch(target, originalInit), /upload failed/)
  await waitForMessages()
  assert.equal(restoredFrames(failed).at(-1).outcome, 'network_error')
}

const overrides = browser()
await (await overrides.window.fetch(new Request(target, { method: 'GET' }), input)).text()
assert.equal(overrides.upstreamCalls[0][0].method, 'POST')
assert.match(await overrides.upstreamCalls[0][0].clone().text(), /EMAIL_0/)

const cannotApply = browser({ maskTransform: () => '{{EMAIL_' + 'a'.repeat(40) + '}}' })
const originalBlob = new Blob(['alice@example.com'], { type: 'text/plain' })
const untouchedUpload = { method: 'PUT', body: originalBlob }
await cannotApply.window.fetch(target, untouchedUpload)
await waitForMessages()
assert.equal(cannotApply.upstreamCalls[0][1], untouchedUpload)
assert.equal(restoredFrames(cannotApply).at(-1).outcome, 'mask_not_applied')

for (const hostname of ['chatgpt.com', 'chat.deepseek.com']) {
  for (const outcome of ['load', 'error', 'abort', 'timeout']) {
    for (const responseType of ['', 'arraybuffer']) {
      const client = browser({ hostname })
      const xhr = new client.window.XMLHttpRequest()
      xhr.open('POST', `https://${hostname}/api/chat/completion`, true)
      xhr.responseType = responseType
      xhr.send(input.body)
      await waitForMessages()
      assert.match(xhr.sentBody, /EMAIL_0/)
      xhr.complete(outcome)
      await waitForMessages()
      const expected = outcome === 'abort' ? 'cancelled' : outcome !== 'load' ? 'network_error'
        : hostname.endsWith('deepseek.com') && responseType === '' ? 'completed' : 'unsupported_response'
      assert.equal(restoredFrames(client).at(-1).outcome, expected, `${hostname}/${responseType}/${outcome}`)
      if (expected === 'completed') assert.equal(xhr.responseText, '{"reply":"alice@example.com"}')
    }
  }
}
const xhrFailed = browser({ hostname: 'chat.deepseek.com', restoreFailure: (body) => body.final && body.await_ack })
const xhr = new xhrFailed.window.XMLHttpRequest()
xhr.open('POST', 'https://chat.deepseek.com/api/chat/completion', true)
xhr.send(input.body)
await waitForMessages()
xhr.complete()
await waitForMessages()
assert.equal(restoredFrames(xhrFailed).at(-1).outcome, 'restore_failed')

const reused = browser({ hostname: 'chat.deepseek.com' })
const reusedXHR = new reused.window.XMLHttpRequest()
for (let i = 0; i < 2; i++) {
  reusedXHR.open('POST', 'https://chat.deepseek.com/api/chat/completion', true)
  reusedXHR.send(input.body)
  await waitForMessages()
  reusedXHR.complete()
  await waitForMessages()
  assert.equal(reusedXHR.responseText, '{"reply":"alice@example.com"}')
}
const confirmed = restoredFrames(reused).filter((body) => body.final && !body.await_ack)
assert.equal(confirmed.length, 2)
assert.equal(new Set(confirmed.map((body) => body.sid)).size, 2)
assert.ok(confirmed.every((body) => body.outcome === 'completed'))

let oldRestoreEntered, oldRestoreRelease
const oldEntered = new Promise((resolve) => { oldRestoreEntered = resolve })
const oldPaused = new Promise((resolve) => { oldRestoreRelease = resolve })
const reopened = browser({ hostname: 'chat.deepseek.com', restoreHook: async (body) => {
  if (body.await_ack && body.sid.endsWith('1')) { oldRestoreEntered(); await oldPaused }
} })
const reopenedXHR = new reopened.window.XMLHttpRequest()
reopenedXHR.open('POST', 'https://chat.deepseek.com/api/chat/completion', true)
reopenedXHR.send(input.body)
await waitForMessages()
reopenedXHR.complete()
await oldEntered
reopenedXHR.open('POST', 'https://chat.deepseek.com/api/chat/completion', true)
reopenedXHR.send(input.body)
await waitForMessages()
reopenedXHR.complete()
oldRestoreRelease()
await waitForMessages()
assert.equal(reopenedXHR.responseText, '{"reply":"alice@example.com"}')
const reopenEnds = restoredFrames(reopened).filter((body) => body.final && !body.await_ack)
assert.equal(reopenEnds.find((body) => body.sid.endsWith('1')).outcome, 'cancelled')
assert.equal(reopenEnds.find((body) => body.sid.endsWith('2')).outcome, 'completed')
for (const [hostname, path, body] of [
  ['chatgpt.com', '/backend-api/f/conversation', JSON.stringify({ messages: [{ content: { parts: ['alice@example.com'] } }], model: 'keep-model' })],
  ['gemini.google.com', '/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate', 'at=keep-csrf&f.req=' + encodeURIComponent(JSON.stringify([null, JSON.stringify([['alice@example.com']])]))],
  ['chat.deepseek.com', '/api/v0/chat/completion', JSON.stringify({ prompt: 'alice@example.com', chat_session_id: 'keep-id' })],
  ['www.doubao.com', '/samantha/chat/completion', JSON.stringify({ messages: [{ content: JSON.stringify({ text: 'alice@example.com' }) }] })],
  ['www.kimi.com', '/api/chat/current/completion', JSON.stringify({ messages: [{ role: 'user', content: 'alice@example.com' }] })],
  ['www.kimi.com', '/api/chat/session-otp-token/completion', JSON.stringify({ messages: [{ content: 'alice@example.com' }] })],
]) {
  const client = browser({ hostname })
  const headers = { authorization: 'Bearer synthetic-provider-key', 'content-type': hostname.includes('gemini') ? 'application/x-www-form-urlencoded' : 'application/json' }
  const out = await client.window.fetch('https://' + hostname + path, { method: 'POST', body, headers })
  assert.equal(JSON.parse(await out.text()).reply, 'alice@example.com')
  const sent = client.upstreamCalls[0][1]
  assert.equal(sent.headers, headers)
  assert.doesNotMatch(decodeURIComponent(String(sent.body)), /alice@example.com/)
  assert.match(decodeURIComponent(String(sent.body)), /EMAIL_0/)
  assert.equal(client.upstreamCalls.length, 1)
  assert.equal(restoredFrames(client).at(-1).outcome, 'completed')
  const session = client.panelCalls.find((c) => c.path.endsWith('/mask'))
  const restoreCount = restoredFrames(client).length
  const denied = await client.sendRuntime({ action: 'restore', payload: { sid: session ? 'ext:' + '1'.padStart(32, '0') : '', stream_id: 'forged', text: '{{EMAIL_0}}' } }, {
    id: 'test-extension', tab: { id: 1, url: 'https://' + hostname }, url: 'https://another-origin.example/',
  })
  assert.equal(denied.ok, false)
  assert.equal(restoredFrames(client).length, restoreCount)
}
const changedShape = browser({ hostname: 'gemini.google.com' })
await assert.rejects(changedShape.window.fetch('https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate', { method: 'POST', body: 'unexpected=alice@example.com' }), /Failed to fetch/)
assert.equal(changedShape.upstreamCalls.length, 0)
const disabledAdapter = browser({ hostname: 'gemini.google.com', enabled: false })
await disabledAdapter.window.fetch('https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate', { method: 'POST', body: 'unexpected=raw-body' })
assert.equal(disabledAdapter.upstreamCalls.length, 1)
const authRequest = browser()
await authRequest.window.fetch('https://chatgpt.com/backend-api/auth/session', { method: 'POST', body: '{"email":"alice@example.com"}' })
assert.equal(authRequest.panelCalls.some((c) => c.path.endsWith('/mask')), false)

const eventClient = browser({ hostname: 'gemini.google.com' })
const eventXHR = new eventClient.window.XMLHttpRequest()
const eventCalls = []
const objectListener = { handleEvent(event) { eventCalls.push(event.type) } }
const cancelledListener = new AbortController()
eventXHR.addEventListener('load', objectListener, { once: true })
eventXHR.addEventListener('load', objectListener, { once: false }) // native deduplication
eventXHR.addEventListener('load', () => eventCalls.push('aborted-listener'), { signal: cancelledListener.signal })
cancelledListener.abort()
eventXHR.open('POST', 'https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate', true)
eventXHR.send('f.req=' + encodeURIComponent(JSON.stringify([null, JSON.stringify([['alice@example.com']])])))
await waitForMessages()
eventXHR.complete()
await waitForMessages()
assert.deepEqual(eventCalls, ['load'])
assert.equal(JSON.parse(eventXHR.responseText).reply, 'alice@example.com')
const captureCalls = []
const callback = () => captureCalls.push('capture')
eventXHR.addEventListener('custom', callback, true)
eventXHR.removeEventListener('custom', callback, false)
eventXHR.dispatchEvent({ type: 'custom' })
eventXHR.removeEventListener('custom', callback, true)
eventXHR.dispatchEvent({ type: 'custom' })
assert.deepEqual(captureCalls, ['capture'])

console.log('test-extension-ledger: OK (five site adapters, real bridge code, host isolation, disabled mode and lifecycle)')
