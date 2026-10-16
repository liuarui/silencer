import assert from 'node:assert/strict'
import { test } from 'node:test'
import { mergeMaskRestore, getDisplayEventType, auditEventMatches, getLogPage, FILTER_ALL } from '../src/lib/log-events.ts'

function event(type, props = {}) {
  return { id: 1, seq: 1, ts: 1, sid: 'request-1', type, host: 'example.test', method: 'POST', path: '/chat', count: 0, restored: 0, items: [], ...props }
}

test('zero-hit request has the same PASS label, filter and noise behavior', () => {
  const events = [event('MASK'), event('RESTORE', { seq: 2, http_status: 200 })]
  const all = mergeMaskRestore(events)
  assert.equal(all.length, 1)
  assert.equal(getDisplayEventType(all[0]), 'PASS')
  assert.equal(mergeMaskRestore(events, { filterType: 'PASS' }).length, 1)
  assert.equal(mergeMaskRestore(events, { filterType: 'PASS_COMBINED' }).length, 1)
  assert.equal(mergeMaskRestore(events, { hideNoise: true }).length, 0)
})

test('phase filters preserve the complete request summary and stable RESTORE detail ID', () => {
  const events = [event('MASK', { detail_id: 'mask', count: 3 }), event('RESTORE', { detail_id: 'restore', restored: 2, seq: 2, status: 'restored', total_ms: 120 })]
  for (const filterType of ['MASK', 'RESTORE']) {
    const rows = mergeMaskRestore(events, { filterType })
    assert.equal(rows.length, 1)
    assert.equal(rows[0].count, 3)
    assert.equal(rows[0].restored, 2)
    assert.equal(rows[0].total_ms, 120)
    assert.equal(rows[0]._detailId, 'restore')
    assert.equal(getDisplayEventType(rows[0]), filterType)
  }
})

test('failed, canceled and DNS requests cannot be labeled PASS or hidden as noise', () => {
  for (const [props, type] of [[{ http_status: 503 }, 'ERR'], [{ error: 'canceled', http_status: 499 }, 'CANCEL'], [{ error: 'dns_error', http_status: 502 }, 'DNS_ERROR']]) {
    const events = [event('MASK'), event('RESTORE', { seq: 2, failure_stage: 'upstream_read', ...props })]
    const rows = mergeMaskRestore(events, { hideNoise: true })
    assert.equal(rows.length, 1)
    assert.equal(getDisplayEventType(rows[0]), type)
    assert.equal(rows[0].failure_stage, 'upstream_read')
    assert.equal(mergeMaskRestore(events, { filterType: type }).length, 1)
    assert.equal(mergeMaskRestore(events, { filterType: 'PASS_COMBINED' }).length, 0)
  }
})

test('phase filters retain masking evidence even when the completed outcome is an error', () => {
  const events = [event('MASK', { count: 3 }), event('RESTORE', { seq: 2, http_status: 503 })]
  for (const filterType of ['MASK', 'RESTORE', 'ERR']) {
    const rows = mergeMaskRestore(events, { filterType, hideNoise: true })
    assert.equal(rows.length, 1)
    assert.equal(rows[0].count, 3)
    assert.equal(getDisplayEventType(rows[0]), 'ERR')
  }
})

test('same-timestamp requests keep their own rows and details; extra phases are not overwritten', () => {
  const events = [event('MASK', { sid: 'a', detail_id: 'a-mask' }), event('RESTORE', { sid: 'a', detail_id: 'a-restore' }), event('MASK', { sid: 'b', detail_id: 'b-mask' }), event('RESTORE', { sid: 'b', detail_id: 'b-restore' }), event('RESTORE', { sid: 'a', detail_id: 'a-extra' })]
  const rows = mergeMaskRestore(events)
  assert.equal(rows.length, 3)
  assert.deepEqual(rows.filter((row) => row._detailId).map((row) => row._detailId).sort(), ['a-restore', 'b-restore'])
  assert(rows.some((row) => row.detail_id === 'a-extra'))
})

test('audit rows obey search, sensitive and ingress filters', () => {
  const audit = { seq: 1, ts: 1, signal_type: 'error_leak', severity: 'LOW', host: 'example.test', evidence: 'response contains a token' }
  const opts = { filterType: FILTER_ALL, sensitive: false, ingress: '__all__', q: '' }
  assert.equal(auditEventMatches(audit, opts), true)
  assert.equal(auditEventMatches(audit, { ...opts, q: 'TOKEN' }), true)
  assert.equal(auditEventMatches(audit, { ...opts, q: 'not-present' }), false)
  assert.equal(auditEventMatches(audit, { ...opts, sensitive: true }), false)
  assert.equal(auditEventMatches(audit, { ...opts, ingress: 'proxy' }), false)
  assert.equal(auditEventMatches(audit, { ...opts, filterType: 'PASS_COMBINED' }), false)
})

test('shrinking a result window cannot leave pagination on an empty higher page', () => {
  assert.equal(getLogPage(5, 10, 50), 1)
  assert.equal(getLogPage(5, 0, 50), 1)
  assert.equal(getLogPage(2, 100, 50), 2)
})

test('allowlisted 404 stays recorded and merges as noise; real failures take priority', () => {
  const allowed = { status: '404', http_status: 404, ingress: 'proxy', noise_whitelisted: true, display_type: 'SKIP' }
  const rows = [event('MASK', { count: 2, ...allowed }), event('RESTORE', { seq: 2, ...allowed })]
  const merged = mergeMaskRestore(rows)
  assert.equal(merged.length, 1)
  assert.equal(merged[0].count, 2)
  assert.equal(merged[0].http_status, 404)
  assert.equal(getDisplayEventType(merged[0]), 'SKIP')
  assert.equal(mergeMaskRestore(rows, { hideNoise: true }).length, 0)
  assert.equal(mergeMaskRestore(rows, { filterType: 'PASS_COMBINED' }).length, 1)
  assert.equal(mergeMaskRestore(rows, { filterType: 'ERR' }).length, 0)
  for (const props of [{ unresolved: 1 }, { http_status: 500 }, { http_status: 403 }, { status: '500' }, { ingress: 'ext' }, { noise_whitelisted: false }, { failure_stage: 'downstream_write' }, { error: 'restore_error' }]) {
    assert.equal(getDisplayEventType(event('RESTORE', { ...allowed, ...props })), 'ERR')
  }
  assert.equal(getDisplayEventType(event('RESTORE', { ...allowed, error: 'canceled' })), 'CANCEL')
  assert.equal(getDisplayEventType(event('RESTORE', { ...allowed, http_status: 499 })), 'CANCEL')
})
