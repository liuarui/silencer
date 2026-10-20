import assert from 'node:assert/strict'
import test from 'node:test'
import { UNINSTALL_PROGRESS_KEY, captureAttentionReasons, captureIsActive, captureSetupStepLimit, captureStatusUsable, cleanUninstallProgress, newUninstallProgress, readUninstallProgress, reconcileUninstallProgress, recordUninstallDownload, writeUninstallProgress } from '../src/lib/capture-lifecycle.ts'

function status() {
  return { state: 'running', platform: 'darwin', config: { enabled: true }, checks: Object.fromEntries(['component', 'platform', 'permissions', 'certificate', 'pipeline'].map((key) => [key, { ok: true, error: '' }])), uninstall: { capture_stopped: true, install_revision: '123' }, certificate: { sha256: 'A'.repeat(64) }, observed_requests: 0 }
}
function storage() {
  const values = new Map()
  return { getItem: (key) => values.get(key) ?? null, setItem: (key, value) => values.set(key, value), removeItem: (key) => values.delete(key) }
}

test('uninstall resume persists choices and download receipt, never credentials or a success claim', () => {
  const now = Date.now()
  const saved = { ...newUninstallProgress(status(), now), step: 2, keepCertificate: true, downloadedAt: now, token: 'do-not-persist', config: { remoteToken: 'do-not-persist' }, success: true, state: 'complete' }
  const store = storage()
  writeUninstallProgress(store, saved)
  const wire = store.getItem(UNINSTALL_PROGRESS_KEY)
  assert.equal(wire.includes('do-not-persist'), false)
  assert.equal(wire.includes('success'), false)
  assert.equal(wire.includes('complete'), false)
  const restored = readUninstallProgress(store, now)
  assert.equal(restored.step, 2)
  assert.equal(restored.keepCertificate, true)
  assert.equal(restored.downloadedAt, now)
})
test('resume rejects corrupt, expired and future records and tolerates disabled storage', () => {
  const now = Date.now()
  const progress = newUninstallProgress(status(), now)
  assert.equal(cleanUninstallProgress({ ...progress, updatedAt: now - 8 * 86400000 }, now), null)
  assert.equal(cleanUninstallProgress({ ...progress, updatedAt: now + 120000 }, now), null)
  assert.equal(readUninstallProgress({ getItem: () => '{broken' }, now), null)
  assert.doesNotThrow(() => writeUninstallProgress({ setItem() { throw Error('denied') }, removeItem() { throw Error('denied') } }, progress))
})
test('a fresh stop check is required after capture restarts', () => {
  const current = status()
  current.uninstall.capture_stopped = false
  const saved = { ...newUninstallProgress(current), step: 2, downloadedAt: Date.now() }
  const result = reconcileUninstallProgress(saved, current)
  assert.equal(result.progress.step, 0)
  assert.equal(result.progress.downloadedAt, 0)
})
test('a changed CA clears old script receipts and returns to scope confirmation', () => {
  const current = status()
  const saved = { ...newUninstallProgress(current), step: 2, downloadedAt: Date.now() }
  current.certificate.sha256 = 'B'.repeat(64)
  const result = reconcileUninstallProgress(saved, current)
  assert.equal(result.reason, 'certificate_changed')
  assert.equal(result.progress.step, 1)
  assert.equal(result.progress.downloadedAt, 0)
})
test('reinstall starts a fresh guide rather than carrying a previous uninstall choice forward', () => {
  const current = status()
  const saved = { ...newUninstallProgress(current), step: 2, keepCertificate: true }
  current.uninstall.install_revision = '456'
  const result = reconcileUninstallProgress(saved, current)
  assert.equal(result.reason, 'installation_changed')
  assert.equal(result.progress.active, false)
  assert.equal(result.progress.step, 0)
  assert.equal(result.progress.keepCertificate, false)
})
test('cached checks cannot advance setup before mount verification or after disconnect', () => {
  const cached = status()
  assert.equal(captureStatusUsable(cached, false, false), false)
  assert.equal(captureStatusUsable(cached, true, true), false)
  assert.equal(captureStatusUsable(cached, true, false), true)
  cached.checks.certificate.ok = false
  assert.equal(captureSetupStepLimit(cached), 3)
  assert.equal(captureIsActive(cached), false)
})
test('a running heartbeat alone cannot override invalid pipeline checks', () => {
  const current = status()
  current.checks.pipeline.ok = false
  assert.equal(captureIsActive(current), false)
  current.checks.pipeline.ok = true
  assert.equal(captureIsActive(current), true)
})
test('attention explains a pending pipeline without blaming a trusted CA or missing client traffic', () => {
  const current = status()
  current.checks.pipeline = { ok: false, error: 'pipeline_check_required' }
  current.runtime = { process_running: true, heartbeat_ready: true, targets_applied: true }
  assert.deepEqual(captureAttentionReasons(current), [{ key: 'pipeline', code: 'pipeline_check_required' }])
  current.checks.pipeline = { ok: true, error: '' }
  assert.deepEqual(captureAttentionReasons(current), [])
  current.runtime.targets_applied = false
  assert.deepEqual(captureAttentionReasons(current), [{ key: 'targets', code: 'capture_targets_not_applied' }])
})
test('a delayed script response cannot restore a receipt after reinstall, CA change or resumed capture', () => {
  const requested = { ...newUninstallProgress(status()), step: 2 }
  const changes = [
    { installRevision: '456' }, { certificateSha: 'B'.repeat(64) },
    { active: false }, { step: 0 }, { keepCertificate: true },
  ]
  for (const change of changes) {
    const actual = { ...requested, ...change }
    assert.equal(recordUninstallDownload(actual, requested, true).downloadedAt, 0)
  }
  assert.equal(recordUninstallDownload(requested, requested, false, 123).downloadedAt, 0)
  assert.equal(recordUninstallDownload(requested, requested, true, 123).downloadedAt, 123)
})
