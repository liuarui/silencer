#!/usr/bin/env node
// Known-fault regressions for the extension's connection diagnostics; no network.
import assert from 'node:assert/strict'
import fs from 'node:fs'
import vm from 'node:vm'

const read = (name) => fs.readFileSync(new URL(`../extension/${name}`, import.meta.url), 'utf8')
const shared = read('shared.js')
const background = read('background.js')
const options = read('options.js')

function storage(initial = {}) {
  const values = { ...initial }
  return {
    values,
    async get(keys) {
      if (keys == null) return { ...values }
      return Object.fromEntries((Array.isArray(keys) ? keys : [keys]).map((key) => [key, values[key]]))
    },
    async set(patch) { Object.assign(values, patch) },
    async remove(keys) { for (const key of keys) delete values[key] },
    async getBytesInUse() { return 0 },
  }
}

function load(source, globals) {
  const context = vm.createContext({ URL, AbortController, setTimeout, clearTimeout, ...globals })
  context.self = context
  vm.runInContext(shared, context)
  vm.runInContext(source, context)
  return context
}

function worker() {
  const session = storage()
  let listener
  let response = { status: 200, body: { ok: true, ext_protocol: 3, version: '1.0.0', block_when_down: true } }
  const context = load(background, {
    chrome: {
      storage: { local: storage(), session },
      runtime: {
        id: 'test-extension',
        onMessage: { addListener(fn) { listener = fn } },
        onInstalled: { addListener() {} },
        onStartup: { addListener() {} },
      },
      action: { async setBadgeText() {}, async setBadgeBackgroundColor() {} },
    },
    async fetch() {
      if (response.error) throw response.error
      return { status: response.status, async json() {
        if (response.invalidJson) throw new SyntaxError('Invalid JSON')
        return response.body
      } }
    },
  })
  function admin(op) {
    return new Promise((resolve) => {
      const asynchronous = listener({ action: 'admin', op }, {
        id: 'test-extension', url: 'chrome-extension://test-extension/options.html', tab: { id: 1 },
      }, resolve)
      assert.equal(asynchronous, true)
    })
  }
  return { session, respond(value) { response = value }, ping: () => admin('ping'), snapshot: () => admin('snapshot') }
}

const sw = worker()
assert.equal((await sw.ping()).ping.alive, true)
for (const [response, error, state] of [
  [{ error: new TypeError('Failed to fetch') }, 'engine_unreachable', 'down'],
  [{ error: Object.assign(new Error('Aborted'), { name: 'AbortError' }) }, 'engine_timeout', 'down'],
  [{ status: 403, body: { ok: false, error: 'invalid_token' } }, 'invalid_token', 'invalid_token'],
  [{ status: 403, body: { ok: false, error: 'ext_bridge_disabled' } }, 'ext_bridge_disabled', 'disabled'],
  [{ status: 200, body: { ok: false, enabled: false } }, 'ext_bridge_disabled', 'disabled'],
  [{ status: 403, invalidJson: true }, 'http_403', 'passthrough'],
  [{ status: 403, body: { cloudflare_error: true, error_code: 1010 } }, 'http_403_cloudflare_1010', 'passthrough'],
  [{ status: 404, invalidJson: true }, 'http_404', 'passthrough'],
  [{ status: 200, invalidJson: true }, 'invalid_ping_response', 'passthrough'],
]) {
  sw.respond(response)
  const { ping } = await sw.ping()
  assert.equal(ping.alive, false)
  assert.equal(ping.error, error)
  assert.equal(sw.session.values['silencer:status'].engine, state)
  const snapshot = await sw.snapshot()
  assert.equal(snapshot.cache.alive, false)
  assert.equal(snapshot.cache.error, error)
  assert.equal(snapshot.cache.blockWhenDown, true)
}
sw.respond({ status: 200, body: { ok: true, ext_protocol: 3, version: '1.0.0', block_when_down: true } })
const { ping: recovered } = await sw.ping()
assert.equal(recovered.alive, true)
assert.equal(recovered.error, '')
assert.equal(sw.session.values['silencer:status'].engine, 'ok')
for (const protocol of [2, null]) {
  sw.respond({ status: 200, body: { ok: true, ext_protocol: protocol } })
  const { ping } = await sw.ping()
  assert.equal(ping.alive, true)
  assert.equal(ping.error, 'protocol_mismatch')
  assert.equal(sw.session.values['silencer:status'].engine, 'error')
  assert.equal((await sw.snapshot()).protoMismatch, true)
}

function settings({ granted = true, permissionError = false, runtimeError = false, reply, saveError = false } = {}) {
  const elements = new Map()
  const element = (id) => {
    if (!elements.has(id)) elements.set(id, { value: '', textContent: '', className: '' })
    return elements.get(id)
  }
  element('panelUrl').value = 'https://gateway.example'
  element('token').value = 'test-extension-token'
  let requests = 0
  const local = storage()
  if (saveError) local.set = async () => { throw new Error('Storage unavailable') }
  const runtime = {
    sendMessage(msg, callback) {
      requests++
      runtime.lastError = runtimeError ? { message: 'The message port closed' } : null
      callback(reply)
      runtime.lastError = null
    },
  }
  const context = load(options, {
    chrome: {
      storage: { local }, runtime,
      permissions: { async request() {
        if (permissionError) throw new Error('Permission request failed')
        return granted
      } },
    },
    document: { getElementById: element, addEventListener() {} },
  })
  return { context, element, local, requests: () => requests }
}

for (const [config, expected] of [
  [{ granted: false }, /未授予网关地址的访问权限/],
  [{ permissionError: true }, /无法申请网关地址访问权限/],
  [{ runtimeError: true }, /扩展后台未响应/],
  [{}, /扩展后台未响应/],
  [{ reply: { ok: false, error: 'background_unavailable' } }, /扩展后台未响应/],
  [{ reply: { ok: true, ping: { alive: false, error: 'invalid_token' } } }, /扩展令牌无效/],
  [{ reply: { ok: true, ping: { alive: false, error: 'http_403_cloudflare_1010' } } }, /Cloudflare.*403.*1010/],
  [{ saveError: true }, /保存失败/],
  [{ reply: { ok: true, ping: { alive: true, version: '1.0.0', protocol: 3 } } }, /连接成功.*v1\.0\.0/],
  [{ reply: { ok: true, ping: { alive: true, protocol: 2 } } }, /协议不兼容.*扩展 v3.*网关 v2/],
  [{ reply: { ok: true, ping: { alive: true } } }, /协议不兼容/],
]) {
  const page = settings(config)
  await vm.runInContext('testConnection()', page.context)
  assert.match(page.element('testMsg').textContent, expected)
  assert.doesNotMatch(page.element('testMsg').textContent, /unreachable \/ invalid_token/)
  if (config.granted === false || config.permissionError || config.saveError) {
    assert.equal(page.requests(), 0)
    assert.equal(page.local.values.token, undefined)
  }
}
const english = settings({ reply: { ok: true, ping: { alive: false, error: 'http_403_cloudflare_1010' } } })
await vm.runInContext("lang = 'en'; testConnection()", english.context)
assert.match(english.element('testMsg').textContent, /Cloudflare.*403.*1010/)
assert.doesNotMatch(vm.runInContext('I18N.zh.presetHint + I18N.en.presetHint', english.context), /⚠|不脱敏|unverified/)
console.log('test-extension-connection: OK')
