(() => {
  'use strict';

  const video    = document.getElementById('video');
  const statusEl = document.getElementById('status');
  const badge    = document.getElementById('mode-badge');

  const WEBRTC_TIMEOUT_MS = 6000;

  const state = {
    mode: 'webrtc',   // 'webrtc' | 'hls' | 'connecting'
    rid: 'h',
    pc: null,
    hls: null,
    timeout: null,
  };

  // ---- Boot diagnostics -------------------------------------------
  const btns = Array.from(document.querySelectorAll('button'));
  console.log('[player] boot: buttons =', btns.length);
  btns.forEach(b => console.log('[player]   btn:', JSON.stringify({
    id: b.id, rid: b.dataset.rid, text: b.textContent.trim()
  })));

  function log(msg) {
    console.log('[player]', msg);
    if (statusEl) statusEl.textContent = msg;
  }

  function setBadge(mode) {
    if (!badge) return;
    badge.textContent = mode;
    badge.style.background = mode === 'webrtc' ? '#2a5'
                          : mode === 'hls'    ? '#a52'
                          : '#444';
  }

  // Toggle button can be found by id or by text, so hand-written HTML works.
  function findToggleButton() {
    const byId = document.getElementById('toggle-mode');
    if (byId) return byId;
    return btns.find(b => b.textContent.trim().toLowerCase().startsWith('cambiar')) || null;
  }

  function updateModeButton() {
    const btn = findToggleButton();
    if (!btn) return;
    if (state.mode === 'hls') {
      btn.textContent = 'Cambiar a WebRTC';
    } else if (state.mode === 'webrtc') {
      btn.textContent = 'Cambiar a HLS';
    } else {
      btn.textContent = 'Conectando…';
    }
  }

  function highlightRID(rid) {
    document.querySelectorAll('[data-rid]').forEach(b => {
      b.classList.toggle('active', b.dataset.rid === rid);
    });
  }

    function teardownWebRTC() {
      clearTimeout(state.timeout); state.timeout = null;
      const pc = state.pc;
      state.pc = null;               // clear BEFORE close, so the state
      if (pc) { try { pc.close(); } catch (_) {} }   // handler ignores it
      video.srcObject = null;
    }

  function teardownHLS() {
    if (state.hls) { try { state.hls.destroy(); } catch (_) {} state.hls = null; }
    video.removeAttribute('src');
    video.load();
  }

  // ---- WebRTC -----------------------------------------------------
  async function startWebRTC(rid) {
    teardownWebRTC();
    teardownHLS();
    state.mode = 'connecting';
    setBadge('conectando webrtc');
    updateModeButton();
    log(`WebRTC: pidiendo capa ${rid}…`);

    const pc = new RTCPeerConnection({
      iceServers: [{ urls: 'stun:stun.l.google.com:19302' }],
    });
    state.pc = pc;
    pc.addTransceiver('video', { direction: 'recvonly' });

    pc.ontrack = (ev) => {
      clearTimeout(state.timeout); state.timeout = null;
      log(`WebRTC: reproduciendo (${rid})`);
      state.mode = 'webrtc';
      setBadge('webrtc');
      updateModeButton();
      video.srcObject = ev.streams[0] || new MediaStream([ev.track]);
      video.play().catch(err => log(`play() failed: ${err.message}`));
    };

    pc.onconnectionstatechange = () => {
      if (state.pc !== pc) return;   // stale handler, ignore
      log(`WebRTC estado: ${pc.connectionState}`);
      if (pc.connectionState === 'failed' ||
          pc.connectionState === 'disconnected' ||
          pc.connectionState === 'closed') {
        log('WebRTC caído → fallback a HLS');
        state.pc = null;             // prevent re-entry
        startHLS();
      }
    };

    state.timeout = setTimeout(() => {
      if (state.pc === pc && !pc.getReceivers().some(r => r.track)) {
        log('WebRTC timeout → fallback a HLS');
        startHLS();
      }
    }, WEBRTC_TIMEOUT_MS);

    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);

    await new Promise(resolve => {
      if (pc.iceGatheringState === 'complete') return resolve();
      const onIce = () => {
        if (pc.iceGatheringState === 'complete') {
          pc.removeEventListener('icegatheringstatechange', onIce);
          resolve();
        }
      };
      pc.addEventListener('icegatheringstatechange', onIce);
    });

    // The Go server exchanges SessionDescription as JSON, not raw SDP.
    const res = await fetch(`/subscribe?rid=${encodeURIComponent(rid)}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        type: pc.localDescription.type,   // "offer"
        sdp:  pc.localDescription.sdp,
      }),
    });

    if (!res.ok) throw new Error(`/subscribe → ${res.status} ${res.statusText}`);

    const answer = await res.json();
    await pc.setRemoteDescription(answer);
    log('WebRTC: SDP negociado');
  }

  // ---- HLS --------------------------------------------------------
  async function startHLS() {
    try {
      const info = await (await fetch('/hls/info')).json();
      if (!info.available) {
        log('HLS no disponible todavía — no se cambia de modo');
        return;
      }
    } catch (e) {
      log(`No se pudo consultar /hls/info: ${e.message}`);
      return;
    }

    teardownWebRTC();
    teardownHLS();

    state.mode = 'hls';
    setBadge('hls');
    updateModeButton();

    const url = '/hls/master.m3u8';
    log('HLS: cargando master playlist…');

    if (window.Hls && Hls.isSupported()) {
      const hls = new Hls({ enableWorker: true });
      state.hls = hls;
      hls.loadSource(url);
      hls.attachMedia(video);

      hls.on(Hls.Events.MANIFEST_PARSED, (_, data) => {
        log(`HLS: ${data.levels.length} variante(s) — reproduciendo`);
        video.play().catch(err => log(`play() failed: ${err.message}`));
      });
      hls.on(Hls.Events.LEVEL_SWITCHED, (_, data) => {
        const lvl = hls.levels[data.level];
        log(`HLS: ABR eligió ${lvl.width}x${lvl.height} @ ${Math.round(lvl.bitrate/1000)} kbps`);
      });
      hls.on(Hls.Events.ERROR, (_, data) => {
        if (data.fatal) { log(`HLS error fatal (${data.type})`); teardownHLS(); }
        else { console.warn('[hls]', data); }
      });
    } else if (video.canPlayType('application/vnd.apple.mpegurl')) {
      video.src = url;
      video.play().catch(err => log(`play() failed: ${err.message}`));
      log('HLS: reproducción nativa');
    } else {
      log('HLS no soportado por este navegador');
    }
  }

  // ---- switchTo ---------------------------------------------------
  async function switchTo(rid, force = false) {
    state.rid = rid;
    highlightRID(rid);
    if (state.mode === 'hls' && !force) {
      log('En modo HLS el ABR gestiona la calidad automáticamente');
      return;
    }
    try {
      await startWebRTC(rid);
    } catch (e) {
      log(`WebRTC falló: ${e.message} → fallback a HLS`);
      await startHLS();
    }
  }

  // ---- Single click dispatcher -----------------------------------
  document.addEventListener('click', (e) => {
    const btn = e.target.closest('button');
    if (!btn) return;

    console.log('[player] click:', JSON.stringify({
      id: btn.id, rid: btn.dataset.rid, text: btn.textContent.trim()
    }));

    // Toggle: id="toggle-mode" or text starting with "cambiar"
    const isToggle = btn.id === 'toggle-mode' ||
      (!btn.dataset.rid && btn.textContent.trim().toLowerCase().startsWith('cambiar'));

    if (isToggle) {
      e.preventDefault();
      if (state.mode === 'hls') switchTo(state.rid, true);
      else                       startHLS();
      return;
    }

    // RID buttons
    if (btn.dataset.rid) {
      switchTo(btn.dataset.rid);
    }
  });

  // ---- Boot -------------------------------------------------------
  highlightRID(state.rid);
  updateModeButton();

  switchTo(state.rid).catch(e => {
    log(`Arranque WebRTC falló: ${e.message} → HLS`);
    startHLS();
  });
})();