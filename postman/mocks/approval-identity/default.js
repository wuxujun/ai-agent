// Synthetic local identity fixtures matching introspection_test.go. No real tokens.
const http = require('node:http');
http.createServer((req, res) => {
  console.log(JSON.stringify({event: 'request', method: req.method, path: req.url.split('?')[0]}));
  res.setHeader('Content-Type', 'application/json');
  if (req.method !== 'GET' || req.url !== '/oauth/introspect') {
    res.writeHead(501); res.end(JSON.stringify({error: 'unmodeled identity dependency call'})); return;
  }
  const users = {
    'Bearer reviewer': '6492', 'Bearer reviewer-refreshed': '6492',
    'Bearer creator': '7001', 'Bearer creator-refreshed': '7001',
    'Bearer unlisted': '7002', 'Bearer expired': '6492',
  };
  const userid = users[req.headers.authorization];
  res.writeHead(200);
  res.end(JSON.stringify({iss: 'https://issuer.example', data: {
    active: Boolean(userid), userid: userid || '', aud: 'ai-agent',
    exp: Math.floor(Date.now() / 1000) + (req.headers.authorization === 'Bearer expired' ? -60 : 600),
    roles: ['admin', 'approver'], admin: true,
  }}));
}).listen(process.env.PORT, '127.0.0.1', () => console.log(JSON.stringify({event: 'listening'})));
