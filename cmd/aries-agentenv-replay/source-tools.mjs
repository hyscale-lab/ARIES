// The three pinned recordings need OpenClaw's own process table or resized image attachments.
import { createHash } from 'node:crypto';
import fs from 'node:fs';
import { registerHooks } from 'node:module';
import net from 'node:net';

const socket = '/tmp/zb-source-tools.sock';
const resume = '0b11c9b18a35d05428c3543a84095e06347d47357ba4939b75c931fa39946b6b';
const approved = new Map([
  [resume, '1ee0cb633f77a2a7d866f025066d077e3b14559cbff950868bbf1c3b75a7bd0a'],
  ['73b9e9bc867c86be5c7c9125553971a9832d6760a256c4091f69f8c3a60308de', '6bf636a59807f4a0da3def240af28e7726889ea8926e52cb5a789fbb00c6bb71'],
  ['73a1f7bfae3bf80a7fa25f6d47eda911a69492e85ed0a79e63b05c83ef6356da', '9a98d4d83a96601004bc100ec2d24a3a45ac647609bc69edd789caa74e6176b5']
]);
const sha256 = data => createHash('sha256').update(data).digest('hex');

if (process.argv[2] === 'prepare') {
  const bytes = fs.readFileSync(process.argv[3]);
  const sha = sha256(bytes);
  if (!approved.has(sha)) throw Error('unreviewed source');
  const calls = new Map();
  for (const line of bytes.toString().trimEnd().split('\n')) {
    const message = JSON.parse(line).message;
    for (const c of message?.content ?? []) {
      if (c.type === 'toolCall') calls.set(c.id, {id:c.id,name:c.name,args:c.arguments});
    }
    if (message?.role !== 'toolResult') continue;
    const c = calls.get(message.toolCallId);
    if (c.name === 'exec' && message.details?.status === 'running') c.handle = message.details.sessionId;
    else if (!(c.name === 'process' || (sha === resume && (c.name === 'image' || c.name === 'read')))) calls.delete(c.id);
  }
  process.stdout.write(JSON.stringify({sha,calls:[...calls.values()]}));
} else if (process.argv[2] === 'serve') {
  const bytes = fs.readFileSync(process.argv[3]);
  const manifest = JSON.parse(bytes);
  if (approved.get(manifest.sha) !== sha256(bytes)) throw Error('unreviewed guest tool manifest');
  const calls = new Map(manifest.calls.map(c => [c.id,c]));
  const bundle = '/usr/lib/node_modules/openclaw/dist/reply-BCcP6j4h.js';
  // The installed bundle has no public export for its coding tools. Expose the original factory only.
  registerHooks({load(url, context, nextLoad) {
    const loaded = nextLoad(url,context);
    if (url !== 'file://' + bundle) return loaded;
    if (sha256(loaded.source) !== '9da5417102041098b85055c7fa6e517663d4b6c67af2e6c95afc831712926b68') throw Error('unreviewed OpenClaw bundle');
    return {...loaded,source:Buffer.from(loaded.source).toString()+'\nexport { createOpenClawCodingTools };'};
  }});
  const { createOpenClawCodingTools } = await import(bundle);
  const primary = 'new-api/gpt-5.6-sol';
  const model = {id:'gpt-5.6-sol',name:'gpt-5.6-sol',input:['text'],reasoning:false,
    cost:{input:0,output:0,cacheRead:0,cacheWrite:0},contextWindow:8192,maxTokens:1024};
  const config = {models:{providers:{'new-api':{api:'openai-completions',baseUrl:'http://127.0.0.1:1/v1',
    apiKey:'offline-diagnostic-placeholder',models:[model]}}},agents:{defaults:{model:{primary},imageModel:{primary}}}};
  const tools = new Map(createOpenClawCodingTools({config,agentDir:'/tmp/zb-native-agent',workspaceDir:manifest.sha === resume ? '/tmp_workspace' : '/root'})
    .map(t => [t.name,t]));
  const handles = new Map();
  net.createServer({allowHalfOpen:true}, client => {
    client.on('error', () => {});
    let body = '';
    client.setEncoding('utf8');
    client.on('data', chunk => { body += chunk; if (body.length > 2048) client.destroy(); });
    client.on('end', async () => {
      try {
        const {sha,id,diagnostic} = JSON.parse(body);
        const c = calls.get(id);
        if (sha !== manifest.sha || !c || !tools.has(c.name)) throw Error('unbound source tool');
        const args = {...c.args};
        if (c.name === 'process' && args.action === 'poll') {
          args.sessionId = handles.get(args.sessionId);
          if (!args.sessionId) throw Error('source poll has no matching live OpenClaw handle');
          if (diagnostic) args.timeout = Math.min(args.timeout,5000);
        } else if (diagnostic) throw Error('unexpected diagnostic request');
        let result;
        try {
          result = await tools.get(c.name).execute(id,args);
        } catch (err) {
          const details = {status:'error',tool:c.name,error:err.message};
          result = {details,content:[{type:'text',text:JSON.stringify(details,null,2)}]};
        }
        if (c.name === 'exec' && result.details?.status === 'running') {
          if (!result.details.sessionId || !c.handle) throw Error('unbound live process');
          handles.set(c.handle,result.details.sessionId);
        }
        client.end(JSON.stringify({result}));
      } catch (err) {
        client.end(JSON.stringify({transportError:err.message}));
      }
    });
  }).listen(socket, () => fs.chmodSync(socket,0o600));
} else if (process.argv[2] === 'call') {
  const [sha,id,mode] = process.argv.slice(3);
  const client = net.createConnection(socket, () => client.end(JSON.stringify({sha,id,diagnostic:mode === 'diagnostic'})));
  for await (const chunk of client) process.stdout.write(chunk);
} else {
  throw Error('expected prepare, serve or call');
}
