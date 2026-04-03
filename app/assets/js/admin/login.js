const input = document.getElementById('api-key-input');
if (input) input.addEventListener('keypress', e => { if (e.key === 'Enter') login(); });

async function login() {
  const key = (input ? input.value : '').trim();
  if (!key) return;
  try {
    if (await verifyKey(`${API_BASE}/verify`, key)) {
      await storeAppKey(key);
      window.location.href = '/admin/token';
    } else {
      showToast(t('common.invalidKey'), 'error');
    }
  } catch { showToast(t('common.connectionFailed'), 'error'); }
}

(async () => {
  const key = await getStoredAppKey();
  if (!key) return;
  try { if (await verifyKey(`${API_BASE}/verify`, key)) window.location.href = '/admin/token'; } catch {}
})();
