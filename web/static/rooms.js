// rooms.js — mesh WebRTC client for /rooms/{id}
//
// Lazy media policy:
//   - No getUserMedia on join. Camera light + mic indicator stay OFF until
//     the user explicitly clicks the toggle.
//   - First click for a kind requests permission, acquires that track, and
//     attaches it to every open PeerConnection (triggers renegotiation).
//   - Toggling OFF stops the track and removes it from every PC — the
//     device is fully released, not just .enabled = false.
//   - Camera and screenshare are INDEPENDENT senders: enabling screen does
//     not replace the camera track. Each lives in its own MediaStream so
//     remote peers see two distinct tiles (camera + screen) and can pick
//     either one to bring to the center stage.
//   - A `meta` signal envelope carries the {streamID → 'screen'|'camera'}
//     map so receivers can label incoming streams correctly. Without it
//     every remote stream defaults to 'camera'.
//
// Server-side identity:
//   - participant key = "u:<userID>" (auth) | "g:<guestID>" (invite guest)
//   - signaling SSE pushes {kind, from, payload} envelopes
//   - presence / chat / admin fragment updates flow over the datastar room
//     SSE stream that the page opens via a hidden data-init div
(() => {
  const root = document.querySelector('.rooms-room');
  if (!root) return;

  const roomID = root.dataset.roomId;
  const communitySlug = root.dataset.communitySlug;
  const roomBase = `/c/${encodeURIComponent(communitySlug)}/rooms/${encodeURIComponent(roomID)}`;
  const myKey = root.dataset.myKey;
  const myName = root.dataset.myName || 'me';
  let iceServers = [];
  try { iceServers = JSON.parse(root.dataset.iceServers || '[]'); } catch { /* noop */ }
  if (iceServers.length === 0) {
    iceServers = [{ urls: 'stun:stun.l.google.com:19302' }];
  }
  // Force-relay (iceTransportPolicy:'relay') makes every PC use ONLY the TURN
  // relay — last resort when direct/STUN paths are unreliable. Requires a
  // working TURN server or no media flows at all, so honour it only when the
  // server actually shipped a turn:/turns: entry.
  const hasTurn = iceServers.some(s => []
    .concat(s.urls || [])
    .some(u => /^turns?:/i.test(u)));
  const forceRelay = root.dataset.forceRelay === 'true' && hasTurn;
  const pcConfig = forceRelay
    ? { iceServers, iceTransportPolicy: 'relay' }
    : { iceServers };

  const videoGrid = root.querySelector('[data-video-grid]');
  const stage = root.querySelector('[data-rooms-stage]');
  const stageVideo = root.querySelector('[data-rooms-stage-video]');
  const stageEmpty = root.querySelector('[data-rooms-stage-empty]');
  const stageLabel = root.querySelector('[data-rooms-stage-label]');
  const stageFullscreenBtn = root.querySelector('[data-rooms-stage-fullscreen]');
  const micBtn = root.querySelector('[data-rooms-mic]');
  const camBtn = root.querySelector('[data-rooms-cam]');
  const screenBtn = root.querySelector('[data-rooms-screen]');
  const blurBtn = root.querySelector('[data-rooms-blur]');
  const leaveBtn = root.querySelector('[data-rooms-leave]');
  const devicesWrap = root.querySelector('[data-rooms-devices]');
  const devicesBtn = root.querySelector('[data-rooms-devices-btn]');
  const devicesPanel = root.querySelector('[data-rooms-devices-panel]');
  const micSelect = root.querySelector('[data-rooms-mic-select]');
  const camSelect = root.querySelector('[data-rooms-cam-select]');

  // ----- input device preferences -----------------------------------------
  //
  // Which mic / camera to capture. Empty string = let the browser pick its
  // default (the original behaviour). Persisted to localStorage so the choice
  // survives reload, same as the blur toggle above.
  const devicePrefs = {
    audio: readDevicePref('rooms.micDeviceId'),
    video: readDevicePref('rooms.camDeviceId'),
  };
  function devicePrefKey(kind) {
    return kind === 'audio' ? 'rooms.micDeviceId' : 'rooms.camDeviceId';
  }
  function readDevicePref(key) {
    try { return localStorage.getItem(key) || ''; } catch { return ''; }
  }
  function setDevicePref(kind, id) {
    devicePrefs[kind] = id;
    try {
      if (id) localStorage.setItem(devicePrefKey(kind), id);
      else localStorage.removeItem(devicePrefKey(kind));
    } catch { /* noop */ }
  }
  function mediaConstraints(kind) {
    if (kind === 'audio') {
      const id = devicePrefs.audio;
      return { audio: id ? { deviceId: { exact: id } } : true };
    }
    const video = { width: 640, height: 480 };
    if (devicePrefs.video) video.deviceId = { exact: devicePrefs.video };
    return { video };
  }

  // Background blur is ON by default — the camera stream feeds MediaPipe
  // Selfie Segmentation and remote peers receive the composited
  // "person sharp, background blurred" stream. Persisted to localStorage
  // so the choice survives reload. Toggle off for raw camera.
  let blurEnabled = (() => {
    try { return localStorage.getItem('rooms.blur') !== 'off'; }
    catch { return true; }
  })();
  let blurController = null; // {stop()} from rooms-blur.wrap when active

  // ----- tile registry -----------------------------------------------------
  //
  // A "tile" is one visible video panel in the strip. Each tile is keyed by
  // a stable string id and owns its own stream so the stage can pick any
  // one independently. Self has up to two tiles (camera + screen). Each
  // peer has one tile per remote stream id we've seen.
  //
  //   tileID  := "self:camera" | "self:screen" | "<peerKey>:<streamID>"
  //   tile     = { card, video, label, name, role, peerKey, getStream }
  //
  // getStream() is called when the stage focuses this tile — it returns
  // the current MediaStream for that tile (lets us swap in late-arriving
  // streams without a stale reference).

  const tiles = new Map();
  let focusedTileID = null;

  function tileIdSelf(role) { return `self:${role}`; }
  function tileIdPeer(peerKey, streamID) { return `${peerKey}:${streamID}`; }

  function makeTile({ id, name, role, peerKey, isLocal, getStream }) {
    const card = document.createElement('div');
    card.className = 'rooms-video-tile'
      + (isLocal ? ' rooms-video-tile-self' : '')
      + (role === 'screen' ? ' rooms-video-tile-screen' : '');
    card.dataset.tileId = id;
    card.title = `Click to focus ${name} on the center stage`;
    const video = document.createElement('video');
    video.autoplay = true;
    video.playsInline = true;
    if (isLocal) video.muted = true;
    const label = document.createElement('div');
    label.className = 'rooms-video-label';
    label.textContent = name + (role === 'screen' ? ' • screen' : '');
    if (role === 'screen') {
      const icon = document.createElement('span');
      icon.className = 'rooms-video-screen-icon';
      icon.setAttribute('aria-hidden', 'true');
      icon.textContent = '🖥';
      card.appendChild(icon);
    }
    card.appendChild(video);
    card.appendChild(label);
    card.addEventListener('click', () => focusTile(id));
    const tile = { id, card, video, label, name, role, peerKey, isLocal, getStream };
    tiles.set(id, tile);
    videoGrid.appendChild(card);
    return tile;
  }

  function removeTile(id) {
    const tile = tiles.get(id);
    if (!tile) return;
    tile.card.remove();
    tiles.delete(id);
    if (focusedTileID === id) clearStage();
  }

  function relabelTile(id, name, role) {
    const tile = tiles.get(id);
    if (!tile) return;
    tile.name = name;
    if (role) tile.role = role;
    tile.label.textContent = tile.name + (tile.role === 'screen' ? ' • screen' : '');
    tile.card.classList.toggle('rooms-video-tile-screen', tile.role === 'screen');
    if (focusedTileID === id) {
      if (stageLabel) stageLabel.textContent = tile.name + (tile.role === 'screen' ? ' • screen' : '');
    }
  }

  function focusTile(id) {
    const tile = tiles.get(id);
    if (!tile) return;
    focusedTileID = id;
    videoGrid.querySelectorAll('.rooms-video-tile').forEach(el => {
      el.classList.toggle('focused', el.dataset.tileId === id);
    });
    const stream = tile.getStream();
    stageVideo.srcObject = stream || null;
    stageVideo.classList.toggle('empty', !stream);
    stageEmpty?.classList.toggle('hidden', !!stream);
    if (stageLabel) stageLabel.textContent = stream
      ? (tile.name + (tile.role === 'screen' ? ' • screen' : ''))
      : '';
    if (stream) stageVideo.play?.().catch(() => {});
  }

  function clearStage() {
    focusedTileID = null;
    stageVideo.srcObject = null;
    stageVideo.classList.add('empty');
    stageEmpty?.classList.remove('hidden');
    if (stageLabel) stageLabel.textContent = '';
  }

  function refreshStageIfFocused(id) {
    if (focusedTileID === id) focusTile(id);
  }

  // ----- self media tracking -----------------------------------------------

  // Each kind tracks: { track, stream } when on; null when off.
  const local = { audio: null, video: null };
  let screenStream = null; // active MediaStream for screenshare or null

  // Self camera tile always exists; its stream is just the local camera
  // MediaStream when the camera is on, null otherwise.
  function getSelfCameraStream() {
    return local.video?.stream || null;
  }
  function getSelfScreenStream() {
    return screenStream || null;
  }

  makeTile({
    id: tileIdSelf('camera'),
    name: myName + ' (you)',
    role: 'camera',
    peerKey: myKey,
    isLocal: true,
    getStream: getSelfCameraStream,
  });

  function ensureSelfScreenTile() {
    if (tiles.has(tileIdSelf('screen'))) return;
    makeTile({
      id: tileIdSelf('screen'),
      name: myName + ' (you)',
      role: 'screen',
      peerKey: myKey,
      isLocal: true,
      getStream: getSelfScreenStream,
    });
    const tile = tiles.get(tileIdSelf('screen'));
    if (tile) tile.video.srcObject = screenStream;
  }

  function refreshSelfCameraPreview() {
    const tile = tiles.get(tileIdSelf('camera'));
    if (!tile) return;
    tile.video.srcObject = getSelfCameraStream();
    refreshStageIfFocused(tile.id);
  }

  // ----- peer plumbing -----------------------------------------------------

  const peers = new Map();
  // peer entry:
  //   pc, audio, name,
  //   senders: { audio, video, screen },        // RTCRtpSender refs
  //   tilesByStream: Map<streamID, tileID>,     // remote stream → tile we created for it
  //   roles: Map<streamID, 'camera'|'screen'>,  // populated from meta envelopes
  //   makingOffer, ignoreOffer, ...

  const polite = (otherKey) => otherKey > myKey;
  let heartbeatTimer = null;
  let leaving = false;

  function ensurePeer(key, name) {
    let entry = peers.get(key);
    if (entry) return entry;
    const pc = new RTCPeerConnection(pcConfig);
    const audio = document.createElement('audio');
    audio.autoplay = true;
    audio.style.display = 'none';
    document.body.appendChild(audio);

    entry = {
      pc, audio, name,
      senders: { audio: null, video: null, screen: null },
      tilesByStream: new Map(),
      roles: new Map(),
      makingOffer: false,
      ignoreOffer: false,
      isSettingRemoteAnswerPending: false,
      iAmPolite: polite(key),
    };
    peers.set(key, entry);

    if (local.audio) entry.senders.audio = pc.addTrack(local.audio.track, local.audio.stream);
    if (local.video) entry.senders.video = pc.addTrack(local.video.track, local.video.stream);
    if (screenStream) {
      const screenTrack = screenStream.getVideoTracks()[0];
      if (screenTrack) entry.senders.screen = pc.addTrack(screenTrack, screenStream);
    }

    pc.ontrack = (ev) => {
      const sid = ev.streams[0]?.id || `track-${ev.track.id}`;
      console.log('[rooms] ontrack', { key, kind: ev.track.kind, sid });
      if (ev.track.kind === 'audio') {
        if (!(entry.audio.srcObject instanceof MediaStream)) {
          entry.audio.srcObject = new MediaStream();
        }
        entry.audio.srcObject.addTrack(ev.track);
        const tryPlay = () => entry.audio.play?.()
          .catch((err) => console.warn('[rooms] audio play blocked', key, err?.name));
        tryPlay();
        setTimeout(tryPlay, 250);
        armGlobalAudioRecovery();
        const dropAudio = () => {
          try { entry.audio.srcObject?.removeTrack(ev.track); } catch {}
        };
        ev.track.addEventListener('mute', dropAudio);
        ev.track.addEventListener('ended', dropAudio);
        return;
      }
      // video → one tile per remote stream id
      let tileID = entry.tilesByStream.get(sid);
      if (!tileID) {
        const role = entry.roles.get(sid) || 'camera';
        tileID = tileIdPeer(key, sid);
        const remoteStream = ev.streams[0] || new MediaStream();
        makeTile({
          id: tileID,
          name: entry.name,
          role,
          peerKey: key,
          isLocal: false,
          getStream: () => remoteStream,
        });
        const tile = tiles.get(tileID);
        if (tile) tile.video.srcObject = remoteStream;
        entry.tilesByStream.set(sid, tileID);
      }
      const tile = tiles.get(tileID);
      if (tile?.video?.srcObject !== ev.streams[0] && ev.streams[0]) {
        tile.video.srcObject = ev.streams[0];
      }
      tile?.video?.play?.().catch(() => {});
      refreshStageIfFocused(tileID);
      // Auto-promote a screen stream to the stage (overrides camera focus).
      const role = entry.roles.get(sid) || 'camera';
      if (role === 'screen' && focusedTileID !== tileID) {
        focusTile(tileID);
      } else if (!focusedTileID || focusedTileID === tileIdSelf('camera') || focusedTileID === tileIdSelf('screen')) {
        focusTile(tileID);
      }
      const drop = () => {
        try { ev.streams[0]?.removeTrack(ev.track); } catch {}
        const ms = ev.streams[0];
        if (!ms || ms.getVideoTracks().length === 0) {
          removeTile(tileID);
          entry.tilesByStream.delete(sid);
        } else {
          refreshStageIfFocused(tileID);
        }
      };
      ev.track.addEventListener('mute', drop);
      ev.track.addEventListener('ended', drop);
    };
    pc.onicecandidate = (ev) => {
      if (!ev.candidate) return;
      // Diagnostic: a "typ relay" candidate proves the TURN server granted an
      // allocation. If you never see this line, TURN is the problem (creds,
      // external-ip, blocked ports) — not the browser.
      if (/ typ relay /.test(' ' + ev.candidate.candidate + ' ')) {
        console.log('[rooms] TURN relay candidate ✓', key);
      }
      sendSignal(key, 'ice', JSON.stringify(ev.candidate));
    };
    pc.oniceconnectionstatechange = () => {
      console.log('[rooms] ice state', key, pc.iceConnectionState);
      if (pc.iceConnectionState === 'failed') {
        try { pc.restartIce?.(); } catch {}
      }
    };
    pc.onconnectionstatechange = () => {
      console.log('[rooms] pc state', key, pc.connectionState);
    };
    pc.onnegotiationneeded = async () => {
      try {
        entry.makingOffer = true;
        const offer = await pc.createOffer();
        await pc.setLocalDescription(offer);
        sendSignal(key, 'offer', JSON.stringify(pc.localDescription));
      } catch (e) {
        console.warn('negotiation failed', e);
      } finally {
        entry.makingOffer = false;
      }
    };

    // Brand-new peer needs to learn about any roles we already publish
    // (screensharing) so they can label our incoming streams correctly.
    queueMicrotask(() => sendMetaTo(key));

    return entry;
  }

  function closePeer(key) {
    const entry = peers.get(key);
    if (!entry) return;
    try { entry.pc.close(); } catch {}
    entry.audio?.remove();
    for (const tileID of entry.tilesByStream.values()) removeTile(tileID);
    peers.delete(key);
  }

  // ----- meta signaling: stream → role map ---------------------------------

  function selfStreamRoleMap() {
    const m = {};
    if (local.video?.stream) m[local.video.stream.id] = 'camera';
    if (screenStream) m[screenStream.id] = 'screen';
    return m;
  }

  function sendMetaTo(peerKey) {
    if (leaving) return;
    sendSignal(peerKey, 'meta', JSON.stringify({ streams: selfStreamRoleMap() }));
  }

  function broadcastMeta() {
    for (const key of peers.keys()) sendMetaTo(key);
  }

  // ----- enable / disable media -------------------------------------------

  async function enableMedia(kind) {
    if (local[kind]) return true;
    if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
      flashMediaError(kind, 'Camera/microphone require HTTPS (or localhost).');
      return false;
    }
    let rawStream;
    try {
      rawStream = await navigator.mediaDevices.getUserMedia(mediaConstraints(kind));
    } catch (e) {
      // A pinned deviceId can become invalid (device unplugged, permissions
      // reset) → Overconstrained/NotFound. Drop the pin and retry once with
      // the browser default before surfacing an error.
      if (devicePrefs[kind] && (e?.name === 'OverconstrainedError' || e?.name === 'NotFoundError')) {
        setDevicePref(kind, '');
        try { rawStream = await navigator.mediaDevices.getUserMedia(mediaConstraints(kind)); }
        catch (e2) { e = e2; }
      }
      if (!rawStream) {
        console.warn('[rooms] getUserMedia denied', kind, e);
        flashMediaError(kind, e?.name ? `${e.name}: ${e.message || ''}` : String(e));
        return false;
      }
    }

    // For camera: optionally run through background blur. The wrapped
    // stream's track is what we expose to peers; the raw stream is kept
    // so we can fully release the device on disable.
    let publishStream = rawStream;
    if (kind === 'video' && blurEnabled && window.fcRoomsBlur) {
      try {
        const wrapped = await window.fcRoomsBlur.wrap(rawStream, { blurPx: 12 });
        publishStream = wrapped.stream;
        blurController = wrapped;
      } catch (e) {
        console.warn('[rooms] background blur failed, falling back to raw camera', e);
        blurController = null;
      }
    }

    const track = publishStream.getTracks().find(t => t.kind === kind);
    if (!track) { publishStream.getTracks().forEach(t => t.stop()); return false; }
    local[kind] = { track, stream: publishStream, rawStream };

    for (const entry of peers.values()) {
      const sender = entry.pc.addTrack(track, publishStream);
      entry.senders[kind] = sender;
    }
    if (kind === 'video') {
      refreshSelfCameraPreview();
      broadcastMeta();
    }
    // Labels are only exposed once permission is granted — refresh the
    // picker so the dropdowns show real device names, not "Microphone 1".
    populateDeviceSelects();
    return true;
  }

  async function disableMedia(kind) {
    const cur = local[kind];
    if (!cur) return;
    cur.track.stop();
    cur.stream.getTracks().forEach(t => t.stop());
    // The raw camera stream (pre-blur) lives separately so we can fully
    // release the device — without this, the camera LED stayed on after
    // toggling off because the wrapper held the only reference.
    cur.rawStream?.getTracks().forEach(t => t.stop());
    local[kind] = null;
    if (kind === 'video' && blurController) {
      try { blurController.stop(); } catch {}
      blurController = null;
    }
    for (const entry of peers.values()) {
      const sender = entry.senders[kind];
      if (sender) {
        try { entry.pc.removeTrack(sender); } catch {}
        entry.senders[kind] = null;
      }
    }
    if (kind === 'video') {
      refreshSelfCameraPreview();
      broadcastMeta();
    }
  }

  micBtn?.addEventListener('click', async () => {
    if (local.audio) { await disableMedia('audio'); syncToggleLabel(micBtn, 'mic', false); }
    else { const ok = await enableMedia('audio'); syncToggleLabel(micBtn, 'mic', ok); }
  });
  camBtn?.addEventListener('click', async () => {
    if (local.video) { await disableMedia('video'); syncToggleLabel(camBtn, 'cam', false); }
    else { const ok = await enableMedia('video'); syncToggleLabel(camBtn, 'cam', ok); }
  });

  syncToggleLabel(micBtn, 'mic', false);
  syncToggleLabel(camBtn, 'cam', false);
  syncBlurLabel();

  function syncToggleLabel(btn, kind, on) {
    if (!btn) return;
    const icons = { mic: '🎙', cam: '🎥' };
    btn.classList.toggle('on', on);
    btn.classList.toggle('off', !on);
    btn.textContent = `${icons[kind]} ${on ? 'on' : 'off'}`;
    btn.title = (kind === 'mic' ? 'Microphone' : 'Camera') + (on ? ' (on)' : ' (off)');
  }

  function syncBlurLabel() {
    if (!blurBtn) return;
    blurBtn.classList.toggle('on', blurEnabled);
    blurBtn.classList.toggle('off', !blurEnabled);
    blurBtn.textContent = blurEnabled ? '🌫 blur' : '🌫 off';
    blurBtn.title = `Background blur (${blurEnabled ? 'on' : 'off'}) — click to toggle`;
  }

  // Toggle blur: persist the choice and, if the camera is already on,
  // tear it down + bring it back up so peers see the new pipeline.
  blurBtn?.addEventListener('click', async () => {
    blurEnabled = !blurEnabled;
    try { localStorage.setItem('rooms.blur', blurEnabled ? 'on' : 'off'); } catch {}
    syncBlurLabel();
    if (local.video) {
      const wasOn = true;
      await disableMedia('video');
      const ok = await enableMedia('video');
      syncToggleLabel(camBtn, 'cam', ok && wasOn);
    }
  });

  // ----- device picker -----------------------------------------------------
  //
  // Two dropdowns (mic + camera) in a popover. Selecting a device pins it for
  // future getUserMedia calls; if that kind is already live we re-acquire so
  // the switch takes effect immediately (same tear-down/bring-up the blur
  // toggle uses).

  function fillDeviceSelect(sel, list, current, noun) {
    if (!sel) return;
    sel.innerHTML = '';
    const def = document.createElement('option');
    def.value = '';
    def.textContent = `Default ${noun.toLowerCase()}`;
    sel.appendChild(def);
    list.forEach((d, i) => {
      const o = document.createElement('option');
      o.value = d.deviceId;
      o.textContent = d.label || `${noun} ${i + 1}`;
      sel.appendChild(o);
    });
    sel.value = [...sel.options].some(o => o.value === current) ? current : '';
  }

  async function populateDeviceSelects() {
    if (!micSelect && !camSelect) return;
    if (!navigator.mediaDevices?.enumerateDevices) return;
    let devs;
    try { devs = await navigator.mediaDevices.enumerateDevices(); }
    catch { return; }
    fillDeviceSelect(micSelect, devs.filter(d => d.kind === 'audioinput'), devicePrefs.audio, 'Microphone');
    fillDeviceSelect(camSelect, devs.filter(d => d.kind === 'videoinput'), devicePrefs.video, 'Camera');
  }

  async function applyDeviceChange(kind, id) {
    setDevicePref(kind, id);
    if (!local[kind]) return; // not live — picked device used on next enable
    const btn = kind === 'audio' ? micBtn : camBtn;
    const label = kind === 'audio' ? 'mic' : 'cam';
    await disableMedia(kind);
    const ok = await enableMedia(kind);
    syncToggleLabel(btn, label, ok);
  }

  micSelect?.addEventListener('change', () => applyDeviceChange('audio', micSelect.value));
  camSelect?.addEventListener('change', () => applyDeviceChange('video', camSelect.value));

  function closeDevicesPanel() {
    devicesPanel?.setAttribute('hidden', '');
    devicesBtn?.classList.remove('on');
  }
  devicesBtn?.addEventListener('click', (ev) => {
    ev.stopPropagation();
    if (!devicesPanel) return;
    if (devicesPanel.hasAttribute('hidden')) {
      devicesPanel.removeAttribute('hidden');
      devicesBtn.classList.add('on');
      populateDeviceSelects();
    } else {
      closeDevicesPanel();
    }
  });
  document.addEventListener('click', (ev) => {
    if (!devicesWrap || devicesPanel?.hasAttribute('hidden')) return;
    if (!devicesWrap.contains(ev.target)) closeDevicesPanel();
  });
  navigator.mediaDevices?.addEventListener?.('devicechange', () => {
    if (devicesPanel && !devicesPanel.hasAttribute('hidden')) populateDeviceSelects();
  });
  populateDeviceSelects();

  // ----- screenshare -------------------------------------------------------
  //
  // Independent of camera: addTrack a new screen video sender on every peer,
  // make the self screen tile, and broadcast a meta envelope so peers label
  // the incoming stream as 'screen'. Toggling off removes the sender and
  // the tile but leaves the camera untouched.

  screenBtn?.addEventListener('click', async () => {
    if (screenStream) { await stopScreenshare(); return; }
    try {
      screenStream = await navigator.mediaDevices.getDisplayMedia({ video: true, audio: false });
    } catch { return; }
    const screenTrack = screenStream.getVideoTracks()[0];
    if (!screenTrack) { screenStream = null; return; }
    screenTrack.addEventListener('ended', () => { stopScreenshare(); });

    for (const entry of peers.values()) {
      const sender = entry.pc.addTrack(screenTrack, screenStream);
      entry.senders.screen = sender;
    }
    ensureSelfScreenTile();
    broadcastMeta();
    screenBtn.classList.add('on');
    // Auto-focus our own screen tile so the user sees what they are sharing.
    focusTile(tileIdSelf('screen'));
  });

  async function stopScreenshare() {
    if (!screenStream) return;
    screenStream.getTracks().forEach(t => t.stop());
    screenStream = null;
    for (const entry of peers.values()) {
      const sender = entry.senders.screen;
      if (sender) {
        try { entry.pc.removeTrack(sender); } catch {}
        entry.senders.screen = null;
      }
    }
    removeTile(tileIdSelf('screen'));
    broadcastMeta();
    screenBtn.classList.remove('on');
  }

  // ----- stage fullscreen --------------------------------------------------

  stageFullscreenBtn?.addEventListener('click', () => toggleStageFullscreen());
  stage?.addEventListener('dblclick', () => toggleStageFullscreen());
  document.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape' && document.fullscreenElement === stage) {
      document.exitFullscreen?.();
    }
    if (ev.key === 'f' && !ev.target.matches('input, textarea')) {
      toggleStageFullscreen();
    }
  });

  function toggleStageFullscreen() {
    if (!stage) return;
    if (document.fullscreenElement) {
      document.exitFullscreen?.();
    } else {
      (stage.requestFullscreen?.() ?? Promise.reject()).catch(() => {});
    }
  }

  // ----- leave -------------------------------------------------------------

  leaveBtn?.addEventListener('click', () => {
    leaving = true;
    teardown();
    fetch(`${roomBase}/leave`, { method: 'POST', keepalive: true })
      .finally(() => { window.location.href = `/c/${encodeURIComponent(communitySlug)}/rooms`; });
  });

  window.addEventListener('beforeunload', () => {
    leaving = true;
    navigator.sendBeacon?.(`${roomBase}/leave`);
  });
  window.addEventListener('pagehide', () => {
    leaving = true;
    navigator.sendBeacon?.(`${roomBase}/leave`);
  });

  function teardown() {
    if (heartbeatTimer) clearInterval(heartbeatTimer);
    for (const k of [...peers.keys()]) closePeer(k);
    disableMedia('audio');
    disableMedia('video');
    screenStream?.getTracks().forEach(t => t.stop());
  }

  // ----- heartbeat ---------------------------------------------------------

  function startHeartbeat() {
    const ping = () => {
      if (leaving) return;
      fetch(`${roomBase}/ping`, { method: 'POST', keepalive: true }).catch(() => {});
    };
    ping();
    heartbeatTimer = setInterval(ping, 10000);
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'visible') ping();
    });
    window.addEventListener('focus', ping);
    window.addEventListener('online', ping);
  }

  // ----- signaling ---------------------------------------------------------

  window.__roomsSignal = async (msg) => {
    if (!msg || !msg.kind) return;
    if (msg.kind === 'hello') return;
    if (msg.kind === 'bye') { closePeer(msg.from); return; }
    const entry = ensurePeer(msg.from, msg.from);
    const pc = entry.pc;
    try {
      if (msg.kind === 'offer' || msg.kind === 'answer') {
        const desc = JSON.parse(msg.payload);
        const readyForOffer =
          !entry.makingOffer &&
          (pc.signalingState === 'stable' || entry.isSettingRemoteAnswerPending);
        const offerCollision = desc.type === 'offer' && !readyForOffer;
        entry.ignoreOffer = !entry.iAmPolite && offerCollision;
        if (entry.ignoreOffer) return;
        entry.isSettingRemoteAnswerPending = desc.type === 'answer';
        await pc.setRemoteDescription(desc);
        entry.isSettingRemoteAnswerPending = false;
        if (desc.type === 'offer') {
          const answer = await pc.createAnswer();
          await pc.setLocalDescription(answer);
          sendSignal(msg.from, 'answer', JSON.stringify(pc.localDescription));
        }
      } else if (msg.kind === 'ice') {
        try { await pc.addIceCandidate(JSON.parse(msg.payload)); }
        catch (e) { if (!entry.ignoreOffer) throw e; }
      } else if (msg.kind === 'meta') {
        const parsed = JSON.parse(msg.payload || '{}');
        const streams = parsed?.streams || {};
        // Update roles map for this peer and relabel any tiles already
        // bound to those streams.
        for (const [sid, role] of Object.entries(streams)) {
          entry.roles.set(sid, role);
          const tileID = entry.tilesByStream.get(sid);
          if (tileID) relabelTile(tileID, entry.name, role);
        }
      }
    } catch (e) { console.warn('signal handle failed', msg.kind, e); }
  };

  function sendSignal(to, kind, payload) {
    if (leaving) return;
    fetch(`${roomBase}/signal/send`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ to, kind, payload }),
    }).catch(() => {});
  }

  // ----- peer discovery via presence DOM -----------------------------------

  const peoplePanel = root.querySelector('[data-rooms-people]');
  if (peoplePanel) {
    const obs = new MutationObserver(reconcilePeers);
    obs.observe(peoplePanel, { childList: true, subtree: true });
    reconcilePeers();
  }

  function reconcilePeers() {
    const wanted = new Map();
    root.querySelectorAll('.rooms-participants li[data-key]').forEach((li) => {
      const key = li.getAttribute('data-key');
      const name = li.querySelector('.rooms-people-name')?.textContent?.trim() || 'peer';
      if (key && key !== myKey) wanted.set(key, name);
    });
    for (const [key, name] of wanted) {
      const existing = peers.get(key);
      if (!existing) {
        ensurePeer(key, name);
      } else if (existing.name !== name) {
        existing.name = name;
        for (const tileID of existing.tilesByStream.values()) {
          const t = tiles.get(tileID);
          if (t) relabelTile(tileID, name, t.role);
        }
      }
    }
    for (const key of [...peers.keys()]) {
      if (!wanted.has(key)) closePeer(key);
    }
  }

  // Browsers block <audio>.play() until the user has interacted with the
  // page. We arm a single click listener that replays every peer audio
  // element on the first click.
  function armGlobalAudioRecovery() {
    if (window.__roomsAudioRecoveryArmed) return;
    window.__roomsAudioRecoveryArmed = true;
    const replay = () => {
      document.querySelectorAll('audio').forEach((a) => {
        if (a.srcObject instanceof MediaStream && a.srcObject.getTracks().length > 0) {
          a.play?.().catch(() => {});
        }
      });
    };
    document.addEventListener('click', replay, { once: false });
    document.addEventListener('keydown', replay, { once: false });
    document.addEventListener('touchstart', replay, { once: false });
  }

  function flashMediaError(kind, msg) {
    const host = root.querySelector('.rooms-toolbar') || root;
    let banner = root.querySelector('.rooms-media-error');
    if (!banner) {
      banner = document.createElement('div');
      banner.className = 'rooms-media-error';
      banner.style.cssText = 'color:#b00;background:#fee;border:1px solid #f99;padding:6px 10px;margin:6px 0;border-radius:6px;font-size:13px;';
      host.parentNode?.insertBefore(banner, host.nextSibling);
    }
    banner.textContent = `${kind === 'audio' ? 'Microphone' : 'Camera'} unavailable — ${msg}`;
    clearTimeout(banner._t);
    banner._t = setTimeout(() => { banner.remove(); }, 8000);
  }

  // ----- boot --------------------------------------------------------------
  startHeartbeat();
})();
