// Run the v1.3 image's web_search tool in the guest. Only the provider HTTP hop leaves via the broker.
import { AsyncLocalStorage } from 'node:async_hooks';
import { chmodSync } from 'node:fs';
import net from 'node:net';

const socket = '/tmp/zb-brave.sock';
if (process.argv[2] === 'serve') {
  const broker = new URL(process.env.ZB_BRAVE_BROKER_URL);
  if (!['http:', 'https:'].includes(broker.protocol) || broker.pathname !== '/search' ||
      broker.username || broker.password || broker.search || broker.hash) throw Error('invalid Brave broker URL');
  const { Vt: createOpenClawTools } = await import('/usr/lib/node_modules/openclaw/dist/reply-BCcP6j4h.js');
  const config = {tools:{web:{search:{provider:'brave', apiKey:'runner-broker-placeholder', cacheTtlMinutes:0}}}};
  const tool = createOpenClawTools({workspaceDir:'/tmp_workspace',config}).find(t => t.name === 'web_search');
  if (!tool) throw Error('missing image web_search tool');
  const requests = new AsyncLocalStorage();
  const fetchBroker = globalThis.fetch;
  globalThis.fetch = async (target, init) => {
    const call = requests.getStore();
    const url = new URL(target);
    if (!call || call.dispatched || init.method !== 'GET' ||
        url.origin + url.pathname !== 'https://api.search.brave.com/res/v1/web/search' ||
        url.searchParams.get('q') !== call.args.query) throw Error('unexpected native provider request');
    call.dispatched = true;
    return fetchBroker(broker, {method:'POST', redirect:'error', signal:init.signal,
      headers:{'Content-Type':'application/json'}, body:JSON.stringify({source_sha256:call.sha,call_id:call.id})});
  };
  net.createServer({allowHalfOpen:true}, client => {
    client.on('error', () => {});
    client.setEncoding('utf8');
    let body = '';
    client.on('data', chunk => {
      body += chunk;
      if (body.length > 16384) client.destroy();
    });
    client.on('end', async () => {
      try {
        const {sha,id,args} = JSON.parse(body);
        if (!/^[0-9a-f]{64}$/.test(sha) || !/^call_[A-Za-z0-9]+$/.test(id) ||
            typeof args?.query !== 'string') throw Error('invalid search binding');
        const call = {sha,id,args,dispatched:false};
        const result = await requests.run(call, () => tool.execute(id,args));
        if (!call.dispatched || result.details?.provider !== 'brave' || result.details.query !== args.query ||
            !Array.isArray(result.details.results) || result.details.count !== result.details.results.length ||
            typeof result.content?.[0]?.text !== 'string') throw Error('native Brave tool did not return results');
        client.end(JSON.stringify({exit:0,text:result.content[0].text}));
      } catch (error) {
        client.end(JSON.stringify({exit:1,text:JSON.stringify({status:'error',tool:'web_search',error:error.message},null,2)}));
      }
    });
  }).listen(socket, () => chmodSync(socket,0o600));
} else if (process.argv[2] === 'call') {
  const [sha,id,raw] = process.argv.slice(3);
  const request = JSON.stringify({sha,id,args:JSON.parse(raw)});
  const client = net.createConnection(socket, () => client.end(request));
  client.setEncoding('utf8');
  let body = '';
  for await (const chunk of client) body += chunk;
  const {exit,text} = JSON.parse(body);
  if ((exit !== 0 && exit !== 1) || typeof text !== 'string') throw Error('invalid native tool reply');
  process.stdout.write(text);
  process.exitCode = exit;
} else {
  throw Error('expected serve or call');
}
