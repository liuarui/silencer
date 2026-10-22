#!/usr/bin/env node
// Synthetic, sanitized protocol fixtures; no network/accounts/live-site claim.
import assert from 'node:assert/strict'
import fs from 'node:fs'
import vm from 'node:vm'

const context = vm.createContext({ URL, TextEncoder })
vm.runInContext(fs.readFileSync(new URL('../webchat-protocols.js', import.meta.url), 'utf8'), context)
const codec = context.SILENCER_WEBCHAT
const email = 'support-test@example.invalid'
const marker = '{{EMAIL_0123456789abcdef}}'
const original = '中文😀 "quoted" \\ slash\n' + email
const mask = async (text) => ({ ok: true, sid: 'ext:fixture', masked_text: text.replaceAll(email, marker) })
const restore = async (text) => text.replaceAll(marker, original)
const target = {
  chatgpt: 'https://chatgpt.com/backend-api/f/conversation',
  gemini: 'https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate',
  deepseek: 'https://chat.deepseek.com/api/v0/chat/completion',
  doubao: 'https://www.doubao.com/samantha/chat/completion',
  kimi: 'https://www.kimi.com/api/chat/current/completion',
}
const provider = (id) => codec.match(target[id], new URL(target[id]).origin)
for (const id of Object.keys(target)) assert.equal(provider(id)?.id, id)
assert.equal(codec.match(target.chatgpt, 'https://evil.example'), null)
assert.equal(codec.match('https://chatgpt.com.evil.example/backend-api/conversation', 'https://chatgpt.com'), null)

for (const id of ['chatgpt', 'deepseek', 'doubao', 'kimi']) {
  const document = {
    model: email, parent_id: email, metadata: { text: email },
    messages: [{ id: email, content: { parts: [email] } }],
  }
  const result = await codec.maskRequest(provider(id), JSON.stringify(document), mask)
  const sent = JSON.parse(result.masked_text)
  assert.equal(sent.messages[0].content.parts[0], marker)
  assert.equal(sent.messages[0].id, email)
  assert.equal(sent.model, email)
  assert.equal(sent.metadata.text, email)
  assert.equal(result.sid, 'ext:fixture')
  const raw = ' { "prompt" : "hello, no secrets" } '
  assert.equal((await codec.maskRequest(provider(id), raw, mask)).masked_text, raw)
}
const doubao = JSON.stringify({ messages: [{ content: JSON.stringify({ text: email, format: 'text' }) }], conversation_id: email })
const doubaoResult = JSON.parse((await codec.maskRequest(provider('doubao'), doubao, mask)).masked_text)
assert.equal(JSON.parse(doubaoResult.messages[0].content).text, marker)
assert.equal(doubaoResult.conversation_id, email)
const params = 'at=csrf%2Bkeep+exact&f.req=' + encodeURIComponent(JSON.stringify([null, JSON.stringify([[email], null, ['conversation-id', email]])])) + '&extra=one%20two'
const geminiRequest = await codec.maskRequest(provider('gemini'), params, mask)
assert.ok(geminiRequest.masked_text.startsWith('at=csrf%2Bkeep+exact&f.req='))
assert.ok(geminiRequest.masked_text.endsWith('&extra=one%20two'))
const nested = JSON.parse(JSON.parse(new URLSearchParams(geminiRequest.masked_text).get('f.req'))[1])
assert.equal(nested[0][0], marker)
assert.equal(nested[2][1], email)
const encoded = '{"prompt":"support-test@exampl\\u0065.invalid","id":"keep"}'
assert.equal(JSON.parse((await codec.maskRequest(provider('deepseek'), encoded, mask)).masked_text).prompt, marker)
for (const [id, raw] of [['gemini', 'f.req=invalid&at=private'], ['kimi', '\0protobuf'], ['chatgpt', '{"unexpected_field":"text"}']]) {
  await assert.rejects(codec.maskRequest(provider(id), raw, mask), (e) => e.silencerBlocking === true)
}
await assert.rejects(codec.maskRequest(provider('deepseek'), '{"prompt":"x"}', async () => ({ ok: true, masked_text: '[]' })), /invalid_mask_response/)
const prepareProvider = codec.match('https://chatgpt.com/backend-api/f/conversation/prepare', 'https://chatgpt.com')
const prepared = await codec.maskRequest(prepareProvider, JSON.stringify({ partial_query: { content: { parts: [email] } }, auth: { token: email } }), async (text) => {
  assert.equal(text, JSON.stringify([email]))
  return mask(text)
})
assert.equal(JSON.parse(prepared.masked_text).partial_query.content.parts[0], marker)
assert.equal(JSON.parse(prepared.masked_text).auth.token, email)
const prepareEmpty = '{"action":"prepare-without-prompt"}'
assert.equal((await codec.maskRequest(prepareProvider, prepareEmpty, async () => assert.fail('empty prepare must not call masking'))).masked_text, prepareEmpty)
const prepareResponse = '{"conduit_token":"synthetic-not-a-real-credential"}'
const prepareCodec = codec.createRestorer(prepareProvider, 'application/json', async () => assert.fail('prepare credentials must not enter restoration'))
assert.equal(await prepareCodec.push(prepareResponse, true), prepareResponse)

const sse = (value) => 'data: ' + JSON.stringify(value) + '\n\n'
const events = (text) => text.split(/\r?\n\r?\n/).map((s) => s.split(/\r?\n/).find((line) => line.startsWith('data: '))).filter((s) => s && s !== 'data: [DONE]').map((s) => JSON.parse(s.slice(6)))
async function decode(id, ct, input, cut = 1) {
  const calls = []
  const stream = codec.createRestorer(typeof id === 'string' ? provider(id) : id, ct, async (value) => { calls.push(value); return restore(value) })
  let output = ''
  for (let i = 0; i < input.length; i += cut) output += await stream.push(input.slice(i, i + cut))
  output += await stream.push('', true)
  return { output, calls }
}

const cg = await decode('chatgpt', 'text/event-stream', sse({ message: { id: 'same', content: { parts: [marker] } } }) + 'data: [DONE]\n\n')
assert.equal(events(cg.output)[0].message.content.parts[0], original)
assert.ok(cg.output.endsWith('data: [DONE]\n\n'))
const patch = await decode('deepseek', 'text/event-stream',
  'event: message\nid: keep\n' + sse({ p: 'response/content', o: 'APPEND', v: marker.slice(0, 12) }) +
  sse({ p: 'response/thinking_content', o: 'APPEND', v: 'Reasoning: ' }) +
  sse({ p: 'response/content', o: 'APPEND', v: marker.slice(12) }), 7)
const patchValues = events(patch.output)
assert.equal(patchValues.filter((x) => x.p === 'response/content').map((x) => x.v).join(''), original)
assert.equal(patchValues[1].v, 'Reasoning: ')
assert.ok(patch.output.includes('id: keep'))
const implicit = await decode('chatgpt', 'text/event-stream', sse({ p: '/message/content/parts/0', o: 'append', v: marker.slice(0, 10) }) + sse({ o: 'append', v: marker.slice(10) }), 3)
assert.equal(events(implicit.output).map((x) => x.v).join(''), original)
const messageSnapshot = (id, parts) => ({ message: { id, content: { parts } } })
for (const envelope of [false, true]) {
  for (const [id, initial, path] of [
    ['deepseek', { response: { message_id: 1, fragments: [{ id: 2, type: 'RESPONSE', content: '{{' }] } }, 'response/fragments/0/content'],
    ['chatgpt', messageSnapshot('assistant-a', ['{{']), '/message/content/parts/0'],
  ]) {
    const combined = await decode(id, 'text/event-stream', sse(envelope ? { v: initial } : initial) +
      sse({ p: path, o: 'APPEND', v: marker.slice(2, 10) }) + sse({ v: marker.slice(10) }), 3)
    assert.ok(combined.calls.includes(marker), 'snapshot and delta must reach native as one complete marker')
    assert.equal(events(combined.output).at(-1).v, original)
  }
}
const fragments = await decode('deepseek', 'text/event-stream',
  sse({ v: { response: { message_id: 1, fragments: [{ id: 2, type: 'THINK', content: '{{' }] } } }) +
  sse({ p: 'response/fragments', o: 'APPEND', v: [{ id: 3, type: 'RESPONSE', content: '{{' }] }) +
  sse({ p: 'response/fragments/0/content', o: 'APPEND', v: marker.slice(2) }) +
  sse({ p: 'response/fragments/1/content', o: 'APPEND', v: marker.slice(2) }), 2)
assert.deepEqual(events(fragments.output).slice(-2).map((x) => x.v), [original, original])
// Official DeepSeek Array.at and recursive relative BATCH semantics.
const negativeIndex = await decode('deepseek', 'text/event-stream',
  sse({ v: { response: { fragments: [{ id: 1, content: '{{' }] } } }) +
  sse({ p: 'response/fragments/-1/content', o: 'APPEND', v: marker.slice(2) }), 2)
assert.equal(events(negativeIndex.output).at(-1).v, original)
const batch = await decode('deepseek', 'text/event-stream', sse({ p: 'response', o: 'BATCH', v: [
  { p: 'fragments', o: 'SET', v: [{ id: 1, content: '{{' }] },
  { p: 'fragments/-1/content', o: 'APPEND', v: marker.slice(2, 10) },
  { v: marker.slice(10) },
] }), 2)
assert.equal(events(batch.output)[0].v.at(-1).v, original)
const nestedBatch = await decode('deepseek', 'text/event-stream', sse({ o: 'BATCH', v: [
  { p: 'response', o: 'BATCH', v: [
    { p: 'fragments', o: 'SET', v: [{ content: '{{' }] },
    { p: 'fragments/-1/content', o: 'APPEND', v: marker.slice(2) },
  ] },
  // A new BATCH starts fresh p/o state; it must not inherit the last child.
  { p: 'other', o: 'BATCH', v: [{ p: 'text', v: 'safe' }] },
] }), 3)
assert.equal(events(nestedBatch.output)[0].v[0].v[1].v, original)
assert.equal(events(nestedBatch.output)[0].v[1].v[0].v, 'safe')
const setInheritance = await decode('deepseek', 'text/event-stream',
  sse({ p: 'response/content', o: 'SET', v: '{{' }) + sse({ v: 'replacement' }), 2)
assert.equal(events(setInheritance.output).at(-1).v, 'replacement')
const defaultSet = await decode('deepseek', 'text/event-stream',
  sse({ p: 'response/content', v: '{{' }) + sse({ v: 'replacement' }), 2)
assert.equal(events(defaultSet.output).at(-1).v, 'replacement')
const untouchedChatgptFrame = 'id: 7\r\nevent: message\r\ndata:  { "message" : { "id" : "safe-id", "status" : "finished_successfully", "end_turn" : true } }\r\n\r\n'
const untouchedChatgpt = codec.createRestorer(provider('chatgpt'), 'text/event-stream', restore)
assert.equal(await untouchedChatgpt.push(untouchedChatgptFrame, true), untouchedChatgptFrame, 'unchanged one-line SSE frames retain their original bytes')
assert.equal(untouchedChatgpt.diagnostics().sse.preservedFrames, 1)
assert.equal(untouchedChatgpt.diagnostics().sse.rewrittenFrames, 0)
const liveShapedChatgpt = codec.createRestorer(provider('chatgpt'), 'text/event-stream', restore)
const liveTextPatch = { o: 'PATCH', v: [
  { p: '/message/content/parts/0', o: 'APPEND', v: marker },
  { p: '/message/metadata/recipient', o: 'REPLACE', v: 'all' },
  { p: '/message/metadata/channel', o: 'REPLACE', v: 'final' },
  { p: '/message/metadata/content_type', o: 'REPLACE', v: 'text' },
  { p: '/message/metadata/weight', o: 'INCREMENT', v: 1 },
] }
const liveStatusPatch = { o: 'PATCH', v: [
  { p: '/message/status', o: 'REPLACE', v: 'finished_successfully' },
  { p: '/message/end_turn', o: 'REPLACE', v: true },
  { p: '/message/metadata', o: 'APPEND', v: { finish_details: { type: 'stop' } } },
] }
const liveStatusWire = 'event: message\r\ndata: ' + JSON.stringify(liveStatusPatch) + '\r\n\r\n'
const liveShapedOutput = await liveShapedChatgpt.push(
  'event: message\r\ndata: ' + JSON.stringify(liveTextPatch) + '\r\n\r\n' +
  liveStatusWire +
  'data: [DONE]\r\n\r\n', true)
const liveShapedEvents = events(liveShapedOutput)
assert.equal(liveShapedEvents.length, 2)
assert.equal(liveShapedEvents[0].v[0].v, original, 'ChatGPT text PATCH still restores masked text')
assert.deepEqual(liveShapedEvents[0].v[4], { p: '/message/metadata/weight', o: 'INCREMENT', v: 1 }, 'opaque numeric metadata operations pass through')
assert.equal(liveShapedEvents[1].v[0].v, 'finished_successfully')
assert.equal(liveShapedEvents[1].v[1].v, true)
assert.ok(liveShapedOutput.includes(liveStatusWire), 'unchanged end-state PATCH preserves original CRLF wire frame')
assert.ok(liveShapedOutput.startsWith('event: message\r\ndata: '), 'rewritten text frame preserves its original event and line endings')
assert.equal(liveShapedOutput.endsWith('data: [DONE]\r\n\r\n'), true, 'terminal control frame stays intact')
assert.equal(liveShapedChatgpt.isTerminal(), true)
assert.equal(liveShapedChatgpt.diagnostics().sse.rewrittenFrames, 1)
assert.equal(liveShapedChatgpt.diagnostics().sse.preservedFrames, 1)
const opaqueMetadataPatch = codec.createRestorer(provider('chatgpt'), 'text/event-stream', restore)
const opaqueMetadataFrame = sse({ o: 'PATCH', v: [{ p: '/message/metadata/weight', o: 'INCREMENT', v: 2 }] })
assert.equal(await opaqueMetadataPatch.push(opaqueMetadataFrame, true), opaqueMetadataFrame, 'unknown numeric metadata patch is preserved without opening text fields')
const patchList = await decode('chatgpt', 'text/event-stream',
  sse({ v: messageSnapshot('nested', ['{{']) }) + sse({ o: 'patch', v: [
    { p: '/message/content/parts/0', o: 'append', v: marker.slice(2, 10) },
    { p: '/message/content/parts/0', o: 'append', v: marker.slice(10) },
  ] }), 2)
assert.equal(events(patchList.output).at(-1).v.at(-1).v, original)
const chatgptRemove = await decode('chatgpt', 'text/event-stream',
  sse({ v: messageSnapshot('remove-array-item', ['{{', 'safe-to-remove', '{{']) }) + sse({ p: '', o: 'patch', v: [
    { p: '/message/content/parts/0', o: 'remove' },
    { p: '/message/content/parts/1', o: 'append', v: marker.slice(2) },
  ] }), 2)
const removeChildren = events(chatgptRemove.output).at(-1).v
assert.equal('v' in removeChildren[0], false, 'official ChatGPT REMOVE children omit v')
assert.equal(removeChildren[1].v, original, 'array removal reindexes the remaining partial text lane')
assert.ok(chatgptRemove.calls.includes(marker), 'reindexed placeholder is restored as one complete value')
const diagnosticStream = codec.createRestorer(provider('chatgpt'), 'text/event-stream', restore)
await diagnosticStream.push(sse({ v: messageSnapshot('synthetic-private-message-id', [marker]) }))
await diagnosticStream.push('data: [DONE]\n\n')
const streamDiagnostic = diagnosticStream.diagnostics()
assert.equal(streamDiagnostic.provider, 'chatgpt')
assert.equal(streamDiagnostic.records, 1)
assert.equal(streamDiagnostic.pending.length, 0)
assert.equal(diagnosticStream.isTerminal(), true)
assert.ok(streamDiagnostic.patchCount > 0)
assert.equal(streamDiagnostic.sse.frames, 2)
assert.equal(streamDiagnostic.sse.doneFrames, 1)
assert.equal(streamDiagnostic.sse.shapeChanges, 0)
assert.equal(streamDiagnostic.sse.recent[0].shapeChanged, false)
assert.equal(streamDiagnostic.sse.recent[0].operations.map((item) => item.op).join(','), 'ADD')
assert.equal(streamDiagnostic.sse.recent[0].tag, 'other')
assert.doesNotMatch(JSON.stringify(streamDiagnostic), /synthetic-private-message-id|EMAIL_|0123456789abcdef|support-test@example\.invalid/)
const incompleteAtDone = codec.createRestorer(provider('chatgpt'), 'text/event-stream', restore)
await incompleteAtDone.push(sse({ message: { id: 'pending-at-done', content: { parts: ['{{'] } } }))
await incompleteAtDone.push('data: [DONE]\n\n')
assert.equal(incompleteAtDone.isTerminal(), true)
await assert.rejects(incompleteAtDone.push('', true), (error) => error.silencerBlocking === true && error.message.endsWith('webchat_incomplete_placeholder'))
const relativeChatgptPatch = await decode('chatgpt', 'text/event-stream',
  sse({ v: messageSnapshot('relative-patch', ['{{']) }) + sse({ p: '/message', o: 'patch', v: [
    { p: '/content/parts/0', o: 'append', v: marker.slice(2) },
  ] }), 2)
assert.equal(events(relativeChatgptPatch.output).at(-1).v[0].v, original, 'PATCH child paths are relative to the parent target')
for (const [id, op] of [['chatgpt', 'patch'], ['deepseek', 'BATCH']]) {
  const isolatedBatch = codec.createRestorer(provider(id), 'text/event-stream', async (text) => { assert.notEqual(text, marker, 'batch child identities must not join'); return text })
  await isolatedBatch.push(sse({ o: op, v: [
    { id: 'private-owner-a', p: 'response/content', o: 'append', v: marker.slice(0, 10) },
    { id: 'private-owner-b', p: 'response/content', o: 'append', v: marker.slice(10) },
  ] }))
  await assert.rejects(isolatedBatch.push('', true), (error) => {
    assert.match(error.message, /incomplete_placeholder/)
    const diagnostic = JSON.stringify(error.silencerDiagnostic)
    assert.doesNotMatch(diagnostic, /private-owner|EMAIL_|0123456789abcdef/)
    assert.ok(diagnostic.includes('response/content'))
    return true
  })
}
const unknownNegative = codec.createRestorer(provider('deepseek'), 'text/event-stream', restore)
await assert.rejects(unknownNegative.push(sse({ p: 'response/fragments/-1/content', o: 'APPEND', v: 'safe' })), /unknown_array_index/)
// Decoder channel c is compressed separately from message identity.
const wireChannels = await decode('chatgpt', 'text/event-stream',
  sse({ c: 0, v: messageSnapshot('same-owner', ['{{']) }) +
  sse({ c: 1, p: '', o: 'add', v: messageSnapshot('same-owner', ['{{']) }) +
  sse({ c: 0, p: '/message/content/parts/0', o: 'append', v: marker.slice(2) }) +
  sse({ c: 1, v: marker.slice(2) }), 2)
assert.deepEqual(events(wireChannels.output).slice(-2).map((x) => x.v), [original, original])
const inheritedChannel = await decode('chatgpt', 'text/event-stream',
  sse({ c: 3, p: '/message/content/parts/0', o: 'append', v: marker.slice(0, 10) }) + sse({ v: marker.slice(10) }), 2)
assert.equal(events(inheritedChannel.output).at(-1).v, original)
for (const [input, reason, kind] of [
  [{ o: 'patch', v: 'private-body' }, 'batch_value_not_array', 'string'],
  [{ o: 'patch', v: [{ p: '/message/content/parts/0', v: marker }] }, 'unknown_patch_operation', 'string'],
  [{ o: 'patch', v: [{ p: '/message/content/parts/0', o: 'append' }] }, 'invalid_batch_child', 'missing'],
  [{ o: 'patch', v: [null] }, 'invalid_batch_child', 'missing'],
]) {
  const invalid = codec.createRestorer(provider('chatgpt'), 'text/event-stream', restore)
  await assert.rejects(invalid.push(sse(input)), (error) => {
    assert.match(error.message, /patch_shape_changed/)
    assert.equal(error.silencerDiagnostic.reason, reason)
    assert.equal(error.silencerDiagnostic.vKind, kind)
    assert.doesNotMatch(JSON.stringify(error.silencerDiagnostic), /private-body|secret-session-id/)
    return true
  })
}
const interleavedArrays = [
  { response: { message_id: 1, fragments: [{ id: 10, content: 'safe' }] } },
  { response: { message_id: 2, fragments: [{ id: 20, content: 'safe' }, { id: 21, content: 'safe' }] } },
]
const appendToFirst = await decode('deepseek', 'text/event-stream', interleavedArrays.map(sse).join('') +
  sse({ message_id: 1, p: 'response/fragments', o: 'APPEND', v: [{ id: 11, content: '{{' }] }) +
  sse({ message_id: 1, p: 'response/fragments/1/content', o: 'APPEND', v: marker.slice(2) }), 3)
assert.equal(events(appendToFirst.output).at(-1).v, original)
const ambiguousArray = codec.createRestorer(provider('deepseek'), 'text/event-stream', restore)
await ambiguousArray.push(interleavedArrays.map(sse).join(''))
await assert.rejects(ambiguousArray.push(sse({ p: 'response/fragments', o: 'APPEND', v: [{ content: 'new' }] })), /ambiguous_array_append/)
const multiParts = await decode('chatgpt', 'text/event-stream',
  sse(messageSnapshot('assistant-parts', ['{{', '{{'])) +
  sse({ p: '/message/content/parts/1', o: 'append', v: marker.slice(2) }) +
  sse({ p: '/message/content/parts/0', o: 'append', v: marker.slice(2) }), 4)
assert.deepEqual(events(multiParts.output).slice(-2).map((x) => x.v), [original, original])
const ambiguous = codec.createRestorer(provider('chatgpt'), 'text/event-stream', restore)
await ambiguous.push(sse(messageSnapshot('a', ['{{'])) + sse(messageSnapshot('b', ['{{'])))
await assert.rejects(ambiguous.push(sse({ p: '/message/content/parts/0', o: 'append', v: marker.slice(2) })), /ambiguous_channel/)
const identified = await decode('chatgpt', 'text/event-stream',
  sse(messageSnapshot('a', ['{{'])) + sse(messageSnapshot('b', ['{{'])) +
  sse({ id: 'a', p: '/message/content/parts/0', o: 'append', v: marker.slice(2) }) +
  sse({ id: 'b', p: '/message/content/parts/0', o: 'append', v: marker.slice(2) }), 4)
assert.deepEqual(events(identified.output).slice(-2).map((x) => x.v), [original, original])
const pointerEscapes = await decode('deepseek', 'text/event-stream',
  sse({ response: { 'nested/name': { content: '{{' } } }) +
  sse({ p: 'response/nested~1name/content', o: 'append', v: marker.slice(2) }), 2)
assert.equal(events(pointerEscapes.output).at(-1).v, original)
const ownerCollision = codec.createRestorer(provider('deepseek'), 'text/event-stream', restore)
await ownerCollision.push(sse({ response: { message_id: 1, fragments: [{ id: 2, content: '{{' }] } }))
await ownerCollision.push(sse({ message_id: 2, p: 'response/fragments/0/content', o: 'append', v: marker.slice(2) }))
await assert.rejects(ownerCollision.push('', true), /incomplete_placeholder/)
// Identical patch paths still belong to separate event, choice and message
// lanes. Each lane must inherit its own path when later deltas omit `p`.
for (const [key, left, right] of [['event', 'answer', 'thinking'], ['index', 0, 1], ['id', 'message-a', 'message-b']]) {
  const isolated = await decode('deepseek', 'text/event-stream',
    sse({ [key]: left, p: 'response/content', o: 'append', v: marker.slice(0, 10) }) +
    sse({ [key]: right, p: 'response/thinking_content', o: 'append', v: marker.slice(0, 10) }) +
    sse({ [key]: left, o: 'append', v: marker.slice(10) }) +
    sse({ [key]: right, o: 'append', v: marker.slice(10) }), 5)
  for (const identity of [left, right]) assert.equal(events(isolated.output).filter((x) => x[key] === identity).map((x) => x.v).join(''), original)
  const collision = codec.createRestorer(provider('deepseek'), 'text/event-stream', restore)
  assert.equal(await collision.push(sse({ [key]: left, p: 'response/content', v: marker.slice(0, 10) }) + sse({ [key]: right, p: 'response/content', v: marker.slice(10) })), '')
  await assert.rejects(collision.push('', true), /incomplete_placeholder/)
}
const namedEvents = await decode('kimi', 'text/event-stream',
  'event: answer\n' + sse({ text: marker.slice(0, 10) }) +
  'event: thinking\n' + sse({ text: marker.slice(0, 10) }) +
  'event: answer\n' + sse({ text: marker.slice(10) }) +
  'event: thinking\n' + sse({ text: marker.slice(10) }), 3)
assert.deepEqual(events(namedEvents.output).map((x) => x.text), ['', '', original, original])
const eventCollision = codec.createRestorer(provider('kimi'), 'text/event-stream', restore)
await eventCollision.push('event: answer\n' + sse({ text: marker.slice(0, 10) }) + 'event: thinking\n' + sse({ text: marker.slice(10) }))
await assert.rejects(eventCollision.push('', true), /incomplete_placeholder/)
const kimi = await decode('kimi', 'text/event-stream', sse({ event: 'cmpl', text: marker.slice(0, 9) }) + sse({ event: 'thinking', text: 'Separate lane' }) + sse({ event: 'cmpl', text: marker.slice(9) }), 2)
assert.equal(events(kimi.output).filter((x) => x.event === 'cmpl').map((x) => x.text).join(''), original)
assert.equal(events(kimi.output)[1].text, 'Separate lane')
const db = await decode('doubao', 'text/event-stream', sse({ event_type: 2001, event_data: JSON.stringify({ message: { id: marker, content: JSON.stringify({ text: marker }) } }) }), 11)
const message = JSON.parse(events(db.output)[0].event_data).message
assert.equal(message.id, marker)
assert.equal(JSON.parse(message.content).text, original)
const inner = [null, null, null, null, [['candidate-id', [marker]]]]
const envelope = JSON.stringify([['wrb.fr', 'rpc-id', JSON.stringify(inner), null]])
const geminiWire = ")]}'\n\n" + (envelope.length + 1) + '\n' + envelope + '\n'
const geminiResponse = await decode('gemini', 'application/json', geminiWire, 3)
const body = geminiResponse.output.slice(geminiResponse.output.indexOf('\n\n') + 2)
const newline = body.indexOf('\n'), length = Number(body.slice(0, newline)), json = body.slice(newline + 1).trim()
assert.equal(json.length + 1, length)
assert.equal(JSON.parse(JSON.parse(json)[0][2])[4][0][1][0], original)
// Length markers include the preceding LF and use JS UTF-16 units, not bytes.
for (const [text, units] of [['汉', 6], ['😀', 7]]) {
  const unitFrame = ")]}'\n\n" + units + '\n' + JSON.stringify([text]) + '\n3\n[]'
  const result = await decode('gemini', 'application/json', unitFrame, 1)
  assert.equal(result.output, unitFrame)
}
const multiInner = [null, null, null, null, [['candidate-unicode', ['中文😀 ' + marker]]]]
const richText = (text) => [[[[marker, ['plain-text', marker, text]]]]]
multiInner[26] = richText(marker)
const multiBody = JSON.stringify([['wrb.fr', 'rpc-id', JSON.stringify(multiInner), null]])
const diagnosticBody = JSON.stringify([['di', 123]])
const multiWire = ")]}'\n\n" + (multiBody.length + 1) + '\n' + multiBody + '\n' + (diagnosticBody.length + 1) + '\n' + diagnosticBody
const multiGemini = await decode('gemini', 'application/json', multiWire, 1)
assert.ok(multiGemini.output.endsWith((diagnosticBody.length + 1) + '\n' + diagnosticBody))
const firstLine = multiGemini.output.slice(6), firstNewline = firstLine.indexOf('\n'), firstLength = Number(firstLine.slice(0, firstNewline))
const firstPayload = firstLine.slice(firstNewline, firstNewline + firstLength)
assert.equal(firstPayload.length, firstLength)
const multiResult = JSON.parse(JSON.parse(firstPayload)[0][2])
assert.equal(multiResult[4][0][1][0], '中文😀 ' + original)
assert.equal(multiResult[26][0][0][0][1][2], original)
assert.equal(multiResult[26][0][0][0][0], marker, 'adjacent structured node identifiers remain unchanged')
assert.equal(multiResult[26][0][0][0][1][1], marker, 'adjacent structured metadata remains unchanged')
// Read-only Gemini conversation RPC: no identifiers/CSRF enter native masking.
const historyUrl = 'https://gemini.google.com/_/BardChatUi/data/batchexecute?rpcids=hNvQHb'
const historyProvider = codec.match(historyUrl, 'https://gemini.google.com')
assert.equal(historyProvider.history, true)
assert.equal(codec.match(historyUrl.replace('hNvQHb', 'MaZiqc'), 'https://gemini.google.com'), null)
const historyForm = 'at=' + encodeURIComponent(email) + '&f.req=' + encodeURIComponent(JSON.stringify([[
  ['hNvQHb', JSON.stringify([email, 10, null, 1, [1], [4], null, 1]), null, 'generic'],
]]))
const historyRequest = await codec.maskRequest(historyProvider, historyForm, async (text) => {
  assert.equal(text, '[]')
  return { ok: true, sid: 'ext:history', masked_text: text }
})
assert.equal(historyRequest.masked_text, historyForm)
assert.equal(historyRequest.sid, 'ext:history')
await assert.rejects(codec.maskRequest(historyProvider, 'f.req=' + encodeURIComponent(JSON.stringify([[['OtherRPC', 'credentials']]])), mask), /history_request_shape_changed/)
const candidate = Array(38).fill(null)
candidate[0] = marker // identifiers must stay untouched
candidate[1] = [marker]
candidate[22] = [marker]
candidate[37] = [[marker]]
const turn = [[null, 'turn-id'], null, [[marker]], [[candidate]]]
turn[3][12] = richText(marker)
const historyInner = [[turn]]
const historyBody = JSON.stringify([['wrb.fr', 'hNvQHb', JSON.stringify(historyInner)], ['wrb.fr', 'OtherRPC', 'not-json-credential-only']])
const historyWire = ")]}'\n\n" + (historyBody.length + 1) + '\n' + historyBody
const history = await decode(historyProvider, 'application/json', historyWire, 1)
const historyJSON = JSON.parse(history.output.slice(history.output.indexOf('\n', 6) + 1))
const restoredTurn = JSON.parse(historyJSON[0][2])[0][0]
assert.equal(restoredTurn[2][0][0], original)
assert.equal(restoredTurn[3][0][0][1][0], original)
assert.equal(restoredTurn[3][0][0][22][0], original)
assert.equal(restoredTurn[3][0][0][37][0][0], original)
assert.equal(restoredTurn[3][12][0][0][0][1][2], original)
assert.equal(restoredTurn[3][12][0][0][0][0], marker)
assert.equal(restoredTurn[3][12][0][0][0][1][1], marker)
assert.equal(restoredTurn[3][0][0][0], marker)
assert.equal(historyJSON[1][2], 'not-json-credential-only')
assert.equal(history.calls.length, 5)
const shapeReports = []
const shapeStream = codec.createRestorer(provider('gemini'), 'application/json', restore, (report) => shapeReports.push(report))
const shapeInner = [null, null, null, null, [candidate]]
shapeInner[26] = richText(marker)
const shapeBody = JSON.stringify([['wrb.fr', 'rpc', JSON.stringify(shapeInner)]])
const shapeOutput = await shapeStream.push(")]}'\n\n" + (shapeBody.length + 1) + '\n' + shapeBody, true)
const shapeRestoredInner = JSON.parse(JSON.parse(shapeOutput.slice(shapeOutput.indexOf('\n', 6) + 1))[0][2])
const shapeResult = shapeRestoredInner[4][0]
const shapeNode = shapeRestoredInner[26][0][0][0]
assert.equal(shapeResult[1][0], original)
assert.equal(shapeResult[22][0], original)
assert.equal(shapeResult[37][0][0], original)
assert.equal(shapeResult[0], marker)
assert.equal(shapeNode[1][2], original)
assert.equal(shapeNode[1][0], 'plain-text')
assert.equal(shapeNode[1][1], marker)
assert.ok(shapeReports[0].remainingMarkerPaths.some((item) => JSON.stringify(item.path) === '[4,0,0]'))
assert.ok(shapeReports[0].remainingMarkerPaths.some((item) => JSON.stringify(item.path) === '[26,0,0,0,0]'))
assert.ok(!shapeReports[0].remainingMarkerPaths.some((item) => JSON.stringify(item.path) === '[26,0,0,0,1,2]'), 'known structured text markers are restored before diagnostics')
assert.doesNotMatch(JSON.stringify(shapeReports), /EMAIL_|0123456789abcdef|credential-only|support-test/)
const jsonResponse = await decode('deepseek', 'application/json', JSON.stringify({ reply: marker }), 2)
assert.equal(JSON.parse(jsonResponse.output).reply, original)
const ndjson = await decode('kimi', 'application/x-ndjson', JSON.stringify({ text: marker }) + '\n')
assert.equal(JSON.parse(ndjson.output).text, original)

const broken = codec.createRestorer(provider('kimi'), 'text/event-stream', restore)
await broken.push(sse({ event: 'cmpl', text: '{{EMAIL_' }))
await assert.rejects(broken.push('', true), /incomplete_placeholder/)
const truncated = codec.createRestorer(provider('gemini'), 'application/json', restore)
await truncated.push(")]}'\n\n100\n[]")
await assert.rejects(truncated.push('', true), /truncated_frame/)
const failed = codec.createRestorer(provider('chatgpt'), 'text/event-stream', async () => { throw new Error('gateway unavailable') })
await assert.rejects(failed.push(sse({ text: marker })), /gateway unavailable/)
console.log('test-webchat-protocols: five synthetic request/response codecs, streaming, escaping, channel isolation and failure bounds OK (not live verification)')
