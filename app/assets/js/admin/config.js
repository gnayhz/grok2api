let apiKey = '';
let currentConfig = {};
const byId = id => document.getElementById(id);

const NUMERIC_FIELDS = new Set([
  'timeout','max_retry','retry_backoff_base','retry_backoff_factor','retry_backoff_max',
  'retry_budget','refresh.interval_hours','refresh.super_interval_hours','runtime.fail_threshold',
  'limit_mb','refresh.on_demand_min_interval_sec','refresh.on_demand_max_tokens',
  'upload_concurrent','upload_timeout','download_concurrent','download_timeout',
  'list_concurrent','list_timeout','list_batch_size','delete_concurrent','delete_timeout',
  'delete_batch_size','reload_interval_sec','stream_timeout','final_timeout',
  'blocked_grace_seconds','final_min_bytes','medium_min_bytes','blocked_parallel_attempts',
  'concurrent','batch_size','max_file_size_mb','max_files','request_slow_ms'
]);

const LOCALE_MAP = {
  app:{label:"应用设置",api_key:{title:"API 密钥",desc:"调用 Grok2API 服务的 Token。"},app_key:{title:"后台密码",desc:"登录管理后台的密码（必填）。"},function_enabled:{title:"启用功能玩法",desc:"是否启用功能玩法入口。"},function_key:{title:"Function 密码",desc:"功能玩法页面的访问密码。"},app_url:{title:"应用地址",desc:"服务外部访问 URL。"},image_format:{title:"图片格式",desc:"默认图片格式。"},video_format:{title:"视频格式",desc:"默认视频格式。"},temporary:{title:"临时对话",desc:"默认启用临时对话。"},disable_memory:{title:"禁用记忆",desc:"默认禁用 Grok 记忆。"},stream:{title:"流式响应",desc:"默认启用流式输出。"},thinking:{title:"思维链",desc:"默认启用思维链。"},dynamic_statsig:{title:"动态指纹",desc:"动态生成 Statsig 指纹。"},custom_instruction:{title:"自定义指令",desc:"透传为 customPersonality。"},filter_tags:{title:"过滤标签",desc:"自动过滤响应中的特殊标签。"}},
  proxy:{label:"代理配置",base_proxy_url:{title:"基础代理 URL",desc:"代理到 Grok 的基础地址。"},asset_proxy_url:{title:"资源代理 URL",desc:"代理到 Grok 的资源地址。"},skip_proxy_ssl_verify:{title:"跳过 SSL 校验",desc:"代理自签名证书时启用。"},enabled:{title:"Managed Clearance",desc:"通过 FlareSolverr 维护 CF clearance。"},flaresolverr_url:{title:"FlareSolverr 地址",desc:"Managed clearance provider 地址。"},refresh_interval:{title:"预热间隔（秒）",desc:"预热间隔，建议≥300。"},timeout:{title:"挑战超时（秒）",desc:"等待解决挑战的最大时间。"},cf_clearance:{title:"CF Clearance",desc:"手动配置的 clearance cookie。"},browser:{title:"浏览器指纹",desc:"curl_cffi 浏览器指纹。"},user_agent:{title:"User-Agent",desc:"HTTP User-Agent。"}},
  retry:{label:"重试策略",max_retry:{title:"最大重试",desc:"最大重试次数。"},retry_status_codes:{title:"重试状态码",desc:"触发重试的 HTTP 状态码。"},reset_session_status_codes:{title:"重建状态码",desc:"触发重建 session 的状态码。"},retry_backoff_base:{title:"退避基数",desc:"基础延迟（秒）。"},retry_backoff_factor:{title:"退避倍率",desc:"指数放大系数。"},retry_backoff_max:{title:"退避上限",desc:"单次最大延迟（秒）。"},retry_budget:{title:"退避预算",desc:"最大重试总耗时（秒）。"}},
  chat:{label:"对话配置",concurrent:{title:"并发上限",desc:"Reverse 并发上限。"},timeout:{title:"请求超时",desc:"超时（秒）。"},stream_timeout:{title:"流空闲超时",desc:"流空闲超时（秒）。"}},
  video:{label:"视频配置",enable_public_asset:{title:"公开资产链接",desc:"生成后创建 Public 资产。"},concurrent:{title:"并发上限",desc:"并发上限。"},timeout:{title:"请求超时",desc:"超时（秒）。"},stream_timeout:{title:"流空闲超时",desc:"流空闲超时（秒）。"},upscale_timing:{title:"超分时机",desc:"720p 超分模式。"}},
  image:{label:"图像配置",timeout:{title:"请求超时",desc:"WebSocket 超时（秒）。"},stream_timeout:{title:"流空闲超时",desc:"流空闲超时（秒）。"},final_timeout:{title:"最终图超时",desc:"等待最终图的超时。"},blocked_grace_seconds:{title:"审查宽限",desc:"判定审查的宽限秒数。"},nsfw:{title:"NSFW 模式",desc:"启用 NSFW。"},medium_min_bytes:{title:"中等图最小字节",desc:"中等图最小字节数。"},final_min_bytes:{title:"最终图最小字节",desc:"最终图最小字节数。"},blocked_parallel_enabled:{title:"并行补偿",desc:"审查时并行补偿。"},blocked_parallel_attempts:{title:"补偿次数",desc:"补偿并发次数。"}},
  imagine_fast:{label:"Imagine Fast",n:{title:"生成数量",desc:"生成数量。"},size:{title:"图片尺寸",desc:"尺寸。"},response_format:{title:"响应格式",desc:"返回格式。"}},
  asset:{label:"资产配置",upload_concurrent:{title:"上传并发",desc:"上传最大并发。"},upload_timeout:{title:"上传超时",desc:"超时（秒）。"},download_concurrent:{title:"下载并发",desc:"下载最大并发。"},download_timeout:{title:"下载超时",desc:"超时（秒）。"},list_concurrent:{title:"查询并发",desc:"查询最大并发。"},list_timeout:{title:"查询超时",desc:"超时（秒）。"},list_batch_size:{title:"查询批次",desc:"单次批次大小。"},delete_concurrent:{title:"删除并发",desc:"删除最大并发。"},delete_timeout:{title:"删除超时",desc:"超时（秒）。"},delete_batch_size:{title:"删除批次",desc:"单次批次大小。"}},
  voice:{label:"语音配置",timeout:{title:"请求超时",desc:"超时（秒）。"}},
  account:{label:"Account 配置","runtime.consumed_mode_enabled":{title:"消耗模式",desc:"按 consumed 选号。"},"runtime.fail_threshold":{title:"失败阈值",desc:"连续失败阈值。"},"refresh.enabled":{title:"刷新调度",desc:"启用周期 refresh。"},"refresh.interval_hours":{title:"普通池间隔",desc:"普通池刷新间隔（小时）。"},"refresh.super_interval_hours":{title:"Super 池间隔",desc:"Super 池间隔（小时）。"},"refresh.on_demand_enabled":{title:"按需刷新",desc:"允许按需 refresh。"},"refresh.on_demand_min_interval_sec":{title:"按需最小间隔",desc:"最小间隔（秒）。"},"refresh.on_demand_max_tokens":{title:"按需最大数量",desc:"最多检查账号数。"}},
  log:{label:"日志配置",max_file_size_mb:{title:"单文件上限",desc:"上限（MB）。"},max_files:{title:"保留数",desc:"最多保留日志文件数。"},log_health_requests:{title:"记录健康检查",desc:"记录 /health。"},log_all_requests:{title:"记录全部请求",desc:"全部请求日志。"},request_slow_ms:{title:"慢请求阈值",desc:"阈值（毫秒）。"}},
  cache:{label:"缓存管理",enable_auto_clean:{title:"自动清理",desc:"自动清理缓存。"},limit_mb:{title:"清理阈值",desc:"阈值（MB）。"}},
  nsfw:{label:"NSFW 配置",concurrent:{title:"并发上限",desc:"并发上限。"},batch_size:{title:"批次大小",desc:"批次大小。"},timeout:{title:"请求超时",desc:"超时（秒）。"}},
  usage:{label:"Usage 配置",concurrent:{title:"并发上限",desc:"并发上限。"},batch_size:{title:"批次大小",desc:"批次大小。"},timeout:{title:"请求超时",desc:"超时（秒）。"}}
};

const SECTION_ORDER = new Map(Object.keys(LOCALE_MAP).map((k,i) => [k,i]));
const MANAGED_LOCKED = ['cf_clearance','browser','user_agent'];
const MANAGED_PROVIDER = ['flaresolverr_url','refresh_interval','timeout'];

function getText(s, k) {
  let ti = t('config.fields.'+s+'.'+k+'.title');
  let de = t('config.fields.'+s+'.'+k+'.desc');
  if (!ti.startsWith('config.fields.')) return {title:ti, desc: de.startsWith('config.fields.') ? '' : de};
  const m = LOCALE_MAP[s]; return m?.[k] || {title: k.replace(/_/g,' '), desc:''};
}
function getSectionLabel(s) {
  let l = t('config.sections.'+s);
  return l.startsWith('config.sections.') ? (LOCALE_MAP[s]?.label || s) : l;
}
function sortByOrder(keys, order) {
  if (!order) return keys;
  return keys.sort((a,b) => {
    const ia = order.get(a), ib = order.get(b);
    if (ia !== undefined && ib !== undefined) return ia - ib;
    if (ia !== undefined) return -1;
    if (ib !== undefined) return 1;
    return 0;
  });
}

function setMeta(input, section, key) { input.dataset.section = section; input.dataset.key = key; }

function buildBool(s, k, val) {
  const label = document.createElement('label');
  label.className = 'toggle';
  const input = document.createElement('input');
  input.type = 'checkbox'; input.checked = val; input.className = 'sr-only';
  setMeta(input, s, k);
  const track = document.createElement('div');
  track.className = 'toggle-track';
  label.append(input, track);
  return {input, node: label};
}

function buildSelect(s, k, val, opts) {
  const input = document.createElement('select');
  input.className = 'input'; setMeta(input, s, k);
  opts.forEach(o => {
    const opt = document.createElement('option');
    opt.value = o.val; opt.text = o.text;
    if (val === o.val) opt.selected = true;
    input.appendChild(opt);
  });
  return {input, node: input};
}

function buildJson(s, k, val) {
  const input = document.createElement('textarea');
  input.className = 'input font-mono'; input.rows = 4;
  input.value = JSON.stringify(val, null, 2);
  setMeta(input, s, k); input.dataset.type = 'json';
  return {input, node: input};
}

function buildText(s, k, val) {
  const input = document.createElement('input');
  input.type = 'text'; input.className = 'input'; input.value = val;
  setMeta(input, s, k);
  return {input, node: input};
}

function buildTextarea(s, k, val, rows=5) {
  const input = document.createElement('textarea');
  input.className = 'input'; input.rows = rows; input.value = val||'';
  setMeta(input, s, k);
  return {input, node: input};
}

function randomKey(len) {
  const chars = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789';
  const buf = new Uint8Array(len);
  crypto.getRandomValues(buf);
  return Array.from(buf, b => chars[b % chars.length]).join('');
}

function buildSecret(s, k, val) {
  const input = document.createElement('input');
  input.type = 'text'; input.className = 'input'; input.value = val; input.style.flex = '1';
  setMeta(input, s, k);
  const wrap = document.createElement('div');
  wrap.className = 'flex gap-2';
  const genBtn = document.createElement('button');
  genBtn.className = 'btn btn-primary btn-icon'; genBtn.type = 'button'; genBtn.title = '生成';
  genBtn.innerHTML = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 12a9 9 0 1 1-3-6.7"/><polyline points="21 3 21 9 15 9"/></svg>';
  genBtn.onclick = () => { input.value = randomKey(16); };
  const copyBtn = document.createElement('button');
  copyBtn.className = 'btn btn-primary btn-icon'; copyBtn.type = 'button';
  copyBtn.innerHTML = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>';
  copyBtn.onclick = () => copyToClipboard(input.value, copyBtn);
  wrap.append(input, genBtn, copyBtn);
  return {input, node: wrap};
}

function buildFieldCard(section, key, val) {
  const text = getText(section, key);
  const card = document.createElement('div');
  card.className = 'config-field';
  const title = document.createElement('div');
  title.className = 'config-field-title'; title.textContent = text.title;
  card.appendChild(title);
  if (text.desc) {
    const desc = document.createElement('p');
    desc.className = 'config-field-desc'; desc.textContent = text.desc;
    card.appendChild(desc);
  }
  const inputWrap = document.createElement('div');
  inputWrap.className = 'config-field-input';

  let built;
  if (section === 'app' && key === 'custom_instruction') built = buildTextarea(section, key, val, 6);
  else if (typeof val === 'boolean') built = buildBool(section, key, val);
  else if (key === 'image_format') built = buildSelect(section, key, val, [{val:'url',text:'URL'},{val:'base64',text:'Base64'}]);
  else if (key === 'video_format') built = buildSelect(section, key, val, [{val:'html',text:'HTML'},{val:'url',text:'URL'}]);
  else if (section==='video' && key==='upscale_timing') built = buildSelect(section, key, val, [{val:'single',text:'single'},{val:'complete',text:'complete'}]);
  else if (section==='imagine_fast' && key==='size') built = buildSelect(section, key, val, [{val:'1024x1024',text:'1024x1024'},{val:'1280x720',text:'1280x720'},{val:'720x1280',text:'720x1280'},{val:'1792x1024',text:'1792x1024'},{val:'1024x1792',text:'1024x1792'}]);
  else if (section==='imagine_fast' && key==='response_format') built = buildSelect(section, key, val, [{val:'url',text:'URL'},{val:'b64_json',text:'B64 JSON'},{val:'base64',text:'Base64'}]);
  else if (Array.isArray(val) || typeof val === 'object') built = buildJson(section, key, val);
  else if (['api_key','app_key','function_key'].includes(key)) built = buildSecret(section, key, val);
  else built = buildText(section, key, val);

  if (built) inputWrap.appendChild(built.node);
  card.appendChild(inputWrap);

  if (section==='proxy' && key==='enabled' && built) {
    built.input.addEventListener('change', () => applyManagedState(built.input.checked));
  }
  if (section==='app' && key==='function_enabled') {
    card.classList.add('has-action');
    const link = document.createElement('a');
    link.href = '/login'; link.className = 'btn btn-primary btn-icon'; link.title = '访问';
    link.innerHTML = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M7 17L17 7"/><path d="M7 7h10v10"/></svg>';
    link.style.display = val ? 'inline-flex' : 'none';
    card.appendChild(link);
    if (built) built.input.addEventListener('change', () => { link.style.display = built.input.checked ? 'inline-flex' : 'none'; });
  }
  return card;
}

function applyManagedState(enabled) {
  function setDisabled(k, disabled) {
    const el = document.querySelector(`[data-section="proxy"][data-key="${k}"]`);
    if (!el) return;
    el.disabled = disabled;
    const field = el.closest('.config-field');
    if (field) { field.style.opacity = disabled ? '.45' : ''; field.style.pointerEvents = disabled ? 'none' : ''; }
  }
  MANAGED_LOCKED.forEach(k => setDisabled(k, !!enabled));
  MANAGED_PROVIDER.forEach(k => setDisabled(k, !enabled));
}

function renderConfig(data) {
  const container = byId('config-container');
  if (!container) return;
  container.replaceChildren();
  const frag = document.createDocumentFragment();
  sortByOrder(Object.keys(data), SECTION_ORDER).forEach(section => {
    const items = data[section];
    const localeSection = LOCALE_MAP[section];
    const keyOrder = localeSection ? new Map(Object.keys(localeSection).map((k,i) => [k,i])) : null;
    const keys = sortByOrder(Object.keys(items), keyOrder).filter(k => !(section==='proxy' && k==='cf_cookies'));
    if (!keys.length) return;
    const card = document.createElement('div');
    card.className = 'config-section';
    card.innerHTML = `<div class="config-section-title">${getSectionLabel(section)}</div>`;
    const grid = document.createElement('div');
    grid.className = 'config-grid';
    keys.forEach(k => grid.appendChild(buildFieldCard(section, k, items[k])));
    card.appendChild(grid);
    frag.appendChild(card);
  });
  container.appendChild(frag);
  applyManagedState(data.proxy?.enabled);
}

async function init() {
  apiKey = await ensureAdminKey();
  if (!apiKey) return;
  loadData();
}

async function loadData() {
  try {
    const res = await fetch(`${API_BASE}/config`, { headers: buildAuthHeaders(apiKey) });
    if (res.ok) { currentConfig = await res.json(); renderConfig(currentConfig); }
    else if (res.status === 401) logout();
  } catch { showToast(t('common.connectionFailed'), 'error'); }
}

async function saveConfig() {
  const btn = byId('save-btn');
  const orig = btn.innerText;
  btn.disabled = true; btn.innerText = t('config.saving') || '保存中...';
  try {
    const cfg = structuredClone ? structuredClone(currentConfig) : JSON.parse(JSON.stringify(currentConfig));
    document.querySelectorAll('[data-section][data-key]').forEach(input => {
      const s = input.dataset.section, k = input.dataset.key;
      let val = input.value;
      if (input.type === 'checkbox') val = input.checked;
      else if (input.dataset.type === 'json') { try { val = JSON.parse(val); } catch { throw new Error(`Invalid JSON: ${getText(s,k).title}`); } }
      else if (k === 'app_key' && !val.trim()) throw new Error('app_key 不能为空');
      else if (NUMERIC_FIELDS.has(k) && val.trim() !== '' && !isNaN(Number(val))) val = Number(val);
      if (!cfg[s]) cfg[s] = {};
      cfg[s][k] = val;
    });
    if (cfg.proxy?.enabled && !String(cfg.proxy.flaresolverr_url||'').trim()) {
      showToast('启用 Managed Clearance 时需配置 FlareSolverr 地址', 'error');
      btn.disabled = false; btn.innerText = orig; return;
    }
    const res = await fetch(`${API_BASE}/config`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', ...buildAuthHeaders(apiKey) },
      body: JSON.stringify(cfg)
    });
    if (res.ok) { showToast(t('config.configSaved') || '配置已保存', 'success'); }
    else { showToast(t('common.saveFailed') || '保存失败', 'error'); }
  } catch (e) { showToast(e.message, 'error'); }
  finally { btn.disabled = false; btn.innerText = orig; }
}

window.onload = init;
