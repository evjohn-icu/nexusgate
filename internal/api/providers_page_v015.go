package api

// The channel manager is the only place a Provider key is ever entered, so the
// page is written against three boundaries that are easy to lose in an edit:
//
//   - A key exists in the browser only as the live value of a password input.
//     It is never stored (no localStorage/sessionStorage), never placed in a
//     URL or query string, never logged, and never rendered back: the admin
//     API blanks secret_ref and returns no api_key, so a re-render after a
//     successful write cannot echo one. Every key input is cleared on success.
//   - PATCH /api/v1/admin/provider-channels/{id} replaces the whole member set
//     (app.UpdateProviderChannel assigns channel.Members from the patch), and a
//     member is matched to its stored secret by id first, label second. So the
//     add/remove affordances resend *every* surviving member with its id and
//     without api_key — that is what preserves the already-stored keys of the
//     other members. Sending only the new member would drop the rest.
//   - Two members of one channel must not share a label. A new member carrying
//     an existing member's label is matched by that label on the server, which
//     would overwrite the older member's secret before the duplicate member id
//     fails the write. The duplicate check below keeps that input out.
const providersHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Timingdex · 能力与服务</title>
<style>
:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#0c1422;color:#edf3ff;font:15px ui-sans-serif,system-ui,sans-serif}
header{display:flex;gap:18px;align-items:center;padding:15px 5vw;border-bottom:1px solid #283a57}.brand{font-weight:850;color:#fff;margin-right:auto}a{color:#b9ccf5;text-decoration:none}
.wrap{max-width:1180px;margin:auto;padding:38px 24px}.eyebrow{font-size:12px;font-weight:800;letter-spacing:.1em;color:#96aeff}h1{font-size:42px;letter-spacing:-.05em;margin:8px 0}.muted{color:#a9b9d5;line-height:1.55}
.auth,.panel,.card{background:#121f34;border:1px solid #2b405e;border-radius:14px}.auth,.panel{padding:18px}.auth{display:flex;gap:10px;align-items:end;margin:22px 0}.field{flex:1}label{display:block;font-size:12px;font-weight:800;color:#cbd8f2;margin:0 0 6px}
input,select{width:100%;background:#091322;border:1px solid #40577a;border-radius:8px;color:#fff;padding:10px;font:inherit}button{border:0;border-radius:8px;padding:10px 13px;background:#8ca6ff;color:#081225;font-weight:850;cursor:pointer}
button.ghost{background:transparent;border:1px solid #40577a;color:#c9d9ff;padding:6px 10px;font-size:12px;font-weight:800}button.danger{background:transparent;border:1px solid #7a4058;color:#ffc0ca;padding:5px 9px;font-size:12px;font-weight:800}
.layout{display:grid;grid-template-columns:1fr 1.4fr;gap:16px}.formgrid{display:grid;grid-template-columns:1fr 1fr;gap:12px}.wide{grid-column:1/-1}.cards{display:grid;gap:10px}.card{padding:15px}.cardtop{display:flex;justify-content:space-between;gap:12px;align-items:center}.pill{display:inline-block;border-radius:99px;padding:4px 8px;background:#223855;color:#c9d9ff;font-size:11px}.ok{background:#164936;color:#a2efc6}.bad{background:#552d39;color:#ffc0ca}.member{margin-top:9px;padding-top:9px;border-top:1px solid #29405d;color:#aebfdd;font-size:12px}.memberline{display:flex;gap:10px;align-items:center;justify-content:space-between}.notice{margin-top:18px;border-left:3px solid #8da7ff;padding:12px 14px;background:#172744;color:#cbd8f2}.error{color:#ffadba}.empty{padding:25px;text-align:center;color:#a8b9d4;border:1px dashed #3c5272;border-radius:12px}
.keyhead{display:flex;justify-content:space-between;align-items:center;margin:16px 0 8px}.keyhead b{font-size:12px;font-weight:800;letter-spacing:.1em;color:#96aeff}.memberrow{border:1px solid #2b405e;border-radius:10px;background:#0e1a2c;padding:12px;margin-bottom:10px}.rowtop{display:flex;justify-content:space-between;align-items:center;margin-bottom:9px}.rowtop b{font-size:12px;color:#cbd8f2}.addkey{margin-top:12px;padding-top:11px;border-top:1px solid #29405d}.addform{margin-top:10px}[hidden]{display:none}
@media(max-width:760px){.layout,.formgrid{grid-template-columns:1fr}.auth{display:block}.auth button{margin-top:10px}.wrap{padding:28px 18px}}
</style></head>
<body><!--SHELL_HEADER-->
<main class="wrap"><div class="eyebrow">MODEL CHANNELS</div><h1>能力与服务</h1>
<p class="muted">按能力管理模型通道。同一通道可以配置多个 Key：运行时按并发最少优先轮换，某个 Key 的额度用尽或临时失败时会降级到同通道内的其他 Key。</p>
<section class="auth"><div class="field"><label for="admin-token">Hub 管理 Token（只保留在当前页面内存）</label><input id="admin-token" type="password" autocomplete="off" placeholder="输入后加载和修改模型通道"></div><button onclick="loadChannels()">加载通道</button></section>
<div class="layout"><section class="panel"><h2>新增模型通道</h2>
<form id="channel-form" onsubmit="saveChannel(event)"><div class="formgrid">
<div><label for="capability">能力</label><select id="capability"><option value="video_analysis">Video analysis</option><option value="asr">ASR</option><option value="embedding">Embedding</option><option value="tag_curator">Tag curation</option><option value="repurpose">Repurpose</option></select></div>
<div><label for="provider-name">Provider</label><select id="provider-name"></select></div>
<div><label for="label">通道名称</label><input id="label" required placeholder="例如 Gemini 主账号"></div><div><label for="model">模型</label><input id="model" placeholder="模型 ID"></div>
<div class="wide"><label for="endpoint">Endpoint</label><input id="endpoint" placeholder="https://…"></div>
</div>
<div class="keyhead"><b>KEY 列表</b><button type="button" class="ghost" onclick="addMemberRow()">+ 再加一个 Key</button></div>
<div id="member-rows"><div class="memberrow"><div class="rowtop"><b>默认 Key</b></div><div class="formgrid">
<div><label for="member-label">Key 名称</label><input id="member-label" class="m-label" value="primary" required></div><div><label for="api-key">API Key</label><input id="api-key" class="m-key" type="password" autocomplete="new-password" required></div>
<div><label for="weight">权重</label><input id="weight" class="m-weight" type="number" min="1" value="1"></div><div><label for="max-inflight">并发上限</label><input id="max-inflight" class="m-inflight" type="number" min="1" value="1"></div>
</div></div></div>
<button type="submit">保存到 Hub 密钥库</button><p id="form-status" class="muted"></p></form>
<div class="notice">API Key 通过管理员 HTTPS 请求写入 Hub 加密密钥库；页面不会回显，也不会写进数据库、浏览器存储或 Worker 配置。已存在的通道可以直接在右侧追加或移除 Key，无需重建通道。</div>
</section><section class="panel"><div class="cardtop"><h2>已配置通道</h2><span class="pill">Video analysis · ASR · Embedding · Tag curation · Repurpose</span></div><p id="channels-status" class="muted"></p><div id="channels" class="empty">输入 Hub 管理 Token 后加载。</div></section></div></main>
<script>
const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const providerOptions={video_analysis:[['gemini','Gemini'],['qwen_video','千问视频'],['volcengine_video','火山视频'],['local_vlm','本地 VLM']],asr:[['stepfun','StepFun'],['qwen','千问 ASR'],['volcengine_asr','火山 ASR']],embedding:[['openai_embeddings','OpenAI-compatible Embeddings'],['gemini_embed_content','Gemini Embeddings'],['volc_agent_plan_embedding','火山 Agent Plan Embeddings'],['volc_coding_plan_embedding','火山 Coding Plan Embeddings']],tag_curator:[['openai_chat','OpenAI-compatible Chat'],['volc_agent_plan','火山 Agent Plan'],['volc_coding_plan','火山 Coding Plan']],repurpose:[['openai_chat','OpenAI-compatible Chat']]};
let channelCache=[];
function syncProviderOptions(){const capability=document.getElementById('capability').value;const select=document.getElementById('provider-name');const prior=select.value;select.innerHTML=(providerOptions[capability]||[]).map(([value,label])=>'<option value="'+value+'">'+label+'</option>').join('');if([...select.options].some(o=>o.value===prior))select.value=prior}
function adminHeaders(json=false){const token=document.getElementById('admin-token').value.trim();const headers={Authorization:'Bearer '+token};if(json)headers['Content-Type']='application/json';return headers}
async function json(r){if(!r.ok)throw Error(await r.text());return r.status===204?null:r.json()}
function duplicateLabel(labels){const seen=new Set();for(const raw of labels){const key=String(raw||'').trim().toLowerCase();if(!key)continue;if(seen.has(key))return raw;seen.add(key)}return ''}
function keyFields(prefix){return '<div class="formgrid"><div><label>Key 名称</label><input class="'+prefix+'label" value="" required></div><div><label>API Key</label><input class="'+prefix+'key" type="password" autocomplete="new-password" required></div><div><label>权重</label><input class="'+prefix+'weight" type="number" min="1" value="1"></div><div><label>并发上限</label><input class="'+prefix+'inflight" type="number" min="1" value="1"></div></div>'}
function addMemberRow(){const rows=document.getElementById('member-rows');const row=document.createElement('div');row.className='memberrow';row.innerHTML='<div class="rowtop"><b>追加 Key</b><button type="button" class="danger rowremove">移除这一行</button></div>'+keyFields('m-');rows.appendChild(row);const label=row.querySelector('.m-label');label.value='key-'+rows.children.length;row.querySelector('.rowremove').addEventListener('click',()=>row.remove());label.focus()}
function formMembers(){return [...document.querySelectorAll('#member-rows .memberrow')].map(row=>({label:row.querySelector('.m-label').value.trim(),api_key:row.querySelector('.m-key').value.trim(),enabled:true,weight:Number(row.querySelector('.m-weight').value)||1,max_inflight:Number(row.querySelector('.m-inflight').value)||1}))}
function clearFormKeys(){document.querySelectorAll('#member-rows .m-key').forEach(input=>{input.value=''});[...document.querySelectorAll('#member-rows .memberrow')].slice(1).forEach(row=>row.remove())}
function memberPatches(channel){return (channel.members||[]).map(m=>({id:m.id,label:m.label,enabled:m.enabled!==false,weight:Number(m.weight)||1,max_inflight:Number(m.max_inflight)||1}))}
function channelStatus(text){document.getElementById('channels-status').textContent=text}
async function patchMembers(id,members){return fetch('/api/v1/admin/provider-channels/'+encodeURIComponent(id),{method:'PATCH',headers:adminHeaders(true),body:JSON.stringify({members})}).then(json)}
function renderChannels(items){channelCache=items;const el=document.getElementById('channels');if(!items.length){el.className='empty';el.textContent='还没有通道。左侧新增后会显示在这里。';return}el.className='cards';el.innerHTML=items.map(c=>'<article class="card" data-channel="'+esc(c.id)+'"><div class="cardtop"><div><b>'+esc(c.label)+'</b><div class="muted">'+esc(c.capability)+' · '+esc(c.provider_name)+' · '+esc(c.model||'未指定模型')+'</div></div><span class="pill '+(c.enabled?'ok':'bad')+'">'+(c.enabled?'已启用':'已停用')+'</span></div>'+(c.members||[]).map(m=>'<div class="member"><div class="memberline"><span>'+esc(m.label)+' · 权重 '+Number(m.weight||1)+' · 并发 '+Number(m.max_inflight||1)+' · '+(m.secret_ready?'Key 已配置':'缺少 Key')+'</span><button type="button" class="danger" data-act="remove-member" data-member="'+esc(m.id)+'">移除</button></div></div>').join('')+'<div class="addkey"><button type="button" class="ghost" data-act="toggle-add">+ 添加 Key</button><div class="addform" hidden>'+keyFields('a-')+'<button type="button" data-act="save-key">写入 Hub 密钥库</button> <button type="button" class="ghost" data-act="cancel-key">取消</button></div><p class="muted cardstatus"></p></div></article>').join('')}
async function loadChannels(){channelStatus('');try{const items=await fetch('/api/v1/admin/provider-channels',{headers:adminHeaders()}).then(json);renderChannels(items||[])}catch(e){channelCache=[];const el=document.getElementById('channels');el.className='empty error';el.textContent='无法加载：'+e.message}}
async function saveChannel(event){event.preventDefault();const status=document.getElementById('form-status');const members=formMembers();const dup=duplicateLabel(members.map(m=>m.label));if(dup){status.textContent='Key 名称重复：'+dup+'；同一通道内每个 Key 需要不同名称。';return}status.textContent='正在安全保存…';const body={capability:document.getElementById('capability').value,label:document.getElementById('label').value.trim(),provider_name:document.getElementById('provider-name').value,endpoint:document.getElementById('endpoint').value.trim(),model:document.getElementById('model').value.trim(),enabled:true,route_order:0,members};try{await fetch('/api/v1/admin/provider-channels',{method:'POST',headers:adminHeaders(true),body:JSON.stringify(body)}).then(json);clearFormKeys();status.textContent='已保存；Key 不会再次显示。';await loadChannels()}catch(e){status.textContent='保存失败：'+e.message}}
async function addChannelKey(card,channel){const form=card.querySelector('.addform');const status=card.querySelector('.cardstatus');const label=form.querySelector('.a-label').value.trim();const key=form.querySelector('.a-key').value.trim();if(!label||!key){status.textContent='请填写 Key 名称和 API Key。';return}const members=memberPatches(channel);const dup=duplicateLabel(members.map(m=>m.label).concat([label]));if(dup){status.textContent='Key 名称重复：'+dup+'；同一通道内每个 Key 需要不同名称。';return}members.push({label,api_key:key,enabled:true,weight:Number(form.querySelector('.a-weight').value)||1,max_inflight:Number(form.querySelector('.a-inflight').value)||1});status.textContent='正在安全写入…';try{await patchMembers(channel.id,members);form.querySelector('.a-key').value='';await loadChannels();channelStatus('已写入 Key「'+label+'」；Key 不会再次显示。')}catch(e){status.textContent='添加失败：'+e.message}}
async function removeChannelKey(card,channel,memberID){const status=card.querySelector('.cardstatus');const members=memberPatches(channel).filter(m=>m.id!==memberID);if(!members.length){status.textContent='通道至少需要保留一个 Key；如果整个通道不再使用，请停用或删除通道。';return}if(!confirm('移除后这个 Key 立即退出调度，且不会再显示。确定移除？'))return;status.textContent='正在移除…';try{await patchMembers(channel.id,members);await loadChannels();channelStatus('已移除该 Key，其余 Key 保持不变。')}catch(e){status.textContent='移除失败：'+e.message}}
document.getElementById('channels').addEventListener('click',event=>{const button=event.target.closest('button[data-act]');if(!button)return;const card=button.closest('.card');const channel=channelCache.find(c=>c.id===card.dataset.channel);if(!channel)return;const form=card.querySelector('.addform');const action=button.dataset.act;if(action==='toggle-add'){form.hidden=!form.hidden;if(form.hidden){form.querySelector('.a-key').value=''}else{form.querySelector('.a-label').value='key-'+((channel.members||[]).length+1);form.querySelector('.a-label').focus()}return}if(action==='cancel-key'){form.querySelector('.a-key').value='';form.hidden=true;card.querySelector('.cardstatus').textContent='';return}if(action==='save-key'){addChannelKey(card,channel);return}if(action==='remove-member'){removeChannelKey(card,channel,button.dataset.member)}});
document.getElementById('capability').addEventListener('change',syncProviderOptions);syncProviderOptions();
</script></body></html>`
