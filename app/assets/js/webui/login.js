const input = document.getElementById('function-key-input');
if (input) input.addEventListener('keypress', e => { if (e.key === 'Enter') login(); });

async function requestFunctionLogin(key) {
  const h = key ? { Authorization: `Bearer ${key}` } : {};
  return (await fetch(`${FN_BASE}/voice/token?voice=ara`, { headers: h })).ok;
}

async function login() {
  const key = (input ? input.value : '').trim();
  try {
    const ok = await requestFunctionLogin(key);
    if (ok) { await storeFunctionKey(key); window.location.href = '/chat'; }
    else showToast(t('common.invalidKey'), 'error');
  } catch { showToast(t('common.connectionFailed'), 'error'); }
}

(async () => {
  try {
    const stored = await getStoredFunctionKey();
    if (stored && await requestFunctionLogin(stored)) { window.location.href = '/chat'; return; }
    if (stored) clearStoredFunctionKey();
    if (await requestFunctionLogin('')) window.location.href = '/chat';
  } catch {}
})();
