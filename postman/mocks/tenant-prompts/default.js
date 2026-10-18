// Local-only Langfuse contract fixture, based on promptmanager's saved
// GET/POST bootstrap contract tests. No live credentials or provider calls.
const http = require('node:http');
const prompts = new Map([
  ['teams/test/planner', { name: 'teams/test/planner', prompt: 'simulation system prompt', version: 1, labels: ['production', 'latest'] }],
]);
http.createServer(async (req, res) => {
  res.setHeader('Content-Type', 'application/json');
  const send = (status, body) => { res.writeHead(status); res.end(JSON.stringify(body)); };
  const url = new URL(req.url, 'http://local.test');
  const prefix = '/api/public/v2/prompts/';
  if (req.method === 'GET' && url.pathname.startsWith(prefix)) {
    const name = decodeURIComponent(url.pathname.slice(prefix.length));
    if (name !== 'teams/test/planner' && !name.startsWith('tenants/')) return send(501, { error: 'unmodeled source prompt' });
    const prompt = prompts.get(name);
    if (!prompt) return send(404, { error: 'not found' });
    if (url.searchParams.has('version') && Number(url.searchParams.get('version')) !== prompt.version) return send(404, { error: 'version not found' });
    if (url.searchParams.has('label') && !prompt.labels.includes(url.searchParams.get('label'))) return send(404, { error: 'label not found' });
    return send(200, prompt);
  }
  if (req.method === 'POST' && url.pathname === '/api/public/v2/prompts') {
    let raw = '';
    for await (const chunk of req) raw += chunk;
    let body;
    try { body = JSON.parse(raw); } catch { return send(400, { error: 'invalid JSON' }); }
    if (body.type !== 'text' || !body.name?.startsWith('tenants/') || typeof body.prompt !== 'string') return send(400, { error: 'invalid tenant prompt' });
    const previous = prompts.get(body.name);
    const prompt = { name: body.name, prompt: body.prompt, version: (previous?.version || 0) + 1, labels: [...new Set(['latest', ...(body.labels || [])])] };
    prompts.set(body.name, prompt);
    return send(201, prompt);
  }
  return send(501, { error: 'unmodeled Langfuse call' });
}).listen(process.env.PORT);
