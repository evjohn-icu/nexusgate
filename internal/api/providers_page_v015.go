package api

const providersHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Timingdex · 能力与服务</title>
<style>
:root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;background:#0c1422;color:#edf3ff;font:15px ui-sans-serif,system-ui,sans-serif}
header{display:flex;gap:18px;align-items:center;padding:15px 5vw;border-bottom:1px solid #283a57}.brand{font-weight:850;color:#fff;margin-right:auto}a{color:#b9ccf5;text-decoration:none}
.wrap{max-width:1180px;margin:auto;padding:38px 24px}.eyebrow{font-size:12px;font-weight:800;letter-spacing:.1em;color:#96aeff}h1{font-size:42px;letter-spacing:-.05em;margin:8px 0}.muted{color:#a9b9d5;line-height:1.55}
.auth,.panel,.card{background:#121f34;border:1px solid #2b405e;border-radius:14px}.auth,.panel{padding:18px}.auth{display:flex;gap:10px;align-items:end;margin:22px 0}.field{flex:1}label{display:block;font-size:12px;font-weight:800;color:#cbd8f2;margin:0 0 6px}
input,select{width:100%;background:#091322;border:1px solid #40577a;border-radius:8px;color:#fff;padding:10px;font:inherit}button{border:0;border-radius:8px;padding:10px 13px;background:#8ca6ff;color:#081225;font-weight:850;cursor:pointer}
.layout{display:grid;grid-template-columns:1fr 1.4fr;gap:16px}.formgrid{display:grid;grid-template-columns:1fr 1fr;gap:12px}.wide{grid-column:1/-1}.cards{display:grid;gap:10px}.card{padding:15px}.cardtop{display:flex;justify-content:space-between;gap:12px}.pill{display:inline-block;border-radius:99px;padding:4px 8px;background:#223855;color:#c9d9ff;font-size:11px}.ok{background:#164936;color:#a2efc6}.bad{background:#552d39;color:#ffc0ca}.member{margin-top:9px;padding-top:9px;border-top:1px solid #29405d;color:#aebfdd;font-size:12px}.notice{margin-top:18px;border-left:3px solid #8da7ff;padding:12px 14px;background:#172744;color:#cbd8f2}.error{color:#ffadba}.empty{padding:25px;text-align:center;color:#a8b9d4;border:1px dashed #3c5272;border-radius:12px}
@media(max-width:760px){.layout,.formgrid{grid-template-columns:1fr}.auth{display:block}.auth button{margin-top:10px}.wrap{padding:28px 18px}}
</style></head>
<body><header><span class="brand">Timingdex</span><a href="/">素材库</a><a href="/setup">启动配置</a><a href="/progress">处理进度</a></header>
<main class="wrap"><div class="eyebrow">MODEL CHANNELS</div><h1>能力与服务</h1>
<p class="muted">按能力管理模型通道。同一 Provider 可以添加多个 Key；运行时优先使用健康通道，临时失败会轮换并降级。</p>
<section class="auth"><div class="field"><label for="admin-token">Hub 管理 Token（只保留在当前页面内存）</label><input id="admin-token" type="password" autocomplete="off" placeholder="输入后加载和修改模型通道"></div><button onclick="loadChannels()">加载通道</button></section>
<div class="layout"><section class="panel"><h2>新增模型通道</h2>
<form id="channel-form" onsubmit="saveChannel(event)"><div class="formgrid">
<div><label for="capability">能力</label><select id="capability"><option value="video_analysis">Video analysis</option><option value="asr">ASR</option><option value="embedding">Embedding</option><option value="tag_curator">Tag curation</option><option value="repurpose">Repurpose</option></select></div>
<div><label for="provider-name">Provider</label><select id="provider-name"></select></div>
<div><label for="label">通道名称</label><input id="label" required placeholder="例如 Gemini 主账号"></div><div><label for="model">模型</label><input id="model" placeholder="模型 ID"></div>
<div class="wide"><label for="endpoint">Endpoint</label><input id="endpoint" placeholder="https://…"></div>
<div><label for="member-label">Key 名称</label><input id="member-label" value="primary" required></div><div><label for="api-key">API Key</label><input id="api-key" type="password" autocomplete="new-password" required></div>
<div><label for="weight">权重</label><input id="weight" type="number" min="1" value="1"></div><div><label for="max-inflight">并发上限</label><input id="max-inflight" type="number" min="1" value="1"></div>
</div><button type="submit">保存到 Hub 密钥库</button><p id="form-status" class="muted"></p></form>
<div class="notice">API Key 通过管理员 HTTPS 请求写入 Hub 加密密钥库；页面不会回显，也不会写进数据库、浏览器存储或 Worker 配置。</div>
</section><section class="panel"><div class="cardtop"><h2>已配置通道</h2><span class="pill">Video analysis · ASR · Embedding · Tag curation · Repurpose</span></div><div id="channels" class="empty">输入 Hub 管理 Token 后加载。</div></section></div></main>
<script>
const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const providerOptions={video_analysis:[['gemini','Gemini'],['qwen_video','千问视频'],['volcengine_video','火山视频'],['local_vlm','本地 VLM']],asr:[['stepfun','StepFun'],['qwen','千问 ASR'],['volcengine_asr','火山 ASR']],embedding:[['openai_embeddings','OpenAI-compatible Embeddings'],['gemini_embed_content','Gemini Embeddings'],['volc_agent_plan_embedding','火山 Agent Plan Embeddings'],['volc_coding_plan_embedding','火山 Coding Plan Embeddings']],tag_curator:[['openai_chat','OpenAI-compatible Chat'],['volc_agent_plan','火山 Agent Plan'],['volc_coding_plan','火山 Coding Plan']],repurpose:[['openai_chat','OpenAI-compatible Chat']]};
function syncProviderOptions(){const capability=document.getElementById('capability').value;const select=document.getElementById('provider-name');const prior=select.value;select.innerHTML=(providerOptions[capability]||[]).map(([value,label])=>'<option value="'+value+'">'+label+'</option>').join('');if([...select.options].some(o=>o.value===prior))select.value=prior}
function adminHeaders(json=false){const token=document.getElementById('admin-token').value.trim();const headers={Authorization:'Bearer '+token};if(json)headers['Content-Type']='application/json';return headers}
async function json(r){if(!r.ok)throw Error(await r.text());return r.status===204?null:r.json()}
function renderChannels(items){const el=document.getElementById('channels');if(!items.length){el.className='empty';el.textContent='还没有通道。左侧新增后会显示在这里。';return}el.className='cards';el.innerHTML=items.map(c=>'<article class="card"><div class="cardtop"><div><b>'+esc(c.label)+'</b><div class="muted">'+esc(c.capability)+' · '+esc(c.provider_name)+' · '+esc(c.model||'未指定模型')+'</div></div><span class="pill '+(c.enabled?'ok':'bad')+'">'+(c.enabled?'已启用':'已停用')+'</span></div>'+(c.members||[]).map(m=>'<div class="member">'+esc(m.label)+' · 权重 '+Number(m.weight||1)+' · 并发 '+Number(m.max_inflight||1)+' · '+(m.secret_ready?'Key 已配置':'缺少 Key')+'</div>').join('')+'</article>').join('')}
async function loadChannels(){try{const items=await fetch('/api/v1/admin/provider-channels',{headers:adminHeaders()}).then(json);renderChannels(items||[])}catch(e){const el=document.getElementById('channels');el.className='empty error';el.textContent='无法加载：'+e.message}}
async function saveChannel(event){event.preventDefault();const status=document.getElementById('form-status');status.textContent='正在安全保存…';const body={capability:document.getElementById('capability').value,label:document.getElementById('label').value.trim(),provider_name:document.getElementById('provider-name').value,endpoint:document.getElementById('endpoint').value.trim(),model:document.getElementById('model').value.trim(),enabled:true,route_order:0,members:[{label:document.getElementById('member-label').value.trim(),api_key:document.getElementById('api-key').value.trim(),enabled:true,weight:Number(document.getElementById('weight').value)||1,max_inflight:Number(document.getElementById('max-inflight').value)||1}]};try{await fetch('/api/v1/admin/provider-channels',{method:'POST',headers:adminHeaders(true),body:JSON.stringify(body)}).then(json);document.getElementById('api-key').value='';status.textContent='已保存；Key 不会再次显示。';await loadChannels()}catch(e){status.textContent='保存失败：'+e.message}}
document.getElementById('capability').addEventListener('change',syncProviderOptions);syncProviderOptions();
</script></body></html>`
