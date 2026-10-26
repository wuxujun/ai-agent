// Local deterministic OpenAI Chat fixture, derived from planner/provider.go.
// No provider SDK, external requests, credentials, tools, or real inference.
const http = require('node:http');
const model = 'candidate-deterministic';
const finalAnswer = 'Isolated candidate completed deterministically.';
let scenario = 'success';
const scenarios = new Set(['success', 'timeout', 'http-json', 'http-html', 'malformed-stream']);
const sentinel = 'candidate-upstream-secret-sentinel';
const stats = { model_calls: 0, unexpected_calls: 0, streaming_calls: 0 };
function json(res, status, body) {
  res.writeHead(status, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(body));
}
const server = http.createServer((req, res) => {
  const path = req.url.split('?')[0];
  console.log(JSON.stringify({ event: 'request', method: req.method, path }));
  if (req.method === 'GET' && path === '/healthz') return json(res, 200, { ok: true });
  if (req.method === 'GET' && path === '/_stats') return json(res, 200, stats);
  if (req.method === 'POST' && path.startsWith('/_scenario/')) {
    const selected = path.slice('/_scenario/'.length);
    if (!scenarios.has(selected)) return json(res, 400, { error: 'unknown scenario' });
    scenario = selected;
    return json(res, 200, { scenario });
  }
  if (req.method !== 'POST' || path !== '/v1/chat/completions') {
    stats.unexpected_calls++;
    return json(res, 501, { error: 'unmodeled candidate dependency call' });
  }
  const chunks = [];
  let bytes = 0;
  req.on('data', chunk => {
    bytes += chunk.length;
    if (bytes > 1024 * 1024) req.destroy();
    else chunks.push(chunk);
  });
  req.on('end', () => {
    let input;
    try { input = JSON.parse(Buffer.concat(chunks)); }
    catch { return json(res, 400, { error: 'JSON request required' }); }
    if (input.model !== model || !Array.isArray(input.messages)
        || !input.messages.some(message => message.role === 'system')
        || !input.messages.some(message => message.role === 'user')) {
      return json(res, 400, { error: 'candidate planner contract mismatch' });
    }
    stats.model_calls++;
    if (scenario === 'timeout') {
      // Hold headers beyond the candidate's configured 3s request timeout.
      const timer = setTimeout(() => res.end(), 20000);
      res.on('close', () => clearTimeout(timer));
      return;
    }
    if (scenario === 'http-json') return json(res, 500, { error: { message: sentinel } });
    if (scenario === 'http-html') {
      res.writeHead(500, { 'Content-Type': 'text/html' });
      return res.end(`<html><body>${sentinel}</body></html>`);
    }
    if (scenario === 'malformed-stream') {
      res.writeHead(200, { 'Content-Type': 'text/event-stream' });
      return res.end('data: {broken-json\n\ndata: [DONE]\n\n');
    }
    const content = JSON.stringify({
      thought_summary: 'Deterministic local fixture; no tools needed.',
      stop: true, final_answer: finalAnswer, actions: [{ action: 'none', parameters: {} }],
    });
    const usage = { prompt_tokens: 10, completion_tokens: 5, total_tokens: 15 };
    if (input.stream === true) {
      stats.streaming_calls++;
      res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' });
      res.write(`data: ${JSON.stringify({ choices: [{ index: 0, delta: { role: 'assistant', content } }] })}\n\n`);
      res.write(`data: ${JSON.stringify({ choices: [], usage })}\n\n`);
      return res.end('data: [DONE]\n\n');
    }
    return json(res, 200, { choices: [{ index: 0, message: { role: 'assistant', content } }], usage });
  });
});
server.listen(Number(process.env.PORT || 0), '127.0.0.1', () => {
  console.log(JSON.stringify({ event: 'listening', port: server.address().port }));
});
