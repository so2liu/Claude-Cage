// Real settings interactions against isolated API fixtures; no user profile is used.
import assert from 'node:assert/strict';
import { createWorkbench, chooseInline } from './workbench-fixture.mjs';
const policies = {
 first: { mode:'static',scope:'country',ip:'1.1.1.1' },
 second: { mode:'static',scope:'country',ip:'8.8.4.4' },
};
const proxy = {
 sources:[{id:'home',name:'住宅代理',nodes:[{id:'first',name:'住宅节点001',type:'socks5'},{id:'second',name:'住宅节点003',type:'socks5'}]}],
 selected:'first',route:['first'],active:true,running:true,core_available:true,port:17896,state_label:'待启动环境',route_error:'',
};
let fail = false, sequence = 0;
const f = await createWorkbench({beforeNavigate: async (page,state)=>{
 state.settings.exit_policy = {...policies.first};
 await page.route('**/api/proxy', r=>r.fulfill({json:proxy}));
 await page.route('**/api/proxy/route', r=>{
  const ids=r.request().postDataJSON().node_ids;
  if (!fail) {
   proxy.selected=ids.at(-1); proxy.route=ids;
   state.settings.exit_policy={...policies[proxy.selected]};
  }
  state.status.job={id:`route-${++sequence}`,kind:'proxy',running:false,error:fail?'代理启动失败；原节点已保留':'',message:'节点已应用'};
  return r.fulfill({json:{id:state.status.job.id}});
 });
}});
try {
 const {page,state}=f;
 await page.getByRole('button',{name:/^设置/}).click();
 await page.getByRole('button',{name:'网络与代理',exact:true}).click();
 const ip=page.getByLabel('绑定的 IPv4 地址',{exact:true});
 assert.equal(await ip.inputValue(),policies.first.ip);
 const dns=page.getByLabel('加密 DNS',{exact:true});
 await dns.fill('https://dns.google/dns-query');
 const select=async(id)=>{
  await chooseInline(page.getByRole('combobox',{name:'最终出口',exact:true}),id);
  await page.getByRole('button',{name:'应用代理',exact:true}).click();
  await page.waitForFunction(()=>!document.querySelector('.proxy-progress'));
 };
 await select('second');
 await page.waitForFunction(ip=>document.querySelector('.network-exit-policy input[spellcheck="false"]')?.value===ip,policies.second.ip);
 assert.equal(await ip.inputValue(),policies.second.ip,'settings retained the previous node IP');
 assert.equal(await dns.inputValue(),'https://dns.google/dns-query','node selection discarded unrelated settings');
 await select('first');
 await page.waitForFunction(ip=>document.querySelector('.network-exit-policy input[spellcheck="false"]')?.value===ip,policies.first.ip);
 fail=true;
 await select('second');
 await page.getByRole('alert').filter({hasText:'代理启动失败'}).waitFor();
 assert.equal(await ip.inputValue(),policies.first.ip,'failed node switch replaced the binding');
 assert.equal(await dns.inputValue(),'https://dns.google/dns-query');
 assert.equal(state.settings.exit_policy.ip,policies.first.ip);
 assert.deepEqual(f.errors,[]);
 console.log(JSON.stringify({passed:true,checks:['node switch updates bound IP','switch back restores IP','unsaved DNS preserved','failed switch preserves binding'],browserErrors:f.errors}));
} finally { await f.browser.close(); }
