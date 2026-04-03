/* Voice — LiveKit session, updated API paths */
(() => {
  let Room, createLocalTracks, RoomEvent, Track, room = null, visualizerTimer = null;
  const $ = id => document.getElementById(id);
  const startBtn=$('startBtn'), stopBtn=$('stopBtn'), statusText=$('statusText'),
    logContainer=$('log'), voiceSelect=$('voiceSelect'), personalitySelect=$('personalitySelect'),
    speedRange=$('speedRange'), speedValue=$('speedValue'), statusVoice=$('statusVoice'),
    statusPersonality=$('statusPersonality'), statusSpeed=$('statusSpeed'),
    audioRoot=$('audioRoot'), copyLogBtn=$('copyLogBtn'), clearLogBtn=$('clearLogBtn'),
    visualizer=$('visualizer');

  function log(msg, level='info') {
    if (!logContainer) return;
    const p = document.createElement('p');
    p.textContent = `[${new Date().toLocaleTimeString()}] ${msg}`;
    if (level === 'error') p.classList.add('log-error');
    else if (level === 'warn') p.classList.add('log-warn');
    logContainer.prepend(p);
  }
  function toast(msg, type) { if (typeof showToast === 'function') showToast(msg, type); else log(msg, type === 'error' ? 'error' : 'info'); }
  function setStatus(state, text) { if (!statusText) return; statusText.textContent = text; statusText.className = 'status-text' + (state ? ' ' + state : ''); }
  function setButtons(connected) { startBtn?.classList.toggle('hidden', connected); stopBtn?.classList.toggle('hidden', !connected); if (!connected && startBtn) startBtn.disabled = false; }
  function updateMeta() { if (statusVoice) statusVoice.textContent = voiceSelect.value; if (statusPersonality) statusPersonality.textContent = personalitySelect.value; if (statusSpeed) statusSpeed.textContent = speedRange.value + 'x'; }

  function initLK() { const lk = window.LiveKitClient || window.LivekitClient; if (!lk) return false; Room=lk.Room; createLocalTracks=lk.createLocalTracks; RoomEvent=lk.RoomEvent; Track=lk.Track; return true; }
  function ensureLK() { if (Room) return true; if (!initLK()) { log(t('voice.livekitSDKError'),'error'); toast(t('voice.livekitLoadFailed'),'error'); return false; } return true; }

  async function startSession() {
    if (!ensureLK()) return;
    const auth = await ensureFunctionKey();
    if (auth === null) { toast(t('common.configurePublicKey'),'error'); window.location.href='/login'; return; }
    startBtn.disabled = true; updateMeta(); setStatus('connecting', t('voice.connectingStatus')); log(t('voice.fetchingToken'));
    const params = new URLSearchParams({ voice: voiceSelect.value, personality: personalitySelect.value, speed: speedRange.value });
    try {
      const res = await fetch(`${FN_BASE}/voice/token?${params}`, { headers: buildAuthHeaders(auth) });
      if (!res.ok) throw new Error(t('voice.fetchTokenFailed', { status: res.status }));
      const { token, url } = await res.json();
      log(`${t('voice.fetchTokenSuccess')} (${voiceSelect.value}, ${personalitySelect.value}, ${speedRange.value}x)`);
      room = new Room({ adaptiveStream: true, dynacast: true });
      room.on(RoomEvent.ParticipantConnected, p => log(t('voice.participantConnected', { identity: p.identity })));
      room.on(RoomEvent.ParticipantDisconnected, p => log(t('voice.participantDisconnected', { identity: p.identity })));
      room.on(RoomEvent.TrackSubscribed, track => {
        log(t('voice.trackSubscribed', { kind: track.kind }));
        if (track.kind === Track.Kind.Audio) { const el = track.attach(); (audioRoot || document.body).appendChild(el); }
      });
      room.on(RoomEvent.Disconnected, () => { log(t('voice.disconnected')); resetUI(); });
      await room.connect(url, token);
      log(t('voice.connectedToServer')); setStatus('connected', t('voice.inCall')); setButtons(true);
      log(t('voice.openingMic'));
      const tracks = await createLocalTracks({ audio: true, video: false });
      for (const tr of tracks) await room.localParticipant.publishTrack(tr);
      log(t('voice.voiceEnabled')); toast(t('voice.voiceConnected'), 'success');
    } catch (err) {
      const msg = err?.message || t('common.connectionFailed');
      log(t('voice.errorPrefix', { msg }), 'error'); toast(msg, 'error');
      setStatus('error', t('common.connectionError')); startBtn.disabled = false;
    }
  }

  async function stopSession() { if (room) await room.disconnect(); resetUI(); }
  function resetUI() { setStatus('', t('common.notConnected')); setButtons(false); if (audioRoot) audioRoot.innerHTML = ''; }

  speedRange?.addEventListener('input', e => {
    speedValue.textContent = Number(e.target.value).toFixed(1);
    const pct = ((e.target.value - speedRange.min) / (speedRange.max - speedRange.min)) * 100;
    speedRange.style.setProperty('--range-progress', pct + '%');
    updateMeta();
  });
  voiceSelect?.addEventListener('change', updateMeta);
  personalitySelect?.addEventListener('change', updateMeta);
  startBtn?.addEventListener('click', startSession);
  stopBtn?.addEventListener('click', stopSession);
  copyLogBtn?.addEventListener('click', async () => {
    const lines = Array.from(logContainer.querySelectorAll('p')).map(p => p.textContent).join('\n');
    try { await navigator.clipboard.writeText(lines); toast(t('voice.logCopied'),'success'); } catch { toast(t('voice.copyLogFailed'),'error'); }
  });
  clearLogBtn?.addEventListener('click', () => { if (logContainer) logContainer.innerHTML = ''; });

  // Init
  speedValue.textContent = Number(speedRange.value).toFixed(1);
  { const pct = ((speedRange.value - speedRange.min) / (speedRange.max - speedRange.min)) * 100; speedRange.style.setProperty('--range-progress', pct + '%'); }

  function buildBars() {
    if (!visualizer) return;
    visualizer.innerHTML = '';
    const n = Math.max(36, Math.floor(visualizer.offsetWidth / 7));
    for (let i = 0; i < n; i++) { const b = document.createElement('div'); b.className = 'bar'; visualizer.appendChild(b); }
  }
  window.addEventListener('resize', buildBars);
  buildBars(); updateMeta(); setStatus('', t('common.notConnected'));
  visualizerTimer = setInterval(() => {
    visualizer?.querySelectorAll('.bar').forEach(b => {
      b.style.height = statusText?.classList.contains('connected') ? `${Math.random()*32+6}px` : '6px';
    });
  }, 150);
})();
