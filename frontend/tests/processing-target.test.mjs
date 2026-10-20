import assert from 'node:assert/strict'
import test from 'node:test'
import { processingScope, processingURL, setProcessingTarget } from '../src/lib/processing-target.ts'

test('local host operations never move with the processing target', () => {
  setProcessingTarget(true, 'remote-one')
  for (const url of ['/api/auth/status', '/api/local/capture/config', '/api/capabilities', '/api/config/backups', '/api/config/restore', '/api/status', '/api/health', '/api/proxy/start']) assert.equal(processingURL(url), url)
  assert.equal(processingURL('/api/config/patch', 'POST', JSON.stringify({key:'target_domains'})), '/api/config/patch')
})
test('submitted processor requests stay pinned after switching', () => {
  setProcessingTarget(true, 'remote-one')
  const submitted = processingURL('/api/logs?latest=1')
  const sequenceTarget = processingScope()
  setProcessingTarget(true, 'remote-two')
  assert.equal(submitted, '/api/processing/remote-one/logs?latest=1')
  assert.equal(processingURL('/api/config/patch', 'POST', JSON.stringify({key:'sensitive'}), sequenceTarget), '/api/processing/remote-one/config/patch')
  assert.equal(processingURL('/api/logs'), '/api/processing/remote-two/logs')
  assert.equal(processingURL('/api/upstream/test', 'POST', '{}'), '/api/processing/remote-two/upstream/test')
})
test('server deployment keeps the established API paths', () => {
  setProcessingTarget(false)
  assert.equal(processingURL('/api/config'), '/api/config')
  assert.equal(processingURL('/api/logs'), '/api/logs')
})
