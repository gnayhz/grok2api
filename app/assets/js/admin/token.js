/* Token management — full rewrite with updated API paths */
let apiKey = '';
let consumedModeEnabled = false;
let allTokens = {};
let flatTokens = [];
let isBatchProcessing = false;
let currentBatchAction = null;
let currentBatchTaskId = null;
let batchEventSource = null;
let batchTotal = 0;
let batchProcessed = 0;
let currentFilter = 'all';
let currentPage = 1;
let pageSize = 50;

const byId = id => document.getElementById(id);
const DEFAULT_QUOTA = { ssoBasic: 80, ssoSuper: 140 };

function setText(id, text) { const el = byId(id); if (el) el.innerText = text; }

function getSelectedTokens() { return flatTokens.filter(t => t._selected); }
function countSelected(tokens) { return tokens.filter(t => t._selected).length; }
function setSelectedForTokens(tokens, sel) { tokens.forEach(t => t._selected = sel); }

function syncVisibleSelectionUI(sel) {
  document.querySelectorAll('#token-table-body input[type="checkbox"]').forEach(i => i.checked = sel);
  document.querySelectorAll('#token-table-body tr').forEach(r => r.classList.toggle('row-selected', sel));
}

function getFilteredTokens() {
  if (currentFilter === 'all') return flatTokens;
  return flatTokens.filter(t => {
    if (currentFilter === 'active') return t.status === 'active';
    if (currentFilter === 'cooling') return t.status === 'cooling';
    if (currentFilter === 'expired') return t.status !== 'active' && t.status !== 'cooling';
    if (currentFilter === 'nsfw') return t.tags?.includes('nsfw');
    if (currentFilter === 'no-nsfw') return !t.tags?.includes('nsfw');
    return true;
  });
}

function getPaginationData() {
  const filtered = getFilteredTokens();
  const total = filtered.length;
  const pages = Math.max(1, Math.ceil(total / pageSize));
  if (currentPage > pages) currentPage = pages;
  const start = (currentPage - 1) * pageSize;
  return { filtered, total, pages, visible: filtered.slice(start, start + pageSize) };
}

function getVisibleTokens() { return getPaginationData().visible; }

async function init() {
  apiKey = await ensureAdminKey();
  if (!apiKey) return;
  setupConfirmDialog();
  setupSelectAllMenu();
  makeDraggable(byId('batch-actions'));
  const poolSel = byId('edit-pool');
  const quotaIn = byId('edit-quota');
  if (poolSel && quotaIn) poolSel.addEventListener('change', () => { if (currentEditIndex < 0) quotaIn.value = DEFAULT_QUOTA[poolSel.value] || 80; });
  loadData();
}

async function loadData() {
  try {
    const res = await fetch(`${API_BASE}/tokens`, { headers: buildAuthHeaders(apiKey) });
    if (res.ok) {
      const data = await res.json();
      allTokens = data.tokens;
      consumedModeEnabled = data.consumed_mode_enabled || false;
      processTokens(data.tokens);
      updateStats();
      renderTable();
    } else if (res.status === 401) logout();
    else throw new Error(`HTTP ${res.status}`);
  } catch (e) { showToast(e.message, 'error'); }
}

function processTokens(data) {
  flatTokens = [];
  Object.entries(data).forEach(([pool, tokens]) => {
    if (!Array.isArray(tokens)) return;
    tokens.forEach(t => {
      const o = typeof t === 'string' ? { token: t, status: 'active', quota: 0, tags: [] } : { ...t };
      flatTokens.push({ ...o, pool, _selected: false });
    });
  });
}

function updateStats() {
  let total = flatTokens.length, active = 0, cooling = 0, invalid = 0, nsfw = 0, noNsfw = 0, quota = 0, calls = 0;
  flatTokens.forEach(t => {
    if (t.status === 'active') { active++; quota += (t.quota || 0); }
    else if (t.status === 'cooling') cooling++;
    else invalid++;
    t.tags?.includes('nsfw') ? nsfw++ : noNsfw++;
    calls += (t.use_count || 0);
  });
  setText('stat-total', total.toLocaleString());
  setText('stat-active', active.toLocaleString());
  setText('stat-cooling', cooling.toLocaleString());
  setText('stat-invalid', invalid.toLocaleString());
  setText('stat-chat-quota', (consumedModeEnabled ? flatTokens.reduce((s,t) => s+(t.consumed||0), 0) : quota).toLocaleString());
  setText('stat-image-quota', Math.floor((consumedModeEnabled ? flatTokens.reduce((s,t) => s+(t.consumed||0), 0) : quota) / 2).toLocaleString());
  setText('stat-total-calls', calls.toLocaleString());
  const counts = { all: total, active, cooling, expired: invalid, nsfw, 'no-nsfw': noNsfw };
  Object.entries(counts).forEach(([k, v]) => { const el = byId(`tab-count-${k}`); if (el) el.textContent = v; });
}

function renderTable() {
  const tbody = byId('token-table-body');
  const loading = byId('loading');
  const empty = byId('empty-state');
  if (loading) loading.classList.add('hidden');
  const { total, pages, visible } = getPaginationData();
  updatePagination(total, pages);
  if (!visible.length) {
    tbody.replaceChildren();
    if (empty) { empty.textContent = currentFilter === 'all' ? t('token.emptyState') : t('token.emptyFilterState'); empty.classList.remove('hidden'); }
    updateSelectionState(); return;
  }
  if (empty) empty.classList.add('hidden');
  const idxMap = new Map(flatTokens.map((t, i) => [t, i]));
  const frag = document.createDocumentFragment();
  visible.forEach(item => {
    const idx = idxMap.get(item);
    const tr = document.createElement('tr');
    tr.dataset.index = idx;
    if (item._selected) tr.classList.add('row-selected');
    const short = item.token.length > 24 ? item.token.slice(0,8) + '...' + item.token.slice(-16) : item.token;
    const statusClass = item.status === 'active' ? 'badge-green' : item.status === 'cooling' ? 'badge-orange' : item.status === 'expired' ? 'badge-red' : '';
    const nsfwBadge = item.tags?.includes('nsfw') ? ' <span class="badge badge-purple">nsfw</span>' : '';
    tr.innerHTML = `
      <td class="text-center"><input type="checkbox" class="checkbox" ${item._selected?'checked':''} onchange="toggleSelect(${idx})"></td>
      <td><div class="flex items-center gap-2"><span class="font-mono text-xs text-muted" title="${escapeHtml(item.token)}">${escapeHtml(short)}</span><button class="btn-ghost" style="padding:2px" onclick="copyToClipboard('${escapeHtml(item.token)}',this)"><svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg></button></div></td>
      <td class="text-center"><span class="badge">${escapeHtml(item.pool)}</span></td>
      <td class="text-center"><span class="badge ${statusClass}">${item.status}</span>${nsfwBadge}</td>
      <td class="text-center font-mono text-xs">${consumedModeEnabled ? (item.consumed||0) : (item.quota||0)}</td>
      <td class="text-xs text-muted truncate" style="max-width:150px">${escapeHtml(item.note||'-')}</td>
      <td class="text-center"><div class="flex items-center justify-center gap-2">
        <button onclick="refreshStatus('${escapeHtml(item.token)}')" class="btn-ghost" style="padding:2px" title="${t('token.refreshStatus')}"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="23 4 23 10 17 10"/><polyline points="1 20 1 14 7 14"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg></button>
        <button onclick="openEditModal(${idx})" class="btn-ghost" style="padding:2px" title="${t('common.edit')}"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"/><path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z"/></svg></button>
        <button onclick="deleteToken(${idx})" class="btn-ghost" style="padding:2px;color:var(--error)" title="${t('common.delete')}"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg></button>
      </div></td>`;
    frag.appendChild(tr);
  });
  tbody.replaceChildren(frag);
  updateSelectionState();
}

/* Selection */
function toggleSelectAll() { const cb = byId('select-all'); setSelectedForTokens(getVisibleTokens(), cb?.checked); syncVisibleSelectionUI(cb?.checked); updateSelectionState(); }
function toggleSelect(idx) { flatTokens[idx]._selected = !flatTokens[idx]._selected; const r = document.querySelector(`#token-table-body tr[data-index="${idx}"]`); if (r) r.classList.toggle('row-selected', flatTokens[idx]._selected); updateSelectionState(); }

function setupSelectAllMenu() {
  document.addEventListener('click', e => { if (!byId('select-all-wrap')?.contains(e.target)) byId('select-all-popover')?.classList.add('hidden'); });
  document.addEventListener('keydown', e => { if (e.key === 'Escape') byId('select-all-popover')?.classList.add('hidden'); });
}

function handleSelectAllPrimary(e) {
  e?.stopPropagation();
  if (countSelected(flatTokens) > 0) { clearAllSelection(); return; }
  const p = byId('select-all-popover');
  p?.classList.toggle('hidden');
}
function selectVisibleAllFromMenu() { setSelectedForTokens(getVisibleTokens(), true); syncVisibleSelectionUI(true); updateSelectionState(); byId('select-all-popover')?.classList.add('hidden'); }
function selectAllFilteredFromMenu() { setSelectedForTokens(getFilteredTokens(), true); syncVisibleSelectionUI(true); updateSelectionState(); byId('select-all-popover')?.classList.add('hidden'); }
function clearAllSelection() { setSelectedForTokens(flatTokens, false); syncVisibleSelectionUI(false); updateSelectionState(); byId('select-all-popover')?.classList.add('hidden'); }

function updateSelectionState() {
  const sel = countSelected(flatTokens);
  const vis = getVisibleTokens();
  const visSel = countSelected(vis);
  const cb = byId('select-all');
  if (cb) { cb.checked = vis.length > 0 && visSel === vis.length; cb.indeterminate = visSel > 0 && visSel < vis.length; }
  const label = byId('select-all-label');
  if (label) label.textContent = sel > 0 ? t('token.clearSelection') : t('common.selectAll');
  const trigger = byId('select-all-trigger');
  if (trigger) trigger.classList.toggle('is-active', sel > 0);
  const caret = byId('select-all-caret');
  if (caret) caret.style.display = sel > 0 ? 'none' : 'inline';
  const countEl = byId('selected-count');
  if (countEl) countEl.textContent = sel;
  if (sel > 0) byId('select-all-popover')?.classList.add('hidden');
  setActionButtons(sel);
}

function setActionButtons(count = null) {
  const c = count ?? countSelected(flatTokens);
  const dis = isBatchProcessing;
  ['btn-batch-export','btn-batch-update','btn-batch-disable','btn-batch-enable','btn-batch-nsfw','btn-batch-delete'].forEach(id => {
    const b = byId(id); if (b) b.disabled = dis || c === 0;
  });
}

/* Filtering & Pagination */
function filterByStatus(status) {
  currentFilter = status; currentPage = 1;
  document.querySelectorAll('.tab-item').forEach(t => { t.classList.toggle('active', t.dataset.filter === status); });
  renderTable();
}

function updatePagination(total, pages) {
  const info = byId('pagination-info');
  if (info) info.textContent = `${total === 0 ? 0 : currentPage} / ${pages} · ${total}`;
  const prev = byId('page-prev'); if (prev) prev.disabled = currentPage <= 1;
  const next = byId('page-next'); if (next) next.disabled = currentPage >= pages;
}
function goPrevPage() { if (currentPage > 1) { currentPage--; renderTable(); } }
function goNextPage() { const p = getPaginationData().pages; if (currentPage < p) { currentPage++; renderTable(); } }
function changePageSize() { const v = parseInt(byId('page-size')?.value); if (v) { pageSize = v; currentPage = 1; renderTable(); } }

/* CRUD */
let currentEditIndex = -1;
function addToken() { openEditModal(-1); }

function openEditModal(idx) {
  currentEditIndex = idx;
  const isNew = idx < 0;
  if (isNew) {
    byId('edit-token-display').value = ''; byId('edit-token-display').disabled = false;
    byId('edit-pool').value = 'ssoBasic'; byId('edit-quota').value = 80; byId('edit-note').value = '';
    document.querySelector('#edit-modal .modal-title').textContent = t('token.addTitle');
  } else {
    const item = flatTokens[idx];
    byId('edit-token-display').value = item.token; byId('edit-token-display').disabled = true;
    byId('edit-pool').value = item.pool;
    byId('edit-quota').value = consumedModeEnabled ? (item.consumed||0) : item.quota;
    byId('edit-note').value = item.note || '';
    document.querySelector('#edit-modal .modal-title').textContent = t('token.editTitle');
  }
  openModal('edit-modal');
}
function closeEditModal() { closeModal('edit-modal'); }

async function saveEdit() {
  const pool = byId('edit-pool').value.trim() || 'ssoBasic';
  const quota = parseInt(byId('edit-quota').value) || 0;
  const note = byId('edit-note').value.trim().slice(0, 50);
  if (currentEditIndex >= 0) {
    const item = flatTokens[currentEditIndex];
    item.pool = pool; if (!consumedModeEnabled) item.quota = quota; item.note = note;
  } else {
    const token = byId('edit-token-display').value.trim();
    if (!token) return showToast(t('token.tokenEmpty'), 'error');
    if (flatTokens.some(t => t.token === token)) return showToast(t('token.tokenExists'), 'error');
    flatTokens.push({ token, pool, quota, consumed: 0, note, status: 'active', use_count: 0, tags: [], _selected: false });
  }
  await syncToServer(); closeEditModal(); loadData();
}

async function deleteToken(idx) {
  if (!await confirmAction(t('token.confirmDelete'), { okText: t('common.delete') })) return;
  flatTokens.splice(idx, 1);
  syncToServer().then(loadData);
}

async function syncToServer() {
  const data = {};
  flatTokens.forEach(t => {
    if (!data[t.pool]) data[t.pool] = [];
    const p = { token: t.token, status: t.status, quota: t.quota, consumed: t.consumed||0, note: t.note||'', fail_count: t.fail_count||0, use_count: t.use_count||0, tags: t.tags||[] };
    if (t.created_at) p.created_at = t.created_at;
    if (t.last_used_at) p.last_used_at = t.last_used_at;
    if (t.last_fail_at) p.last_fail_at = t.last_fail_at;
    if (t.last_sync_at) p.last_sync_at = t.last_sync_at;
    if (t.last_asset_clear_at) p.last_asset_clear_at = t.last_asset_clear_at;
    if (t.last_fail_reason) p.last_fail_reason = t.last_fail_reason;
    data[t.pool].push(p);
  });
  try {
    const res = await fetch(`${API_BASE}/tokens`, { method: 'POST', headers: { 'Content-Type': 'application/json', ...buildAuthHeaders(apiKey) }, body: JSON.stringify(data) });
    if (!res.ok) showToast(t('common.saveFailed'), 'error');
  } catch (e) { showToast(e.message, 'error'); }
}

/* Import/Export */
function openImportModal() { openModal('import-modal'); }
function closeImportModal() { closeModal('import-modal', () => { const i = byId('import-text'); if (i) i.value = ''; }); }
async function submitImport() {
  const pool = byId('import-pool').value.trim() || 'ssoBasic';
  const lines = byId('import-text').value.split('\n');
  lines.forEach(l => { const t = l.trim(); if (t && !flatTokens.some(ft => ft.token === t)) flatTokens.push({ token: t, pool, status: 'active', quota: DEFAULT_QUOTA[pool]||80, consumed: 0, note: '', tags: [], fail_count: 0, use_count: 0, _selected: false }); });
  await syncToServer(); closeImportModal(); loadData();
}
function batchExport() {
  const sel = getSelectedTokens();
  if (!sel.length) return showToast(t('common.noTokenSelected'), 'error');
  downloadTextFile(sel.map(t => t.token).join('\n')+'\n', `tokens_${new Date().toISOString().slice(0,10)}.txt`);
}

/* Batch ops */
async function refreshStatus(token) {
  try {
    const res = await fetch(`${API_BASE}/tokens/refresh`, { method: 'POST', headers: { 'Content-Type': 'application/json', ...buildAuthHeaders(apiKey) }, body: JSON.stringify({ tokens: [token] }) });
    if (res.ok) { showToast(t('token.refreshSuccess'), 'success'); loadData(); }
    else showToast(t('token.refreshFailed'), 'error');
  } catch { showToast(t('token.requestError'), 'error'); }
}

async function batchUpdate() { await startBatchAsync('refresh', `${API_BASE}/tokens/refresh/async`); }
async function batchEnableNSFW() {
  const sel = getSelectedTokens();
  if (!sel.length) return showToast(t('common.noTokenSelected'), 'error');
  if (!await confirmAction(t('token.nsfwConfirm', { count: sel.length }), { okText: t('token.nsfwEnable') })) return;
  await startBatchAsync('nsfw', `${API_BASE}/tokens/nsfw/enable/async`);
}

async function startBatchAsync(action, url) {
  if (isBatchProcessing) return showToast(t('common.taskInProgress'), 'info');
  const sel = getSelectedTokens();
  if (!sel.length) return showToast(t('common.noTokenSelected'), 'error');
  isBatchProcessing = true; currentBatchAction = action;
  batchTotal = sel.length; batchProcessed = 0;
  updateBatchProgress(); setActionButtons();
  try {
    const res = await fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json', ...buildAuthHeaders(apiKey) }, body: JSON.stringify({ tokens: sel.map(t => t.token) }) });
    const data = await res.json();
    if (!res.ok || data.status !== 'success') throw new Error(data.detail || 'Failed');
    currentBatchTaskId = data.task_id;
    BatchSSE.close(batchEventSource);
    batchEventSource = BatchSSE.open(currentBatchTaskId, apiKey, {
      onMessage: msg => {
        if (msg.type === 'progress' || msg.type === 'snapshot') {
          if (msg.total) batchTotal = msg.total;
          if (msg.processed != null) batchProcessed = msg.processed;
          updateBatchProgress();
        } else if (msg.type === 'done') {
          batchProcessed = batchTotal; updateBatchProgress();
          finishBatch(false); showToast(action === 'nsfw' ? t('token.nsfwDone') : t('token.refreshDone'), 'success');
          closeBatchSSE();
        } else if (msg.type === 'cancelled') {
          finishBatch(true); showToast(t('common.cancelled'), 'info'); closeBatchSSE();
        } else if (msg.type === 'error') {
          finishBatch(true); showToast(msg.error || t('common.unknownError'), 'error'); closeBatchSSE();
        }
      },
      onError: () => { finishBatch(true); showToast(t('common.connectionInterrupted'), 'error'); closeBatchSSE(); }
    });
  } catch (e) { finishBatch(true); showToast(e.message, 'error'); }
}

function closeBatchSSE() { currentBatchTaskId = null; BatchSSE.close(batchEventSource); batchEventSource = null; }
function stopBatchRefresh() { if (currentBatchTaskId) { BatchSSE.cancel(currentBatchTaskId, apiKey); closeBatchSSE(); } finishBatch(true); }
function finishBatch(aborted) { isBatchProcessing = false; currentBatchAction = null; updateBatchProgress(); setActionButtons(); updateSelectionState(); loadData(); }

function updateBatchProgress() {
  const c = byId('batch-progress'), txt = byId('batch-progress-text'), stop = byId('btn-stop-action');
  if (!c) return;
  if (!isBatchProcessing) { c.classList.add('hidden'); stop?.classList.add('hidden'); return; }
  c.classList.remove('hidden'); stop?.classList.remove('hidden');
  if (txt) txt.textContent = batchTotal ? `${Math.floor(batchProcessed/batchTotal*100)}%` : '...';
}

async function batchDelete() {
  if (isBatchProcessing) return;
  const sel = getSelectedTokens();
  if (!sel.length) return showToast(t('common.noTokenSelected'), 'error');
  if (!await confirmAction(t('token.confirmBatchDelete', { count: sel.length }), { okText: t('common.delete') })) return;
  const remove = new Set(sel.map(t => t.token));
  flatTokens = flatTokens.filter(t => !remove.has(t.token));
  await syncToServer(); showToast(t('token.deleteDone'), 'success'); loadData();
}

async function batchSetStatus(target) {
  if (isBatchProcessing) return;
  const sel = getSelectedTokens();
  if (!sel.length) return showToast(t('common.noTokenSelected'), 'error');
  const targets = sel.filter(t => t.status !== target);
  if (!targets.length) return;
  const toDisabled = target === 'disabled';
  if (!await confirmAction(t(toDisabled ? 'token.confirmBatchDisable' : 'token.confirmBatchEnable', { count: targets.length }))) return;
  targets.forEach(t => t.status = target);
  await syncToServer(); loadData(); showToast(toDisabled ? t('token.batchDisableDone') : t('token.batchEnableDone'), 'success');
}
function batchDisableTokens() { batchSetStatus('disabled'); }
function batchEnableTokens() { batchSetStatus('active'); }

window.onload = init;
