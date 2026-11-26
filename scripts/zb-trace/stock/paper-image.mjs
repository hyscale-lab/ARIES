// Keep the published image tool resident across the prepared snapshot and tool calls.
import fs from 'node:fs';
import net from 'node:net';
const socket = '/tmp/zb-paper-image.sock';
if (process.argv[2] === 'serve') {
  const { Vt: createOpenClawTools } = await import('/usr/lib/node_modules/openclaw/dist/reply-BCcP6j4h.js');
  const root = '/tmp/zb-paper-agent';
  fs.mkdirSync(root, {mode: 0o700, recursive: true});
  const primary = 'new-api/gpt-5.6-sol';
  const model = {id:'gpt-5.6-sol', name:'gpt-5.6-sol', input:['text'], reasoning:false,
    cost:{input:0, output:0, cacheRead:0, cacheWrite:0}, contextWindow:8192,maxTokens:1024};
  const config = {models:{providers:{'new-api':{api:'openai-completions',baseUrl:'http://127.0.0.1:1/v1',
    apiKey:'offline-diagnostic-placeholder',models:[model]}}},agents:{defaults:{model:{primary},imageModel:{primary}}}};
  const tool = createOpenClawTools({workspaceDir:'/tmp_workspace',agentDir:root,config}).find(t=>t.name==='image');
  net.createServer({allowHalfOpen:true}, client=>{
    let body='';
    client.setEncoding('utf8');
    client.on('data', chunk=>{body+=chunk; if(body.length>16384)client.destroy();});
    client.on('end',async ()=>{
      try {
        const args=JSON.parse(body);
        const keys = Object.keys(args).sort().join();
        const paper = keys === 'images,prompt' && Array.isArray(args.images) && args.images.length === 3 &&
          args.images.every(p=>/^\/tmp_workspace\/page_renders\/page_\d+\.png$/.test(p));
        const sam = keys === 'image,prompt' && args.image === '/tmp_workspace/sam3/assets/images/test_image.jpg';
        if (!paper && !sam) throw Error('Unexpected image arguments');
        const result=await tool.execute('paper-image',args);
        client.end(JSON.stringify(result));
      } catch(error) {
        client.end(JSON.stringify({status:'error',tool:'image',error:error.message},null,2));
      }
    });
  }).listen(socket,()=>fs.chmodSync(socket,0o600));
} else {
  const client=net.createConnection(socket,()=>client.end(process.argv[2]));
  client.pipe(process.stdout);
}
