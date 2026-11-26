const { chromium } = await import(process.env.HATAORI_PLAYWRIGHT || 'playwright-core');
import http from 'node:http';
import fs from 'node:fs/promises';
import path from 'node:path';

const repo = path.resolve(new URL('../../..', import.meta.url).pathname);
const output = process.env.CAPTURE_OUTPUT || '/tmp/hataori-dashboard-captures';
const chosen = process.env.CAPTURE_VIEWS?.split(',');
await fs.mkdir(output, {recursive:true});
const at = '2026-11-01T10:00:00Z';
const titles = ['Build a calmer project overview with a very long title that should still be readable', 'Choose the release illustration', 'Review the color palette', 'Save the next idea for later', 'Try the new preview', 'Prepare the workspace', 'Finish the sample project', 'Remove the old experiment', 'Resolve the example startup warning'];
const states = ['running','waiting','failed','asleep','idle','creating','closed','removed','failed','waiting'];
const threads = states.map((state,i)=>({id:i+1,scope:i<5?'design':'writing',links:[],title:titles[i]||'Approve the proposed draft',repo:'demo-project',state,started_by:'Demo designer',created:at,updated:at,agent:'codex',model:'demo-model',effort:'medium',turns:2,last:'The fictional preview is ready for review.',context:{used:42000,size:200000},tokens:{turn:1200,today:24000}}));
const events = n => [
 {pos:n*10,thread:n,time:at,kind:'turn_started',data:{by:'Demo designer',turn:1}},
 {pos:n*10+1,thread:n,time:at,kind:'input',data:{text:'Please make a calm, readable preview for the demo project.',by:'Demo designer'}},
 {pos:n*10+2,thread:n,time:at,kind:'text',data:{text:'I have prepared the fictional preview. The colors are consistent and the main actions are easy to find.\n\nHere is the next step: review the illustration and choose the version you prefer.'}},
 {pos:n*10+3,thread:n,time:at,kind:'tool_started',data:{tool:'Preview',call:'preview',input:'Render the fictional preview'}},
 {pos:n*10+4,thread:n,time:at,kind:'tool_ended',data:{call:'preview',tool:'Preview',output:'Preview ready'}},
 ...(n===3||n===9?[{pos:n*10+5,thread:n,time:at,kind:'state',data:{from:'creating',to:'failed',reason:'The fictional preview could not start. Please review the example settings and try again.'}}]:[])
];
const request = n => ({id:'fictional-question',kind:n===10?1:0,text:n===10?'Allow the fictional preview to continue?':(scenario==='long-question'?'Illustration'+('Preview'.repeat(22)):'Which illustration should the fictional preview use?'),choices:[{token:'warm',label:'A warm illustration with an unusually long descriptive name for the preview'},{token:'cool',label:'A cool illustration'},{token:'simple',label:'A simple outline'}]});
const detail = n => ({...threads[n-1],requests:[2,10].includes(n)?[request(n)]:[],events:events(n),...(n===9?{failed_start:{session:1,event:95,reason:'blocked',detail:'The fictional preview needs a confirmation before it can start.',text:'Prepare the fictional preview.'}}:{})});
let scenario = 'normal';
const hosts=[{device:'demo-shell',process:'zsh',activity:'2026-10-01T10:00:00Z',tmux:{pane:'demo-shell-pane',session:'Archive',window:'1',window_name:'Old notes',index:'0',attached:false}},{device:'demo-terminal',process:'preview',activity:at,tmux:{pane:'demo-pane',session:'Preview',window:'1',window_name:'Illustration review',index:'0',attached:true}},{device:'demo-console',process:'notes',activity:at}];
const server = http.createServer(async(req,res)=>{
 try {
 const u=new URL(req.url,'http://example.test'), p=u.pathname;
 const json=x=>{res.writeHead(200,{'Content-Type':'application/json'});res.end(JSON.stringify(x));};
 if(p==='/api/threads')return scenario==='empty'?json([]):scenario==='signed-out'?(res.writeHead(401),res.end()):json(threads);
 if(p==='/api/scopes')return json([{key:'design',name:'Design studio'},{key:'writing',name:'Writing room'}]);
 if(p==='/api/layout')return json([{client:'Demo chat',groups:[{id:'creative',name:'Creative projects',channels:[{id:'illustration',name:'design-studio',scope:'design',threads:threads.filter(x=>x.scope==='design').map(x=>({id:'demo'+x.id,name:x.title,thread:x.id}))},{id:'drafts',name:'writing-room',scope:'writing',threads:threads.filter(x=>x.scope==='writing').map(x=>({id:'demo'+x.id,name:x.title,thread:x.id}))}]}]}]);
 if(p==='/api/thread')return json(detail(Number(u.searchParams.get('thread'))));
 if(p==='/api/threadevents')return json(u.searchParams.has('before')?[]:events(Number(u.searchParams.get('thread'))));
 if(p==='/api/me')return json({person:'Demo designer',way:'local',passkey:false});
 if(p==='/api/limits')return json([{agent:'codex',at,every:999999999,windows:[{name:'session',minutes:300,used:35,resets:'2026-11-02T10:00:00Z'}]}]);
 if(p==='/api/clients')return json([{client:'Demo chat',connected:true,since:at}]);
 if(p==='/api/workspace')return json({branch:'demo-preview',base:'main',added:18,removed:4,uncommitted:2,unpushed:1});
 if(p==='/api/hostterminals')return json(hosts);
 if(p==='/api/act/may'||p==='/api/operate/may')return json({may:true});
 if(p==='/api/passkeys')return json([{id:'fictional-key',made:at,used:at}]);
 if(p==='/api/trusts')return json([{id:1,scope:'Design studio',repo:'demo-project',file:scenario==='long-trusts'?'ExampleSettings'.repeat(18)+'.json':'example-settings.json',by:'Demo designer',at}]);
 if(p==='/api/hostscreen'){res.writeHead(200,{'Content-Type':'text/event-stream'});res.write('data: '+JSON.stringify({screen:'Fictional preview terminal\n\nPreview is ready.'})+'\n\n');return;}
 if(p==='/api/terminal'&&req.method==='HEAD'&&[4,5,9].includes(Number(u.searchParams.get('thread')))){res.writeHead(404);return res.end();}
 if(p==='/api/events'||p==='/api/terminal'){
   res.writeHead(200,{'Content-Type':'text/event-stream','Cache-Control':'no-cache'});
   if(req.method==='HEAD')return res.end();
   const data=p==='/api/events'?{pos:200}:{run:'demo',pos:1,reset:true,cols:72,rows:16,data:Buffer.from('\x1b[2J\x1b[HFictional preview terminal\r\n\r\nPreparing a calm project overview…\r\n\r\n✓ Colors reviewed\r\n✓ Spacing checked\r\n\r\nPreview is ready.').toString('base64')};
   res.write(`event: ${p==='/api/events'?'caught_up':'message'}\ndata: ${JSON.stringify(data)}\n\n`);
   return;
 }
 if(p.startsWith('/api/')){res.writeHead(404);return res.end();}
 const file=path.join(repo,'internal/dashboard/page',p==='/'?'index.html':p);
 const content=await fs.readFile(file);
 res.writeHead(200,{'Content-Type':file.endsWith('.js')?'application/javascript':file.endsWith('.css')?'text/css':'text/html','Content-Security-Policy':"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; font-src 'self'; img-src 'self'; frame-ancestors 'none'"});res.end(content);
 }catch(e){res.writeHead(500);res.end('Synthetic fixture error');console.error(e);}
});
await new Promise(r=>server.listen(0,'127.0.0.1',r));
const base=`http://127.0.0.1:${server.address().port}`;
const browser=await chromium.launch({executablePath:process.env.HATAORI_TEST_CHROME || 'google-chrome',headless:true,args:['--no-sandbox','--disable-background-networking']});
const allMeasurements=[];
const css=process.env.CAPTURE_CSS?await fs.readFile(process.env.CAPTURE_CSS,'utf8'):'';
const views=['details','composer','tool-detail','long-question','long-trusts','filter','list','running','conversation','question','approval','asleep','failed','startup','idle','creating','closed','removed','host','console','passkeys','trusts','stop','split2','split3','tab-menu','no-panels','empty-filter','empty','signed-out'];
try{
for(const width of (process.env.CAPTURE_WIDTHS||'1440,390').split(',').map(Number))for(const theme of (process.env.CAPTURE_THEMES||'light,dark').split(','))for(const lang of (process.env.CAPTURE_LANGS||'en').split(',')){
 const context=await browser.newContext({viewport:{width,height:width===390?844:900},colorScheme:theme,locale:lang==='ja'?'ja-JP':'en-US',timezoneId:'UTC'});
 await context.addInitScript(({theme})=>{localStorage.setItem('hataori-session','synthetic-session');localStorage.setItem('hataori.theme',theme);localStorage.setItem('hataori.show','all');localStorage.setItem('hataori.layout',JSON.stringify({tree:true,details:true}));localStorage.removeItem('hataori.tree.folds');Date.now=()=>Date.parse('2026-11-01T10:05:00Z');},{theme});
 await context.route('**/*',route=>route.request().url().startsWith(base)?route.continue():route.abort());
 const page=await context.newPage();
 page.on('pageerror',e=>console.error('PAGE ERROR',e.message));
 async function open(n){
  if(width===390){const rest=page.locator('[data-section=rest]');if(await rest.count())await rest.evaluate(e=>e.open=true);}
  const row=page.locator(width===390?`.narrow-list [data-thread="${n}"]`:`.tree [data-thread="${n}"]`).first();
  if(!(await row.count())&&width>720){for(const e of await page.locator('.archived-row').all())await e.click();}await row.waitFor();
  await row.click();await page.waitForSelector(`[data-details="${n}"]`);await page.waitForTimeout(120);
 }
 for(const name of views){
  if(chosen&&!chosen.includes(name))continue;
  if(width===390&&['split2','split3','tab-menu','no-panels','empty-filter'].includes(name))continue;
  scenario=name;
  await page.evaluate(()=>history.replaceState(null,''));await page.goto(base+'/?lang='+lang);await page.waitForSelector('.status');if(!['empty','signed-out'].includes(name))await page.waitForSelector('[data-thread="1"]');await page.waitForTimeout(180);
  // Identifiers are omitted only in documentation captures, after measurement.
  if(css)await page.addStyleTag({content:css});
  const ids={running:1,conversation:1,question:2,approval:10,asleep:4,failed:3,startup:9,idle:5,creating:6,closed:7,removed:8,stop:1,'long-question':2,details:5,composer:4,'tool-detail':4};
  if(ids[name])await open(ids[name]);
  if(name==='details')await page.locator('[data-block=events]').scrollIntoViewIfNeeded();
  if(name==='composer')await page.locator('.input-box textarea').scrollIntoViewIfNeeded();
  if(name==='tool-detail')await page.locator('.conv-tool details').evaluate(e=>e.open=true);
  if(name==='conversation')await page.locator('.view-switch [data-view=conversation]').click();
  if(name==='failed'){await page.locator('[data-block=failure] details').evaluate(e=>e.open=true);}
  if(['host','console'].includes(name)){
    if(width===390){await page.locator(`[data-section=${name==='host'?'tmux':'others'}]`).evaluate(e=>e.open=true);}
    if(width>720)await page.locator('[data-tree-item=host] > .node-head').click();
    await page.locator(`.tree [data-host="${name==='host'?'demo-terminal':'demo-console'}"]`).first().click();await page.waitForTimeout(180);
  }
  if(name==='stop')await page.locator('[data-stop-button]').click();
  if(name==='passkeys'||name==='trusts'||name==='long-trusts')await page.locator(`[data-tool=${name==='long-trusts'?'trusts':name}]`).click();
  if(name==='filter')await page.locator('.show [data-choice=active]').click();
  if(['split2','split3','tab-menu','no-panels'].includes(name)){
   for(const n of [1,2,3,5])await open(n);
   if(name.startsWith('split')){await page.locator('[data-tool=split]').click();if(name==='split3')await page.locator('[data-tool=split]').click();}
   if(name==='tab-menu')await page.locator('[data-tool=tab-list]').click();
   if(name==='no-panels'){await page.locator('[data-tool=tree]').click();await page.locator('[data-tool=details]').click();}
  }
  if(name==='empty-filter')await page.locator('.filter').fill('No matching fictional thread');
  await page.waitForTimeout(140);
  const metrics=await page.evaluate(()=>{
   const rect=s=>{const e=document.querySelector(s);return e?{width:e.getBoundingClientRect().width,height:e.getBoundingClientRect().height,scrollWidth:e.scrollWidth,clientWidth:e.clientWidth}:null;};
   const touch=[...document.querySelectorAll('button,summary,.narrow-list .row')].filter(e=>e.getBoundingClientRect().height>0).map(e=>({class:e.className,height:e.getBoundingClientRect().height,width:e.getBoundingClientRect().width}));
   const inputs=[...document.querySelectorAll('.box-row textarea,.answers-field textarea')].map(e=>({font:getComputedStyle(e).fontSize}));
   return {inputs,filter:rect('.narrow-show .show'),question:rect('.details-top'),answer:rect('.answers-text'),dialog:rect('dialog[open]'),tableTitle:rect('td[data-col="title"]'),body:rect('body'),shell:rect('.shell'),bar:rect('.tabbar'),pane:rect('.pane'),status:rect('.status'),controls:touch};
  });
  allMeasurements.push({name,width,theme,lang,...metrics});
  await page.addStyleTag({content:'.num, [data-col="number"], .device, [data-field="terminal"] dd, [data-field="pane"] dd {visibility:hidden!important}'});
  await page.evaluate(()=>{const w=document.createTreeWalker(document.body,NodeFilter.SHOW_TEXT);let n;while(n=w.nextNode())n.textContent=n.textContent.replace(/#\d+/g,'').replace(/(?:last )?event \d+/g,'events').replace(/イベント \d+/g,'イベント').replace(/demo-terminal|demo-console|demo-pane/g,'Demo preview');});
  const filename=`${width}-${theme}-${lang}-${name}.png`;
  await page.screenshot({path:path.join(output,filename),fullPage:false});
  console.log(filename);
 }
 await context.close();
}
await fs.writeFile(path.join(output,'measurements.json'),JSON.stringify(allMeasurements,null,2)+'\n');
}finally{await browser.close();server.closeAllConnections();await new Promise(r=>server.close(r));}
