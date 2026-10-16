import http from 'node:http'

const host = process.env.SILENCER_FIXTURE_ADDR ?? '127.0.0.1'
const port = Number.parseInt(process.env.SILENCER_FIXTURE_PORT ?? '19090', 10)
const integerSetting = (name, fallback) => Number(process.env[name] ?? fallback)
const sseFrames = integerSetting('SILENCER_FIXTURE_SSE_FRAMES', 1)
const ndjsonFrames = integerSetting('SILENCER_FIXTURE_NDJSON_FRAMES', 2)
const frameIntervalMs = integerSetting('SILENCER_FIXTURE_FRAME_INTERVAL_MS', 0)
const firstByteDelayMs = integerSetting('SILENCER_FIXTURE_FIRST_BYTE_DELAY_MS', 0)

if (![sseFrames, ndjsonFrames, frameIntervalMs, firstByteDelayMs].every(Number.isInteger) ||
    sseFrames < 1 || ndjsonFrames < 1 || frameIntervalMs < 0 || firstByteDelayMs < 0) {
  throw new Error('fixture frame and delay settings must be non-negative integers; frame counts must be positive')
}

const chatResponse = JSON.stringify({
  id: 'fixture',
  object: 'chat.completion',
  choices: [{ index: 0, message: { role: 'assistant', content: 'fixture response' }, finish_reason: 'stop' }],
  usage: { prompt_tokens: 128, completion_tokens: 16, total_tokens: 144 },
})

const server = http.createServer((req, res) => {
  if (req.method === 'GET' && req.url === '/healthz') {
    res.writeHead(200, { 'content-type': 'text/plain', 'content-length': '2' })
    res.end('ok')
    return
  }

  let requestBytes = 0
  req.on('data', (chunk) => { requestBytes += chunk.length })
  req.on('end', () => {
    res.setHeader('x-fixture-request-bytes', String(requestBytes))
    if (req.url === '/v1/stream') {
      const writeFrame = (index) => {
        if (index >= sseFrames) {
          res.end('data: [DONE]\n\n')
          return
        }
        if (index === 0) {
          res.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' })
        }
        res.write(`data: {"choices":[{"delta":{"content":"fixture-${index + 1}"}}]}\n\n`)
        setTimeout(() => writeFrame(index + 1), frameIntervalMs)
      }
      setTimeout(() => writeFrame(0), firstByteDelayMs)
      return
    }
    if (req.url === '/v1/ndjson') {
      const writeFrame = (index) => {
        if (index >= ndjsonFrames) {
          res.end()
          return
        }
        if (index === 0) {
          res.writeHead(200, { 'content-type': 'application/x-ndjson' })
        }
        const done = index === ndjsonFrames - 1
        const payload = done
          ? { model: 'fixture', done: true }
          : { model: 'fixture', response: `fixture-${index + 1}`, done: false }
        res.write(`${JSON.stringify(payload)}\n`)
        setTimeout(() => writeFrame(index + 1), frameIntervalMs)
      }
      setTimeout(() => writeFrame(0), firstByteDelayMs)
      return
    }
    res.writeHead(200, {
      'content-type': 'application/json',
      'content-length': String(Buffer.byteLength(chatResponse)),
    })
    res.end(chatResponse)
  })
})

server.listen(port, host, () => {
  process.stdout.write(`benchmark fixture listening on http://${host}:${port}\n`)
})

for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => server.close(() => process.exit(0)))
}
