// The publisher's tool stays resident across the prepared snapshot and original calls.
import fs from 'node:fs';
import net from 'node:net';

const socket = '/tmp/zb-web-fetch.sock';
if (process.argv[2] === 'serve') {
  const allowed = JSON.parse(fs.readFileSync(process.argv[3], 'utf8'));
  const { Vt: createTools } = await import('/usr/lib/node_modules/openclaw/dist/reply-BCcP6j4h.js');
  const tool = createTools({ workspaceDir: '/tmp_workspace', config: {} }).find(t => t.name === 'web_fetch');
  if (!tool || !Array.isArray(allowed) || !allowed.length) throw Error('Missing tool or approved source arguments');
  net.createServer({ allowHalfOpen: true }, client => {
    let body = '';
    client.setEncoding('utf8');
    client.on('data', chunk => { body += chunk; if (body.length > 16384) client.destroy(); });
    client.on('end', async () => {
      try {
        const args = JSON.parse(body);
        if (!allowed.some(a => Object.keys(a).length === Object.keys(args).length &&
            Object.keys(a).every(k => a[k] === args[k]))) throw Error('Unrecognized source arguments');
        const result = await tool.execute('web_fetch', args);
        const text = result.content.filter(c => c.type === 'text').map(c => c.text).join('\n');
        client.end(JSON.stringify({ status: result.isError || result.details?.error ? 1 : 0, text }));
      } catch (err) {
        client.end(JSON.stringify({ status: 1, text: JSON.stringify({ status: 'error', tool: 'web_fetch', error: String(err.message) }, null, 2) }));
      }
    });
  }).listen(socket, () => fs.chmodSync(socket, 0o600));
} else {
  let reply = '';
  const client = net.createConnection(socket, () => client.end(process.argv[2]));
  client.setEncoding('utf8');
  client.on('data', chunk => { reply += chunk; });
  client.on('end', () => {
    const result = JSON.parse(reply);
    process.stdout.write(result.text);
    process.exitCode = result.status;
  });
}
