// Local failure-injection fixture based on web_search's existing HTTP and
// Firecrawl {error:string} response contract. Not a provider conformance mock.
const http = require('node:http');
http.createServer(async (req, res) => {
  for await (const chunk of req) { /* drain bounded fixture input */ }
  const path = new URL(req.url, 'http://fixture.invalid').pathname;
  const code = path.match(/^\/firecrawl\/(401|429|500)$/);
  res.setHeader('Content-Type', 'application/json');
  if (req.method === 'POST' && code) {
    res.writeHead(Number(code[1]));
    return res.end(JSON.stringify({error:'search-sensitive-sentinel'}));
  }
  if (req.method === 'POST' && path === '/firecrawl/business') {
    return res.end(JSON.stringify({success:false,error:'search-sensitive-sentinel'}));
  }
  res.writeHead(501);
  res.end(JSON.stringify({error:'unmodeled request'}));
}).listen(process.env.PORT);
