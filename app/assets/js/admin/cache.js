/* Cache management — simplified, updated API paths */
let apiKey = '';
let currentTab = 'image';
let listPage = 1;
const PAGE_SIZE = 100;
let listData = { image: { items: [], total: 0 }, video: { items: [], total: 0 } };

const byId = id => document.getElementById(id);

async function init() {
  apiKey = await ensureAdminKey();
  if (!apiKey) return;
  setupConfirmDialog();
  loadStats();
}

async function loadStats() {
  try {
    const res = await fetch(`${API_BASE}/cache`, { headers: buildAuthHeaders(apiKey) });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const data = await res.json();
    byId('img-count').textContent = data.local_image?.count ?? '-';
    byId('img-size').textContent = data.local_image?.size_mb != null ? data.local_image.size_mb + ' MB' : '-';
    byId('video-count').textContent = data.local_video?.count ?? '-';
    byId('video-size').textContent = data.local_video?.size_mb != null ? data.local_video.size_mb + ' MB' : '-';
  } catch (e) { showToast(e.message, 'error'); }
  loadList();
}

async function loadList() {
  try {
    const res = await fetch(`${API_BASE}/cache/list?type=${currentTab}&page=${listPage}&page_size=${PAGE_SIZE}`, { headers: buildAuthHeaders(apiKey) });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const data = await res.json();
    listData[currentTab] = { items: data.items || [], total: data.total || 0 };
    renderList();
  } catch (e) { showToast(e.message, 'error'); }
}

function renderList() {
  const { items, total } = listData[currentTab];
  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE));
  const tbody = byId('file-list-body');
  const empty = byId('file-empty');
  const info = byId('list-info');
  const prev = byId('list-prev');
  const next = byId('list-next');

  if (info) info.textContent = `${total === 0 ? 0 : listPage} / ${pages} · ${total}`;
  if (prev) prev.disabled = listPage <= 1;
  if (next) next.disabled = listPage >= pages;

  if (!items.length) {
    tbody.replaceChildren();
    empty?.classList.remove('hidden');
    return;
  }
  empty?.classList.add('hidden');

  const frag = document.createDocumentFragment();
  items.forEach(f => {
    const tr = document.createElement('tr');
    const sizeKB = f.size_bytes ? (f.size_bytes / 1024).toFixed(1) + ' KB' : '-';
    tr.innerHTML = `
      <td class="font-mono text-xs truncate" style="max-width:400px" title="${escapeHtml(f.name)}">${escapeHtml(f.name)}</td>
      <td class="text-right text-xs text-muted">${sizeKB}</td>
      <td class="text-center">
        <button onclick="deleteFile('${escapeHtml(f.name)}')" class="btn-ghost" style="padding:2px;color:var(--error)" title="删除">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>
        </button>
      </td>`;
    frag.appendChild(tr);
  });
  tbody.replaceChildren(frag);
}

function switchTab(tab) {
  currentTab = tab; listPage = 1;
  document.querySelectorAll('.tab-item').forEach(b => b.classList.toggle('active', b.dataset.tab === tab));
  loadList();
}
function listPrev() { if (listPage > 1) { listPage--; loadList(); } }
function listNext() { const pages = Math.max(1, Math.ceil((listData[currentTab]?.total||0) / PAGE_SIZE)); if (listPage < pages) { listPage++; loadList(); } }

async function deleteFile(name) {
  if (!await confirmAction(`删除文件 ${name}？`)) return;
  try {
    const res = await fetch(`${API_BASE}/cache/item/delete`, {
      method: 'POST', headers: { 'Content-Type': 'application/json', ...buildAuthHeaders(apiKey) },
      body: JSON.stringify({ type: currentTab, name })
    });
    if (res.ok) { showToast('已删除', 'success'); loadStats(); }
    else showToast('删除失败', 'error');
  } catch (e) { showToast(e.message, 'error'); }
}

async function clearLocalCache() {
  if (!await confirmAction(`清理全部${currentTab === 'image' ? '图片' : '视频'}缓存？`)) return;
  try {
    const res = await fetch(`${API_BASE}/cache/clear`, {
      method: 'POST', headers: { 'Content-Type': 'application/json', ...buildAuthHeaders(apiKey) },
      body: JSON.stringify({ type: currentTab })
    });
    if (res.ok) { showToast('已清理', 'success'); loadStats(); }
    else showToast('清理失败', 'error');
  } catch (e) { showToast(e.message, 'error'); }
}

window.onload = init;
