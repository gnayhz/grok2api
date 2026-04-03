/* Core auth module — key storage, verification, session management */
const API_BASE = '/admin/api';  // admin API prefix
const FN_BASE = '/function';    // function API prefix
const APP_KEY_STORAGE = 'grok2api_app_key';
const FUNCTION_KEY_STORAGE = 'grok2api_function_key';
const APP_KEY_XOR_PREFIX = 'enc:xor:';
const APP_KEY_ENC_PREFIX = 'enc:v1:';
const APP_KEY_SECRET = 'grok2api-admin-key';

let cachedAdminKey = null;
let cachedFunctionKey = null;

const _enc = new TextEncoder();
const _dec = new TextDecoder();

function _toB64(bytes) { let s = ''; bytes.forEach(b => s += String.fromCharCode(b)); return btoa(s); }
function _fromB64(b64) { const s = atob(b64); const a = new Uint8Array(s.length); for (let i = 0; i < s.length; i++) a[i] = s.charCodeAt(i); return a; }
function _xor(data, key) { const o = new Uint8Array(data.length); for (let i = 0; i < data.length; i++) o[i] = data[i] ^ key[i % key.length]; return o; }

function xorEncrypt(plain) {
  return APP_KEY_XOR_PREFIX + _toB64(_xor(_enc.encode(plain), _enc.encode(APP_KEY_SECRET)));
}
function xorDecrypt(stored) {
  if (!stored.startsWith(APP_KEY_XOR_PREFIX)) return stored;
  return _dec.decode(_xor(_fromB64(stored.slice(APP_KEY_XOR_PREFIX.length)), _enc.encode(APP_KEY_SECRET)));
}

async function _deriveKey(salt) {
  const km = await crypto.subtle.importKey('raw', _enc.encode(APP_KEY_SECRET), 'PBKDF2', false, ['deriveKey']);
  return crypto.subtle.deriveKey({ name: 'PBKDF2', salt, iterations: 100000, hash: 'SHA-256' }, km, { name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt']);
}

async function encryptAppKey(plain) {
  if (!plain) return '';
  if (!crypto?.subtle) return xorEncrypt(plain);
  const salt = crypto.getRandomValues(new Uint8Array(16));
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const key = await _deriveKey(salt);
  const cipher = await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, key, _enc.encode(plain));
  return `${APP_KEY_ENC_PREFIX}${_toB64(salt)}:${_toB64(iv)}:${_toB64(new Uint8Array(cipher))}`;
}

async function decryptAppKey(stored) {
  if (!stored) return '';
  if (stored.startsWith(APP_KEY_XOR_PREFIX)) return xorDecrypt(stored);
  if (!stored.startsWith(APP_KEY_ENC_PREFIX)) return stored;
  if (!crypto?.subtle) return '';
  const parts = stored.split(':');
  if (parts.length !== 5) return '';
  const [, , saltB64, ivB64, cipherB64] = parts;
  const key = await _deriveKey(_fromB64(saltB64));
  return _dec.decode(await crypto.subtle.decrypt({ name: 'AES-GCM', iv: _fromB64(ivB64) }, key, _fromB64(cipherB64)));
}

async function getStoredAppKey() {
  const s = localStorage.getItem(APP_KEY_STORAGE) || '';
  if (!s) return '';
  try { return await decryptAppKey(s); } catch { clearStoredAppKey(); return ''; }
}

async function getStoredFunctionKey() {
  const s = localStorage.getItem(FUNCTION_KEY_STORAGE) || '';
  if (!s) return '';
  try { return await decryptAppKey(s); } catch { clearStoredFunctionKey(); return ''; }
}

async function storeAppKey(key) {
  if (!key) { clearStoredAppKey(); return; }
  localStorage.setItem(APP_KEY_STORAGE, await encryptAppKey(key) || '');
}

async function storeFunctionKey(key) {
  if (!key) { clearStoredFunctionKey(); return; }
  localStorage.setItem(FUNCTION_KEY_STORAGE, await encryptAppKey(key) || '');
}

function clearStoredAppKey() { localStorage.removeItem(APP_KEY_STORAGE); cachedAdminKey = null; }
function clearStoredFunctionKey() { localStorage.removeItem(FUNCTION_KEY_STORAGE); cachedFunctionKey = null; }

async function verifyKey(url, key) {
  const h = key ? { Authorization: `Bearer ${key}` } : {};
  return (await fetch(url, { headers: h })).ok;
}

async function ensureAdminKey() {
  if (cachedAdminKey) return cachedAdminKey;
  const key = await getStoredAppKey();
  if (!key) { window.location.href = '/admin/login'; return null; }
  try {
    if (!await verifyKey(`${API_BASE}/verify`, key)) throw 0;
    cachedAdminKey = `Bearer ${key}`;
    return cachedAdminKey;
  } catch { clearStoredAppKey(); window.location.href = '/admin/login'; return null; }
}

async function ensureFunctionKey() {
  if (cachedFunctionKey !== null) return cachedFunctionKey;
  const key = await getStoredFunctionKey();
  if (!key) {
    try { if (await verifyKey(`${FN_BASE}/voice/token`, '')) { cachedFunctionKey = ''; return ''; } } catch {}
    return null;
  }
  try {
    if (!await verifyKey(`${FN_BASE}/voice/token`, key)) throw 0;
    cachedFunctionKey = `Bearer ${key}`;
    return cachedFunctionKey;
  } catch { clearStoredFunctionKey(); return null; }
}

function buildAuthHeaders(apiKey) { return apiKey ? { Authorization: apiKey } : {}; }
function logout() { clearStoredAppKey(); clearStoredFunctionKey(); window.location.href = '/admin/login'; }
function functionLogout() { clearStoredFunctionKey(); window.location.href = '/login'; }

async function fetchStorageType() {
  const apiKey = await ensureAdminKey();
  if (!apiKey) return null;
  try {
    const r = await fetch(`${API_BASE}/storage`, { headers: buildAuthHeaders(apiKey) });
    if (!r.ok) return null;
    const d = await r.json();
    return d?.type || null;
  } catch { return null; }
}

function formatStorageLabel(type) {
  if (!type) return '-';
  const map = { local: 'local', mysql: 'mysql', pgsql: 'pgsql', postgres: 'pgsql', postgresql: 'pgsql', redis: 'redis' };
  return map[type.toLowerCase()] || '-';
}
