// Local reload contract from internal/api/config_handler.go and Sample/agent-api.http.
// Only synthetic keys are used. No upstream response bodies or credentials logged.
const http = require('node:http');
http.createServer((req, res) => {
  const path = req.url.split('?')[0];
  console.log(JSON.stringify({event: 'request', method: req.method, path}));
  if (req.method !== 'POST' || !/^\/(ok|failure|hang)\/api\/config\/reload$/.test(path)) {
    res.writeHead(501); res.end(); return;
  }
  if (path.startsWith('/hang/')) {
    const timer = setTimeout(() => { res.writeHead(200); res.end('{}'); }, 2000);
    res.on('close', () => clearTimeout(timer)); return;
  }
  res.setHeader('Content-Type', 'application/json');
  res.writeHead(path.startsWith('/failure/') ? 500 : 200);
  res.end(path.startsWith('/failure/') ? '{"code":"internal_error"}' : '{"status":"reloaded"}');
}).listen(process.env.PORT, '127.0.0.1', () => console.log(JSON.stringify({event:'listening'})));
