import assert from 'node:assert/strict'
import { test } from 'node:test'
import { displayOriginal, displayPreview, redactOriginals, projectPrivateBodies, HIDDEN_TEXT } from '../src/lib/privacy.ts'

test('originals including custom words resembling credential previews are masked', () => {
  for (const value of ['a', 'ab', '13812345678', '10.20.30.40', '<内部系统>', 'secret…value']) {
    assert.notEqual(displayOriginal(value, false), value)
    assert.equal(displayOriginal(value, true), value)
    assert.notEqual(displayPreview(value, false), value)
  }
  assert.equal(displayPreview('{{EMAIL_abc123}}', false), '{{EMAIL_abc123}}')
})

test('body display redacts all occurrences, overlaps, escaped JSON/tool arguments and SSE', () => {
  const originals = ['alice@example.test', 'alice', 'secret"line\nvalue', '内部系统']
  const items = originals.map((original) => ({ original }))
  const nested = JSON.stringify({ arguments: JSON.stringify({ email: originals[0], secret: originals[2], name: originals[3] }) })
  const body = `data: ${nested}\n\ndata: ${JSON.stringify({ email: originals[0] })}\n\n${originals.join(' / ')} / \\u5185\\u90e8\\u7cfb\\u7edf`
  const masked = redactOriginals(body, items)
  for (const value of originals) {
    assert(!masked.includes(value), value)
    assert(!masked.includes(JSON.stringify(value).slice(1, -1)), value)
  }
  assert(!masked.includes('\\u5185\\u90e8\\u7cfb\\u7edf'))
  assert(!masked.includes('@example.test'), 'longest match must win')
})

test('every body/copy projection and mapping is masked without changing fetched data', () => {
  const original = 'alice@example.test'
  const keys = ['raw', 'masked', 'upstream_response', 'restored_response', 'dialog_req', 'dialog', 'dialog_resp', 'req_preview', 'resp_preview']
  const raw = { items: [{ original, preview: original, label: 'EMAIL' }], ...Object.fromEntries(keys.map((key) => [key, `hello ${original}`])) }
  const projected = projectPrivateBodies(raw, false)
  for (const key of keys) assert(!projected[key].includes(original), key)
  assert(!projected.items[0].original.includes(original))
  assert(!projected.items[0].preview.includes(original))
  assert.equal(raw.raw, `hello ${original}`)
  assert.strictEqual(projectPrivateBodies(raw, true), raw)
})

test('mixed Unicode and nested JSON escapes are interpreted before masking', () => {
  const original = 'alice@example.test'
  const mixed = '{"email":"alice\\u0040example.test"}'
  const items = [{ original }]
  for (const body of [mixed, `data: ${mixed}\n\n`, JSON.stringify({ arguments: mixed }), JSON.stringify({ arguments: JSON.stringify({ arguments: mixed }) })]) {
    const masked = redactOriginals(body, items)
    assert(!masked.includes('example.test'))
    assert(!masked.includes('\\u0040'))
  }
  const name = '{"name":"内\\u90E8系\\u7eDF"}'
  assert(!redactOriginals(name, [{ original: '内部系统' }]).includes('\\u90E8'))
})

test('legacy or incomplete mappings cannot expose raw/restored body as a fallback', () => {
  for (const items of [[], [{ preview: '<credential>' }], [{ original: 'known' }, { preview: '<missing>' }]]) {
    const safe = projectPrivateBodies({ raw: 'unknown secret', restored_response: 'unknown secret', dialog: 'unknown secret', masked: '{{SECRET_abc}}', items }, false)
    assert.equal(safe.raw, HIDDEN_TEXT)
    assert.equal(safe.restored_response, HIDDEN_TEXT)
    assert.equal(safe.dialog, HIDDEN_TEXT)
    assert.equal(safe.masked, '{{SECRET_abc}}')
  }
})
