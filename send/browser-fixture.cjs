// Run: node send/browser-fixture.cjs, then open http://127.0.0.1:18788 in DevTools.
// Isolated browser acceptance data; never reads or changes real Codex credentials.
const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');
let pair = {config:'model = "official"\n[mcp_servers.demo]\ncommand = "node"\n',auth:'{"tokens":{"access_token":"fixture-only"}}',profiles:[],active:'',revision:'1',home:'/fixture/.codex'};
const thread = {id:'fixture',name:'持续更新验收',cwd:'/fixture/project',status:{type:'active'},turns:[{id:'turn',status:'inProgress',items:Array.from({length:55},(_,i)=>({id:`message-${i}`,type:'agentMessage',text:`消息 ${i}\n\n这是一段验收消息，用来检查连续更新时的滚动位置。`.repeat(2)}))}]};
const files = {
 '/fixture/project/readme.md':'# Markdown 预览\n\n**加粗内容**与[链接](https://example.com)\n\n| 名称 | 状态 |\n|---|---|\n| 文件预览 | 完成 |',
 '/fixture/project/page.html':'<!doctype html><style>h1 { color: rgb(123, 45, 67) }</style><h1>HTML 预览</h1><script>parent.document.body.dataset.injected="yes"</script>',
 '/fixture/project/data.json':'{"name":"格式化 JSON","values":[1,2,3]}',
 '/fixture/project/long.txt':'very-long-line-'.repeat(150),
};
http.createServer(async (req,res)=>{
 const u=new URL(req.url,'http://localhost'); let body='';for await(const chunk of req)body+=chunk;
 const json=value=>{res.setHeader('Content-Type','application/json');res.end(JSON.stringify(value));};
 if(u.pathname==='/api/session')return json({authenticated:true});
 if(u.pathname==='/api/status')return json({version:'0.7.0',backend:{connected:true,transport:'shared-daemon'},workspaceMode:'unrestricted'});
 if(u.pathname==='/api/events'){res.writeHead(200,{'Content-Type':'text/event-stream'});res.write(': fixture\n\n');return;}
 if(u.pathname==='/api/requests')return json({requests:[],history:[]});
 if(u.pathname==='/api/codex/settings'){
  if(req.method==='POST'){const b=JSON.parse(body);if(b.action==='save'){pair.config=b.config;pair.auth=b.auth;}if(b.action==='profile')pair.profiles.push({id:'fixture-profile',name:b.name});pair.revision=String(+pair.revision+1);return json({ok:true});}return json(pair);
 }
 if(u.pathname==='/api/files'){
  const f=u.searchParams.get('path');if(files[f]){if(u.searchParams.has('preview')){res.setHeader('Content-Type','text/html; charset=utf-8');res.setHeader('Content-Security-Policy',"sandbox; default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'");}else res.setHeader('Content-Type','text/plain');return res.end(files[f]);}
  return json({path:'/fixture/project',parent:'/fixture',entries:[{name:'long.txt',path:'/fixture/project/long.txt',type:'file',size:2100},{name:'folder',path:'/fixture/project/folder',type:'directory'},...Object.keys(files).filter(p=>!p.endsWith('long.txt')).map(p=>({name:path.basename(p),path:p,type:'file',size:files[p].length}))]});
 }
 if(u.pathname==='/api/rpc'){
  const {method}=JSON.parse(body);if(method==='thread/list')return json({data:[thread]});
  if(method==='thread/read')return json({thread});
  if(method==='model/list')return json({data:[{id:'model',model:'official',displayName:'Official',supportedReasoningEfforts:[],isDefault:true}]});
  if(method==='account/rateLimits/read')return json({rateLimits:{primary:{usedPercent:20,windowDurationMins:300},secondary:{usedPercent:35,windowDurationMins:10080}}});
  return json({});
 }
 let file=u.pathname==='/'?'index.html':u.pathname.slice(1);
 if(!['index.html','browser-checks.js','styles.css','app.js','markdown.js','icon.png','manifest.webmanifest','fonts/HarmonyOS_Sans_SC.ttf'].includes(file)){res.statusCode=404;return res.end();}
 let data=fs.readFileSync(path.join(__dirname,file));
 if(file==='index.html')data=data.toString().replace('</body>', '<script src="./browser-checks.js"></script></body>');
 if(file==='app.js')data=data.toString().replace(/\n\}\)\(\);\s*$/,'\nregisterServiceWorker = async () => {}; globalThis.__crTest = {state,selectThread,renderTimeline,handleNotification,openStatusDialog,openProjectFiles,openProjectFile,renderProjectDirectory,setThemePreference,renderProjectPreview};\n})();');
 res.setHeader('Content-Type',file.endsWith('.js')?'text/javascript':file.endsWith('.css')?'text/css':file.endsWith('.html')?'text/html':'application/octet-stream');
 res.setHeader('Content-Security-Policy',"default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; font-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'");
 res.end(data);
}).listen(18788,'127.0.0.1',()=>console.log('Browser fixture: http://127.0.0.1:18788'));
