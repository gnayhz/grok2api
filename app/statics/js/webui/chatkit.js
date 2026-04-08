(() => {
  const VOICE_ENDPOINT = '/webui/api/voice/token';
  const MOBILE_BREAKPOINT = 960;
  const voiceSelect = document.getElementById('voiceSelect');
  const personalitySelect = document.getElementById('personalitySelect');
  const speedSelect = document.getElementById('speedSelect');
  const startVoiceBtn = document.getElementById('startVoiceBtn');
  const muteVoiceBtn = document.getElementById('muteVoiceBtn');
  const newSessionBtn = document.getElementById('newSessionBtn');
  const chatkitPanelToggle = document.getElementById('chatkitPanelToggle');
  const chatkitShell = document.querySelector('.webui-chatkit-shell');
  const chatkitPanel = document.querySelector('.webui-chatkit-panel');
  const voiceLog = document.getElementById('voiceLog');
  const clearVoiceLogBtn = document.getElementById('clearVoiceLogBtn');
  const connectionBadge = document.getElementById('connectionBadge');
  const connectionText = document.getElementById('connectionText');
  const voiceOrb = document.getElementById('voiceOrb');
  const roomName = document.getElementById('roomName');
  const participantName = document.getElementById('participantName');
  const remoteCount = document.getElementById('remoteCount');
  const voiceEndpoint = document.getElementById('voiceEndpoint');
  const audioRoot = document.getElementById('audioRoot');

  let room = null;
  let remoteParticipants = 0;
  let micEnabled = true;
  let outputMuted = false;
  let mobilePanelOpen = false;
  let orbAudioContext = null;
  let orbAnalyser = null;
  let orbSource = null;
  let orbData = null;
  let orbFrame = 0;
  let orbLevel = 0;
  let orbStreamId = '';

  const controlIcon = {
    start: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M9 7.25 17 12l-8 4.75V7.25Z" fill="currentColor" stroke="none"/></svg>',
    pause: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M9 6.5v11"/><path d="M15 6.5v11"/></svg>',
    mute: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 10h3l4-4v12l-4-4H5z"/><path d="M16 9a4.5 4.5 0 0 1 0 6"/><path d="M18.5 6.5a8 8 0 0 1 0 11"/></svg>',
    unmute: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 10h3l4-4v12l-4-4H5z"/><path d="m16 9 5 6"/><path d="m21 9-5 6"/></svg>',
    newSession: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14"/><path d="M5 12h14"/></svg>',
  };

  const text = (key, fallback, params) => {
    if (typeof window.t !== 'function') return fallback;
    const value = t(key, params);
    return value === key ? fallback : value;
  };

  const isMobileLayout = () => window.innerWidth <= MOBILE_BREAKPOINT;

  const syncMobilePanel = () => {
    if (!chatkitShell || !chatkitPanelToggle) return;
    const mobile = isMobileLayout();
    chatkitShell.classList.toggle('is-mobile-panel-open', mobile && mobilePanelOpen);
    chatkitPanelToggle.classList.toggle('is-open', mobile && mobilePanelOpen);
    chatkitPanelToggle.hidden = !mobile;
    chatkitPanelToggle.setAttribute('aria-expanded', mobile && mobilePanelOpen ? 'true' : 'false');
    const label = mobile && mobilePanelOpen
      ? text('webui.chatkit.panelHide', '收起设置')
      : text('webui.chatkit.panelShow', '设置');
    chatkitPanelToggle.setAttribute('aria-label', label);
    chatkitPanelToggle.setAttribute('title', label);
  };

  const syncVoiceLogEmptyState = () => {
    if (!voiceLog) return;
    voiceLog.dataset.empty = text(
      'webui.chatkit.logEmpty',
      '待机中，连接后这里会显示会话事件。',
    );
  };

  const logLine = (message, level = 'info') => {
    if (!voiceLog) return;
    const item = document.createElement('div');
    item.className = `webui-chatkit-log-item${level !== 'info' ? ` is-${level}` : ''}`;
    item.textContent = `[${new Date().toLocaleTimeString()}] ${message}`;
    voiceLog.prepend(item);
    syncVoiceLogEmptyState();
  };

  const setOrbLevel = (level) => {
    orbLevel = Math.max(0, Math.min(1, level || 0));
    if (!voiceOrb) return;
    voiceOrb.style.setProperty('--chatkit-level', orbLevel.toFixed(3));
    voiceOrb.classList.toggle('is-speaking', voiceOrb.classList.contains('is-live') && orbLevel > 0.08);
  };

  const stopOrbAnalysis = () => {
    if (orbFrame) {
      cancelAnimationFrame(orbFrame);
      orbFrame = 0;
    }
    if (orbSource) {
      try {
        orbSource.disconnect();
      } catch {}
    }
    orbAnalyser = null;
    orbSource = null;
    orbData = null;
    orbStreamId = '';
    setOrbLevel(0);
  };

  const ensureOrbAudioContext = async () => {
    const AudioContextCtor = window.AudioContext || window.webkitAudioContext;
    if (!AudioContextCtor) return null;
    if (!orbAudioContext) orbAudioContext = new AudioContextCtor();
    if (orbAudioContext.state === 'suspended') {
      try {
        await orbAudioContext.resume();
      } catch {}
    }
    return orbAudioContext;
  };

  const startOrbAnalysis = async (element) => {
    if (!element || !(element.srcObject instanceof MediaStream)) {
      stopOrbAnalysis();
      return;
    }

    const stream = element.srcObject;
    const streamId = stream.id || stream.getAudioTracks?.()[0]?.id || '';
    if (streamId && streamId === orbStreamId && orbAnalyser) return;

    stopOrbAnalysis();
    const context = await ensureOrbAudioContext();
    if (!context) return;

    try {
      orbAnalyser = context.createAnalyser();
      orbAnalyser.fftSize = 256;
      orbAnalyser.smoothingTimeConstant = 0.82;
      orbData = new Uint8Array(orbAnalyser.fftSize);
      orbSource = context.createMediaStreamSource(stream);
      orbSource.connect(orbAnalyser);
      orbStreamId = streamId;

      const render = () => {
        if (!orbAnalyser || !orbData) return;
        orbAnalyser.getByteTimeDomainData(orbData);
        let sum = 0;
        for (let index = 0; index < orbData.length; index += 1) {
          const normalized = (orbData[index] - 128) / 128;
          sum += normalized * normalized;
        }
        const rms = Math.sqrt(sum / orbData.length);
        const targetLevel = Math.max(0, Math.min(1, (rms - 0.015) * 8.2));
        const nextLevel = orbLevel + (targetLevel - orbLevel) * 0.22;
        setOrbLevel(nextLevel < 0.012 ? 0 : nextLevel);
        orbFrame = requestAnimationFrame(render);
      };

      render();
    } catch {
      stopOrbAnalysis();
    }
  };

  const setStatus = (state, label, description) => {
    if (connectionBadge) connectionBadge.textContent = label;
    if (connectionText) connectionText.textContent = description;
    if (voiceOrb) {
      voiceOrb.classList.remove('is-idle', 'is-connecting', 'is-live', 'is-paused', 'is-output-muted', 'is-error');
      voiceOrb.classList.add(state);
      if (state !== 'is-live') {
        voiceOrb.classList.remove('is-speaking');
        setOrbLevel(0);
      }
    }
  };

  const renderConnectedStatus = () => {
    if (!room) {
      setStatus(
        'is-idle',
        text('webui.chatkit.statusIdle', '未连接'),
        text('webui.chatkit.idleText', '准备好后点击开始，授权麦克风即可进入语音会话。'),
      );
      return;
    }

    if (!micEnabled) {
      setStatus(
        'is-paused',
        text('webui.chatkit.statusPaused', '已暂停'),
        text('webui.chatkit.pausedText', '会话已暂停，点击开始即可继续当前语音会话。'),
      );
      return;
    }

    if (outputMuted) {
      setStatus(
        'is-output-muted',
        text('webui.chatkit.statusMuted', '已静音'),
        text('webui.chatkit.outputMutedText', '扬声器已静音，你仍然可以继续说话。'),
      );
      return;
    }

    setStatus(
      'is-live',
      text('webui.chatkit.statusLive', '语音中'),
      text('webui.chatkit.liveText', '连接已建立，现在可以直接开口和 Grok 对话。'),
    );
  };

  const setButtons = (connected) => {
    if (startVoiceBtn) {
      startVoiceBtn.disabled = false;
      const label = connected && micEnabled
        ? text('webui.chatkit.pause', '暂停')
        : text('webui.chatkit.start', '开始');
      startVoiceBtn.innerHTML = connected && micEnabled ? controlIcon.pause : controlIcon.start;
      startVoiceBtn.setAttribute('aria-label', label);
      startVoiceBtn.setAttribute('title', label);
    }
    if (muteVoiceBtn) {
      muteVoiceBtn.disabled = !connected;
      const label = outputMuted
        ? text('webui.chatkit.unmute', '取消静音')
        : text('webui.chatkit.mute', '静音');
      muteVoiceBtn.innerHTML = outputMuted ? controlIcon.unmute : controlIcon.mute;
      muteVoiceBtn.setAttribute('aria-label', label);
      muteVoiceBtn.setAttribute('title', label);
    }
    if (newSessionBtn) {
      newSessionBtn.disabled = !connected;
      const label = text('webui.chatkit.newSession', '新会话');
      newSessionBtn.innerHTML = controlIcon.newSession;
      newSessionBtn.setAttribute('aria-label', label);
      newSessionBtn.setAttribute('title', label);
    }
  };

  const updateRemoteCount = () => {
    if (remoteCount) remoteCount.textContent = String(Math.max(0, remoteParticipants));
  };

  const detachAudio = () => {
    stopOrbAnalysis();
    if (!audioRoot) return;
    audioRoot.querySelectorAll('audio').forEach((node) => {
      try {
        node.pause();
        node.srcObject = null;
      } catch {}
      node.remove();
    });
  };

  const resetSessionMeta = () => {
    if (roomName) roomName.textContent = '-';
    if (participantName) participantName.textContent = '-';
    if (voiceEndpoint) voiceEndpoint.textContent = 'wss://livekit.grok.com';
    remoteParticipants = 0;
    updateRemoteCount();
  };

  const getLiveKit = () => window.LiveKitClient || window.LivekitClient || null;

  const getAuthHeaders = async () => {
    const key = await webuiKey.get();
    return key ? { Authorization: `Bearer ${key}` } : {};
  };

  const addRemoteAudioTrack = (track) => {
    if (!audioRoot || !track || track.kind !== 'audio') return;
    const element = track.attach();
    element.autoplay = true;
    element.playsInline = true;
    element.muted = outputMuted;
    audioRoot.appendChild(element);
    void startOrbAnalysis(element);
  };

  const bindRoomEvents = (lk, currentRoom) => {
    currentRoom.on(lk.RoomEvent.ParticipantConnected, (participant) => {
      remoteParticipants += 1;
      updateRemoteCount();
      logLine(text('webui.chatkit.participantJoined', 'Remote participant joined: {identity}', {
        identity: participant.identity || 'remote',
      }));
    });

    currentRoom.on(lk.RoomEvent.ParticipantDisconnected, (participant) => {
      remoteParticipants = Math.max(0, remoteParticipants - 1);
      updateRemoteCount();
      logLine(text('webui.chatkit.participantLeft', 'Remote participant left: {identity}', {
        identity: participant.identity || 'remote',
      }));
    });

    currentRoom.on(lk.RoomEvent.TrackSubscribed, (track) => {
      addRemoteAudioTrack(track);
      logLine(text('webui.chatkit.trackSubscribed', 'Remote audio subscribed'));
    });

    currentRoom.on(lk.RoomEvent.TrackUnsubscribed, (track) => {
      try {
        const elements = track.detach();
        let activeRemoved = false;
        elements.forEach((el) => {
          if (el instanceof HTMLMediaElement && el.srcObject instanceof MediaStream) {
            const streamId = el.srcObject.id || el.srcObject.getAudioTracks?.()[0]?.id || '';
            if (streamId && streamId === orbStreamId) activeRemoved = true;
          }
          el.remove();
        });
        if (activeRemoved) {
          const nextAudio = audioRoot?.querySelector('audio');
          if (nextAudio instanceof HTMLAudioElement) {
            void startOrbAnalysis(nextAudio);
          } else {
            stopOrbAnalysis();
          }
        }
      } catch {}
    });

    currentRoom.on(lk.RoomEvent.Disconnected, () => {
      logLine(text('webui.chatkit.disconnected', 'Voice session disconnected'), 'warn');
      teardownSession(false);
    });
  };

  const teardownSession = async (manual) => {
    const currentRoom = room;
    room = null;
    try {
      if (currentRoom) await currentRoom.disconnect();
    } catch {}
    detachAudio();
    resetSessionMeta();
    micEnabled = true;
    outputMuted = false;
    setButtons(false);
    renderConnectedStatus();
    if (manual && connectionText) {
      connectionText.textContent = text('webui.chatkit.endedText', '语音会话已结束，可以重新开始。');
    }
  };

  const startSession = async () => {
    const lk = getLiveKit();
    if (!lk || !lk.Room) {
      showToast?.(text('webui.chatkit.livekitLoadFailed', 'LiveKit SDK 加载失败'), 'error');
      return;
    }

    if (startVoiceBtn) startVoiceBtn.disabled = true;
    void ensureOrbAudioContext();
    setStatus(
      'is-connecting',
      text('webui.chatkit.statusConnecting', '正在连接'),
      text('webui.chatkit.connectingText', '正在向 Grok Voice 申请会话并连接 LiveKit…'),
    );
    logLine(text('webui.chatkit.fetchingToken', 'Requesting voice token...'));

    try {
      const params = new URLSearchParams({
        voice: voiceSelect?.value || 'ara',
        personality: personalitySelect?.value || 'assistant',
        speed: speedSelect?.value || '1.0',
      });
      const res = await fetch(`${VOICE_ENDPOINT}?${params.toString()}`, {
        headers: await getAuthHeaders(),
        cache: 'no-store',
      });
      if (!res.ok) {
        const detail = await res.text().catch(() => '');
        throw new Error(detail || `HTTP ${res.status}`);
      }

      const payload = await res.json();
      if (!payload || !payload.token || !payload.url) {
        throw new Error(text('webui.chatkit.invalidToken', 'Voice token response invalid'));
      }

      if (roomName) roomName.textContent = payload.room_name || '-';
      if (participantName) participantName.textContent = payload.participant_name || '-';
      if (voiceEndpoint) voiceEndpoint.textContent = payload.url || 'wss://livekit.grok.com';

      const currentRoom = new lk.Room({
        adaptiveStream: true,
        dynacast: true,
        audioCaptureDefaults: {
          autoGainControl: true,
          echoCancellation: true,
          noiseSuppression: true,
        },
      });
      room = currentRoom;
      bindRoomEvents(lk, currentRoom);

      await currentRoom.connect(payload.url, payload.token);
      await currentRoom.localParticipant.setMicrophoneEnabled(true);

      micEnabled = true;
      outputMuted = false;
      setButtons(true);
      renderConnectedStatus();
      logLine(text('webui.chatkit.connected', 'Voice session connected'));
    } catch (error) {
      const message = error && error.message ? error.message : String(error);
      logLine(message, 'error');
      showToast?.(message, 'error');
      setStatus(
        'is-error',
        text('webui.chatkit.statusError', '连接失败'),
        text('webui.chatkit.errorText', '连接没有建立成功，请检查麦克风权限后重试。'),
      );
      await teardownSession(false);
    } finally {
      if (startVoiceBtn && !room) startVoiceBtn.disabled = false;
    }
  };

  const togglePause = async () => {
    if (!room) return;
    micEnabled = !micEnabled;
    await room.localParticipant.setMicrophoneEnabled(micEnabled);
    setButtons(true);
    renderConnectedStatus();
    logLine(
      micEnabled
        ? text('webui.chatkit.sessionResumed', 'Voice session resumed')
        : text('webui.chatkit.sessionPaused', 'Voice session paused'),
      'warn',
    );
  };

  const toggleOutputMute = () => {
    if (!room) return;
    outputMuted = !outputMuted;
    if (audioRoot) {
      audioRoot.querySelectorAll('audio').forEach((node) => {
        node.muted = outputMuted;
      });
    }
    setButtons(true);
    renderConnectedStatus();
    logLine(
      outputMuted
        ? text('webui.chatkit.outputMuted', 'Speaker muted')
        : text('webui.chatkit.outputUnmuted', 'Speaker unmuted'),
      'warn',
    );
  };

  const handlePrimaryAction = async () => {
    if (!room) {
      await startSession();
      return;
    }
    await togglePause();
  };

  const startFreshSession = async () => {
    if (!room) return;
    logLine(text('webui.chatkit.startingNewSession', 'Starting a new voice session...'));
    await teardownSession(true);
    await startSession();
  };

  startVoiceBtn?.addEventListener('click', () => {
    void handlePrimaryAction();
  });
  muteVoiceBtn?.addEventListener('click', toggleOutputMute);
  newSessionBtn?.addEventListener('click', () => {
    void startFreshSession();
  });
  clearVoiceLogBtn?.addEventListener('click', () => {
    if (voiceLog) voiceLog.innerHTML = '';
    syncVoiceLogEmptyState();
  });
  chatkitPanelToggle?.addEventListener('click', () => {
    mobilePanelOpen = !mobilePanelOpen;
    syncMobilePanel();
  });
  document.addEventListener('click', (event) => {
    if (!isMobileLayout() || !mobilePanelOpen) return;
    const target = event.target;
    if (!(target instanceof Node)) return;
    if (chatkitPanelToggle?.contains(target) || chatkitPanel?.contains(target)) return;
    mobilePanelOpen = false;
    syncMobilePanel();
  });

  window.addEventListener('beforeunload', () => {
    if (room) void room.disconnect();
  });
  window.addEventListener('resize', () => {
    if (!isMobileLayout()) mobilePanelOpen = false;
    syncMobilePanel();
  });

  resetSessionMeta();
  setButtons(false);
  setStatus(
    'is-idle',
    text('webui.chatkit.statusIdle', '未连接'),
    text('webui.chatkit.idleText', '准备好后点击开始，授权麦克风即可进入语音会话。'),
  );
  syncVoiceLogEmptyState();
  syncMobilePanel();
  if (typeof renderWebuiHeader === 'function') {
    void renderWebuiHeader();
  }
})();
