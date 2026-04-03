/* Batch SSE helper */
(function(g) {
  function normalize(key) {
    if (!key) return '';
    const s = String(key).trim();
    return s.startsWith('Bearer ') ? s.slice(7).trim() : s;
  }
  function open(taskId, apiKey, handlers = {}) {
    if (!taskId) return null;
    const raw = normalize(apiKey);
    const url = `${API_BASE}/batch/${taskId}/stream?app_key=${encodeURIComponent(raw)}`;
    const es = new EventSource(url);
    es.onmessage = e => {
      if (!e.data) return;
      try { const msg = JSON.parse(e.data); if (handlers.onMessage) handlers.onMessage(msg); } catch {}
    };
    es.onerror = () => { if (handlers.onError) handlers.onError(); };
    return es;
  }
  function close(es) { if (es) es.close(); }
  async function cancel(taskId, apiKey) {
    if (!taskId) return;
    const raw = normalize(apiKey);
    try { await fetch(`${API_BASE}/batch/${taskId}/cancel`, { method: 'POST', headers: raw ? { Authorization: `Bearer ${raw}` } : undefined }); } catch {}
  }
  g.BatchSSE = { open, close, cancel };
})(window);
