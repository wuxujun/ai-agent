// Local-only test identity provider. These fixed strings are synthetic fixtures.
// Contract follows internal/api/introspection_test.go; not a production IdP.
const http = require('node:http');
http.createServer((req, res) => {
  res.setHeader('Content-Type', 'application/json');
  if (req.method !== 'GET' || req.url.split('?')[0] !== '/oauth/introspect') {
    res.writeHead(501); res.end(JSON.stringify({error: 'unmodeled candidate dependency call'})); return;
  }
  const fixtures = {
    'Bearer candidate-smoke-a': ['candidate-a', 'subject-a'],
    'Bearer candidate-smoke-b': ['candidate-b', 'subject-b'],
  };
  const identity = fixtures[req.headers.authorization];
  if (!identity) {res.writeHead(200); res.end(JSON.stringify({data:{active:false}})); return;}
  res.writeHead(200);
  res.end(JSON.stringify({iss:'https://candidate.invalid', data:{active:true, userid:identity[0], sub:identity[1], aud:'candidate-console', exp:Math.floor(Date.now()/1000)+600}}));
}).listen(process.env.PORT);
