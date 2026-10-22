#!/usr/bin/env node
// Run only against a disposable gateway created by native.py. Synthetic data.
import assert from 'node:assert/strict'
import fs from 'node:fs'
import vm from 'node:vm'
import { randomUUID } from 'node:crypto'
const base = process.env.SILENCER_TEST_URL
const admin = process.env.SILENCER_TEST_TOKEN
const statePath = process.env.SILENCER_TEST_STATE
assert.ok(base?.startsWith('http://127.0.0.1:'))
const original = '测试😀 "quoted" \\path\ncontact@example.invalid'
const browserScope = '1234567890abcdef1234567890abcdef'
const richText = (text, marker) => [[[[marker, ['plain-text', marker, text]]]]]
const context = vm.createContext({ URL, TextEncoder })
vm.runInContext(fs.readFileSync(new URL('../webchat-protocols.js', import.meta.url), 'utf8'), context)
const codec = context.SILENCER_WEBCHAT
async function api(path, body, token = admin) {
  const result = await fetch(base + path, { method: body == null ? 'GET' : 'POST', headers: { 'X-Silencer-Token': token, 'Content-Type': 'application/json' }, body: body == null ? undefined : JSON.stringify(body) })
  assert.equal(result.status, 200, 'isolated gateway ' + path)
  return result.json()
}
if (process.env.SILENCER_TEST_PHASE === 'seed') await api('/api/config', { ext_bridge_enabled: true, sensitive: { TEST: [original] } })
const config = await api('/api/config')
const token = config.ext_token
assert.equal(typeof token, 'string')
assert.equal((await api('/api/ext/ping', null, token)).ext_protocol, 3)
const mask = (text, host, scope = browserScope) => api('/api/ext/mask', { text, host, scope, url: 'https://' + host + '/synthetic-test', method: 'POST' }, token)
async function responseSession(sid) {
  const stream = randomUUID()
  return {
    async restore(text) { return (await api('/api/ext/restore', { sid, stream_id: stream, text, value_complete: true, content_type: 'text/plain' }, token)).text },
    async finish() {
      const result = await api('/api/ext/restore', { sid, stream_id: stream, text: '', final: true, await_ack: true, content_type: 'text/plain', outcome: 'completed' }, token)
      await api('/api/ext/restore', { sid, stream_id: stream, text: '', final: true, content_type: 'text/plain', outcome: 'completed' }, token)
      return result.text
    },
  }
}
if (process.env.SILENCER_TEST_PHASE === 'seed') {
  const saved = []
  for (const [id, host] of [['chatgpt', 'chatgpt.com'], ['gemini', 'gemini.google.com'], ['deepseek', 'chat.deepseek.com'], ['doubao', 'www.doubao.com'], ['kimi', 'www.kimi.com']]) {
    const provider = codec.providers.find((p) => p.id === id)
    const raw = id === 'gemini'
      ? 'at=synthetic-csrf&f.req=' + encodeURIComponent(JSON.stringify([null, JSON.stringify([[original]])]))
      : JSON.stringify({ messages: [{ content: id === 'doubao' ? JSON.stringify({ text: original }) : { parts: [original] } }] })
    let session, placeholder
    const request = await codec.maskRequest(provider, raw, async (text) => {
      session = await mask(text, host)
      placeholder = JSON.parse(session.text)[0]
      return { ...session, masked_text: session.text }
    })
    assert.notEqual(placeholder, original)
    assert.doesNotMatch(decodeURIComponent(request.masked_text), /contact@example.invalid/)
    const upstream = await responseSession(session.sid)
    let wire, contentType
    if (id === 'gemini') {
      const inner = [null, null, null, null, [['candidate', [placeholder]]]]
      inner[26] = richText(placeholder, 'synthetic-node-id')
      const body = JSON.stringify([['wrb.fr', 'test', JSON.stringify(inner)]])
      wire = ")]}'\n\n" + (body.length + 1) + '\n' + body + '\n'; contentType = 'application/json'
    } else if (id === 'chatgpt' || id === 'deepseek') {
      const initial = id === 'chatgpt' ? { message: { id: 'synthetic-assistant', content: { parts: ['{{', '{{'] } } }
        : { response: { message_id: 1, fragments: [{ id: 2, type: 'RESPONSE', content: '{{' }] } }
      const path = id === 'chatgpt' ? '/message/content/parts/0' : 'response/fragments/-1/content'
      const patch = id === 'deepseek'
        ? { o: 'BATCH', v: [{ p: path, o: 'APPEND', v: placeholder.slice(2) }] }
        : { o: 'patch', v: [{ p: path, o: 'remove' }, { p: path, o: 'append', v: placeholder.slice(2) }] }
      wire = [{ v: initial }, patch].map((value) => 'data: ' + JSON.stringify(value) + '\n\n').join('')
      contentType = 'text/event-stream'
    } else {
      const cut = Math.floor(placeholder.length / 2)
      wire = [placeholder.slice(0, cut), placeholder.slice(cut)].map((text) => 'data: ' + JSON.stringify({ text }) + '\n\n').join('')
      contentType = 'text/event-stream'
    }
    const stream = codec.createRestorer(provider, contentType, upstream.restore)
    let output = ''
    for (let i = 0; i < wire.length; i += 7) output += await stream.push(wire.slice(i, i + 7))
    output += await stream.push('', true)
    assert.equal(await upstream.finish(), '')
    if (id === 'gemini') {
      const body = output.slice(output.indexOf('\n\n') + 2), newline = body.indexOf('\n'), json = body.slice(newline + 1).trim()
      assert.equal(json.length + 1, Number(body.slice(0, newline)))
      const restored = JSON.parse(JSON.parse(json)[0][2])
      assert.equal(restored[4][0][1][0], original)
      assert.equal(restored[26][0][0][0][1][2], original)
      assert.equal(restored[26][0][0][0][0], 'synthetic-node-id')
    } else if (id === 'chatgpt' || id === 'deepseek') {
      const frames = output.trim().split('\n\n').map((frame) => JSON.parse(frame.slice(6)))
      if (id === 'chatgpt') {
        assert.equal('v' in frames[1].v[0], false, 'ChatGPT REMOVE has no v field')
        assert.equal(frames[1].v[1].v, original)
      } else assert.equal(frames[1].v[0].v, original)
    } else {
      assert.equal(output.trim().split('\n\n').map((frame) => JSON.parse(frame.slice(6)).text).join(''), original)
    }
    saved.push({ host, placeholder })
  }
  // A later read-only history request must resolve the prior Gemini mapping.
  const previousGemini = saved.find((item) => item.host === 'gemini.google.com')
  const historyProvider = codec.match('https://gemini.google.com/_/BardChatUi/data/batchexecute?rpcids=hNvQHb', 'https://gemini.google.com')
  const form = 'at=synthetic-csrf&f.req=' + encodeURIComponent(JSON.stringify([[['hNvQHb', JSON.stringify(['c_test', 10, null, 1, [1], [4], null, 1]), null, 'generic']]]))
  let historySession
  const historyRequest = await codec.maskRequest(historyProvider, form, async (text) => {
    assert.equal(text, '[]')
    historySession = await mask(text, 'gemini.google.com')
    return { ...historySession, masked_text: historySession.text }
  })
  assert.equal(historyRequest.masked_text, form)
  const candidate = Array(38).fill(null)
  candidate[0] = 'candidate-id'
  candidate[1] = [previousGemini.placeholder]
  candidate[22] = [previousGemini.placeholder]
  candidate[37] = [[previousGemini.placeholder]]
  const turn = [[null, 'turn-id'], null, [[previousGemini.placeholder]], [[candidate]]]
  turn[3][12] = richText(previousGemini.placeholder, 'synthetic-history-node')
  const historyBody = JSON.stringify([['wrb.fr', 'hNvQHb', JSON.stringify([[turn]])]])
  const historyUpstream = await responseSession(historySession.sid)
  const historyStream = codec.createRestorer(historyProvider, 'application/json', historyUpstream.restore)
  const historyOutput = await historyStream.push(")]}'\n\n" + (historyBody.length + 1) + '\n' + historyBody, true)
  const historyJSON = JSON.parse(historyOutput.slice(historyOutput.indexOf('\n', 6) + 1))
  const restoredTurn = JSON.parse(historyJSON[0][2])[0][0]
  assert.equal(restoredTurn[2][0][0], original)
  assert.equal(restoredTurn[3][0][0][1][0], original)
  assert.equal(restoredTurn[3][0][0][22][0], original)
  assert.equal(restoredTurn[3][0][0][37][0][0], original)
  assert.equal(restoredTurn[3][12][0][0][0][1][2], original)
  assert.equal(restoredTurn[3][12][0][0][0][0], 'synthetic-history-node')
  assert.equal(await historyUpstream.finish(), '')
  fs.writeFileSync(statePath, JSON.stringify(saved), { mode: 0o600 })
  console.log('Native C++: five browser codecs, nested escaping and split-event restoration passed')
} else {
  const saved = JSON.parse(fs.readFileSync(statePath))
  for (const { host, placeholder } of saved) {
    const session = await mask('[]', host)
    const response = await responseSession(session.sid)
    assert.equal(await response.restore(placeholder), original)
    assert.equal(await response.finish(), '')
  }
  for (const [host, scope] of [['other-origin.example', browserScope], ['chatgpt.com', 'ffffffffffffffffffffffffffffffff']]) {
    const session = await mask('[]', host, scope)
    const response = await responseSession(session.sid)
    assert.notEqual(await response.restore(saved[0].placeholder), original)
    await response.finish()
  }
  console.log('Native C++: historical mappings survived gateway restart; site/install scopes isolated')
}
