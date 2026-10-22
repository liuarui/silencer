#!/usr/bin/env node
// MV3 lifecycle regressions. Mock browser APIs; no account/network access.
import assert from 'node:assert/strict'
import fs from 'node:fs'
import vm from 'node:vm'
const read = (name) => fs.readFileSync(new URL(`../extension/${name}`, import.meta.url), 'utf8')
const tick = () => new Promise((resolve) => setImmediate(resolve))
const dynamicHost = 'custom.example.org'
const event = () => {
  const listeners = []
  return { addListener(fn) { listeners.push(fn) }, fire(...args) { for (const fn of listeners) fn(...args) } }
}
function storage(initial = {}) {
  const values = { ...initial }
  return {
    values,
    async get(keys) { return keys == null ? { ...values } : Object.fromEntries((Array.isArray(keys) ? keys : [keys]).map((k) => [k, values[k]])) },
    async set(patch) { Object.assign(values, patch) },
    async remove(keys) { for (const key of keys) delete values[key] },
  }
}
function worker(initial, { blocked = false, denied = false } = {}) {
  const local = storage(initial), session = storage(), changed = event(), added = event(), removed = event()
  const registry = new Map(), injections = []
  let listener, release, reads = 0, active = 0, maxActive = 0
  const gate = new Promise((resolve) => { release = resolve })
  const chrome = {
    storage: { local, session, onChanged: changed },
    runtime: { id: 'test-extension', onMessage: { addListener(fn) { listener = fn } }, onInstalled: event(), onStartup: event() },
    permissions: { onAdded: added, onRemoved: removed },
    action: { async setBadgeText() {}, async setBadgeBackgroundColor() {} },
    tabs: { async query() { return [{ id: 1, url: `https://${dynamicHost}/app` }] } },
    scripting: {
      async getRegisteredContentScripts() { if (++reads === 1 && blocked) await gate; return [...registry.values()] },
      async unregisterContentScripts({ ids }) { for (const id of ids) registry.delete(id) },
      async registerContentScripts(scripts) {
        active++; maxActive = Math.max(maxActive, active)
        try {
          await tick()
          if (denied) throw new Error('Missing host permission')
          for (const script of scripts) { assert.equal(registry.has(script.id), false, 'duplicate registration'); registry.set(script.id, script) }
        } finally { active-- }
      },
      async executeScript(options) { injections.push(options) },
    },
  }
  const context = vm.createContext({ URL, AbortController, setTimeout, clearTimeout, chrome })
  context.self = context
  vm.runInContext(read('shared.js'), context)
  vm.runInContext(read('background.js'), context)
  const resync = () => new Promise((resolve) => listener({ action: 'admin', op: 'resync' }, { id: 'test-extension', url: 'chrome-extension://test-extension/options.html' }, resolve))
  return { local, session, registry, injections, changed, added, removed, release, resync, grant() { denied = false }, reads: () => reads, concurrency: () => maxActive }
}
const cold = worker({ enabledSites: [dynamicHost] })
assert.equal((await cold.resync()).ok, true)
assert.deepEqual([...cold.registry.keys()].sort(), [`silencer-isolated-${dynamicHost}`, `silencer-main-${dynamicHost}`])
assert.ok(cold.injections.some((x) => x.world === 'MAIN' && x.files.join() === 'webchat-protocols.js,bridge-main.js'))
assert.ok(cold.injections.some((x) => x.world === 'ISOLATED'))
assert.equal(cold.concurrency(), 1)
const race = worker({ enabledSites: [] }, { blocked: true })
await tick()
race.local.values.enabledSites = [dynamicHost]
race.changed.fire({ enabledSites: { oldValue: [], newValue: [dynamicHost] } }, 'local')
let resolved = false
const result = race.resync().then((value) => { resolved = true; return value })
await tick()
assert.equal(resolved, false, 'resync must await the pending current configuration')
race.release()
assert.equal((await result).ok, true)
assert.ok(race.registry.has(`silencer-main-${dynamicHost}`))
assert.ok(race.reads() >= 2, 'configuration changed after first snapshot requires another round')
assert.equal(race.concurrency(), 1)
const permission = worker({ enabledSites: [dynamicHost] }, { denied: true })
assert.equal((await permission.resync()).failed.length, 2)
permission.grant()
permission.added.fire({ origins: [`*://*.${dynamicHost}/*`] })
await permission.resync()
assert.ok(permission.registry.has(`silencer-main-${dynamicHost}`))
assert.equal(permission.session.values['silencer:regFailures'].length, 0)
const before = permission.reads()
permission.changed.fire({ irrelevant: {} }, 'local')
permission.changed.fire({ enabledSites: {} }, 'session')
await tick()
assert.equal(permission.reads(), before)
console.log('test-extension-registration: cold wake, config resync race, permission recovery and serial registration OK')
