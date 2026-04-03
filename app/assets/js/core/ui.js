/* Shared UI helpers: modal, draggable, confirm dialog */

function openModal(id) {
  const m = document.getElementById(id);
  if (!m) return null;
  m.classList.remove('hidden');
  requestAnimationFrame(() => m.classList.add('is-open'));
  return m;
}

function closeModal(id, cb) {
  const m = document.getElementById(id);
  if (!m) return;
  m.classList.remove('is-open');
  setTimeout(() => { m.classList.add('hidden'); if (cb) cb(); }, 200);
}

/* Confirm dialog — requires #confirm-dialog, #confirm-message, #confirm-ok, #confirm-cancel in DOM */
let _confirmResolve = null;

function setupConfirmDialog() {
  const d = document.getElementById('confirm-dialog');
  if (!d) return;
  const ok = document.getElementById('confirm-ok');
  const cancel = document.getElementById('confirm-cancel');
  d.addEventListener('click', e => { if (e.target === d) _closeConfirm(false); });
  if (ok) ok.addEventListener('click', () => _closeConfirm(true));
  if (cancel) cancel.addEventListener('click', () => _closeConfirm(false));
}

function confirmAction(message, opts = {}) {
  const d = document.getElementById('confirm-dialog');
  if (!d) return Promise.resolve(false);
  const msg = document.getElementById('confirm-message');
  const ok = document.getElementById('confirm-ok');
  const cancel = document.getElementById('confirm-cancel');
  if (msg) msg.textContent = message;
  if (ok) ok.textContent = opts.okText || t('common.ok');
  if (cancel) cancel.textContent = opts.cancelText || t('common.cancel');
  return new Promise(resolve => {
    _confirmResolve = resolve;
    d.classList.remove('hidden');
    requestAnimationFrame(() => d.classList.add('is-open'));
  });
}

function _closeConfirm(ok) {
  const d = document.getElementById('confirm-dialog');
  if (!d) return;
  d.classList.remove('is-open');
  setTimeout(() => { d.classList.add('hidden'); if (_confirmResolve) { _confirmResolve(ok); _confirmResolve = null; } }, 200);
}

/* Draggable element */
function makeDraggable(el) {
  if (!el) return;
  let dragging = false, sx, sy, il, it;
  el.style.touchAction = 'none';
  el.addEventListener('pointerdown', e => {
    if (e.target.closest('button, a, input, select')) return;
    e.preventDefault();
    dragging = true;
    el.setPointerCapture(e.pointerId);
    sx = e.clientX; sy = e.clientY;
    const r = el.getBoundingClientRect();
    if (!el.style.left) {
      el.style.left = r.left + 'px';
      el.style.top = r.top + 'px';
      el.style.transform = 'none';
      el.style.bottom = 'auto';
    }
    il = parseFloat(el.style.left);
    it = parseFloat(el.style.top);
  });
  document.addEventListener('pointermove', e => {
    if (!dragging) return;
    el.style.left = (il + e.clientX - sx) + 'px';
    el.style.top = (it + e.clientY - sy) + 'px';
  });
  document.addEventListener('pointerup', e => {
    if (dragging) { dragging = false; el.releasePointerCapture(e.pointerId); }
  });
}

/* Escape HTML */
function escapeHtml(s) {
  if (!s) return '';
  return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

/* Copy to clipboard with button feedback */
async function copyToClipboard(text, btn) {
  if (!text) return;
  try {
    await navigator.clipboard.writeText(text);
    if (!btn) return;
    const orig = btn.innerHTML;
    btn.innerHTML = '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="20 6 9 17 4 12"/></svg>';
    btn.style.color = 'var(--success)';
    setTimeout(() => { btn.innerHTML = orig; btn.style.color = ''; }, 1500);
  } catch {}
}

/* Download text file */
function downloadTextFile(content, filename) {
  const url = URL.createObjectURL(new Blob([content], { type: 'text/plain' }));
  const a = document.createElement('a');
  a.href = url; a.download = filename;
  document.body.appendChild(a); a.click(); a.remove();
  URL.revokeObjectURL(url);
}
