// Minimal webhook receiver that verifies TwinStub signatures.
// Node 18+, no dependencies: node verify.js
const crypto = require('node:crypto');
const http = require('node:http');

const SECRET = process.env.WEBHOOK_SECRET || 'whsec_fintech_demo';
const MAX_SKEW_SECONDS = 300;

function verify(secret, header, rawBody) {
  if (!header) return 'signature header missing';
  const parts = Object.fromEntries(
    header.split(',').map((p) => p.split('=', 2))
  );
  const ts = parts.t;
  const sig = parts.v1;
  if (!ts || !sig) return `malformed signature header: ${header}`;
  const age = Math.abs(Date.now() / 1000 - Number(ts));
  if (!Number.isFinite(age) || age > MAX_SKEW_SECONDS) {
    return `timestamp outside the ${MAX_SKEW_SECONDS}s window`;
  }
  const expected = crypto
    .createHmac('sha256', secret)
    .update(`${ts}.`)
    .update(rawBody) // raw bytes, never re-serialized JSON
    .digest('hex');
  const a = Buffer.from(expected);
  const b = Buffer.from(sig);
  if (a.length !== b.length || !crypto.timingSafeEqual(a, b)) {
    return 'signature mismatch';
  }
  return null;
}

http
  .createServer((req, res) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => {
      const body = Buffer.concat(chunks);
      const err = verify(SECRET, req.headers['x-twinstub-signature'], body);
      if (err) {
        console.log(`signature INVALID (${err}): ${body}`);
        res.writeHead(401).end('bad signature');
        return;
      }
      console.log(`signature OK: ${req.headers['x-twinstub-event']} ${body}`);
      res.writeHead(200).end();
    });
  })
  .listen(9999, () => console.log('listening on :9999'));
