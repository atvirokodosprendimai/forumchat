// rooms.js — mesh WebRTC client for /rooms/{id}
//
// Lazy media policy:
//   - No getUserMedia on join. Camera light + mic indicator stay OFF until
//     the user explicitly clicks the toggle.
//   - First click for a kind requests permission, acquires that track, and
//     attaches it to every open PeerConnection (triggers renegotiation).
//   - Toggling OFF stops the track and removes it from every PC — the
//     device is fully released, not just .enabled = false.
//   - A user who never toggles either stays in the room as a pure listener;
//     they still receive remote audio and video.
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
  const myKey = root.dataset.myKey;
  const myName = root.dataset.myName || 'me';
  let iceServers = [];
  try { iceServers = JSON.parse(root.dataset.iceServers || '[]'); } catch { /* noop */ }
  if (iceServers.length === 0) {
    iceServers = [{ urls: 'stun:stun.l.google.com:19302' }];
  }

  const videoGrid = root.querySelector('[data-video-grid]');
  const micBtn = root.querySelector('[data-rooms-mic]');
  const camBtn = root.querySelector('[data-rooms-cam]');
  const screenBtn = root.querySelector('[data-rooms-screen]');
  const leaveBtn = root.querySelector('[data-rooms-leave]');

  const peers = new Map(); // key -> { pc, video, card, name, senders: {audio?, video?} }
  // Each kind tracks: { track, stream } when on; undefined when off.
  const local = { audio: null, video: null };
  let screenStream = null;
  let signalSrc = null;
  let heartbeatTimer = null;

  const tileForSelf = makeTile(myKey, myName + ' (you)', true);
  videoGrid.appendChild(tileForSelf.card);
  syncToggleLabel(micBtn, 'mic', false);
  syncToggleLabel(camBtn, 'cam', false);

  // ----- toggle label ------------------------------------------------------

  function syncToggleLabel(btn, kind, on) {
    if (!btn) return;
    const icons = { mic: '🎙', cam: '🎥' };
    btn.classList.toggle('on', on);
    btn.classList.toggle('off', !on);
    btn.textContent = `${icons[kind]} ${on ? 'on' : 'off'}`;
    btn.title = (kind === 'mic' ? 'Microphone' : 'Camera') + (on ? ' (on)' : ' (off)');
  }

  // ----- self preview composition ------------------------------------------

  function refreshSelfPreview() {
    if (screenStream) { tileForSelf.video.srcObject = screenStream; return; }
    if (!local.audio && !local.video) { tileForSelf.video.srcObject = null; return; }
    const ms = new MediaStream();
    if (local.video) ms.addTrack(local.video.track);
    // Don't render local audio (would cause echo even though muted=true).
    tileForSelf.video.srcObject = ms;
  }

  // ----- enable / disable media -------------------------------------------
  //
  // On enable: getUserMedia for just this kind, attach to every PC,
  // trigger renegotiation by the impolite side.
  // On disable: stop the track, remove from each PC, renegotiate.

  async function enableMedia(kind) {
    if (local[kind]) return true;
    let stream;
    try {
      const constraints = kind === 'audio' ? { audio: true } : { video: { width: 640, height: 480 } };
      stream = await navigator.mediaDevices.getUserMedia(constraints);
    } catch {
      return false;
    }
    const track = stream.getTracks().find(t => t.kind === kind);
    if (!track) { stream.getTracks().forEach(t => t.stop()); return false; }
    local[kind] = { track, stream };

    // Attach to every existing peer connection.
    for (const entry of peers.values()) {
      const sender = entry.pc.addTrack(track, stream);
      entry.senders[kind] = sender;
    }
    refreshSelfPreview();
    return true;
  }

  async function disableMedia(kind) {
    const cur = local[kind];
    if (!cur) return;
    cur.track.stop();
    cur.stream.getTracks().forEach(t => t.stop());
    local[kind] = null;
    for (const entry of peers.values()) {
      const sender = entry.senders[kind];
      if (sender) {
        try { entry.pc.removeTrack(sender); } catch {}
        entry.senders[kind] = null;
      }
    }
    refreshSelfPreview();
  }

  micBtn?.addEventListener('click', async () => {
    if (local.audio) { await disableMedia('audio'); syncToggleLabel(micBtn, 'mic', false); }
    else { const ok = await enableMedia('audio'); syncToggleLabel(micBtn, 'mic', ok); }
  });
  camBtn?.addEventListener('click', async () => {
    if (local.video) { await disableMedia('video'); syncToggleLabel(camBtn, 'cam', false); }
    else { const ok = await enableMedia('video'); syncToggleLabel(camBtn, 'cam', ok); }
  });

  // ----- screenshare -------------------------------------------------------

  screenBtn?.addEventListener('click', async () => {
    if (screenStream) { stopScreenshare(); return; }
    try {
      screenStream = await navigator.mediaDevices.getDisplayMedia({ video: true, audio: false });
    } catch { return; }
    const screenTrack = screenStream.getVideoTracks()[0];
    screenTrack.addEventListener('ended', stopScreenshare);
    for (const entry of peers.values()) {
      const sender = entry.senders.video;
      if (sender) {
        await sender.replaceTrack(screenTrack);
      } else {
        const s = entry.pc.addTrack(screenTrack, screenStream);
        entry.senders.video = s;
      }
    }
    refreshSelfPreview();
    screenBtn.classList.add('on');
  });

  async function stopScreenshare() {
    if (!screenStream) return;
    screenStream.getTracks().forEach(t => t.stop());
    screenStream = null;
    const camTrack = local.video?.track || null;
    for (const entry of peers.values()) {
      const sender = entry.senders.video;
      if (!sender) continue;
      if (camTrack) {
        await sender.replaceTrack(camTrack);
      } else {
        try { entry.pc.removeTrack(sender); } catch {}
        entry.senders.video = null;
      }
    }
    refreshSelfPreview();
    screenBtn.classList.remove('on');
  }

  // ----- leave -------------------------------------------------------------

  leaveBtn?.addEventListener('click', () => {
    teardown();
    fetch(`/rooms/${encodeURIComponent(roomID)}/leave`, { method: 'POST', keepalive: true })
      .finally(() => { window.location.href = '/rooms'; });
  });

  window.addEventListener('beforeunload', () => {
    navigator.sendBeacon?.(`/rooms/${encodeURIComponent(roomID)}/leave`);
  });
  window.addEventListener('pagehide', () => {
    navigator.sendBeacon?.(`/rooms/${encodeURIComponent(roomID)}/leave`);
  });

  function teardown() {
    if (heartbeatTimer) clearInterval(heartbeatTimer);
    for (const k of [...peers.keys()]) closePeer(k);
    disableMedia('audio');
    disableMedia('video');
    screenStream?.getTracks().forEach(t => t.stop());
    if (signalSrc) signalSrc.close();
  }

  // ----- heartbeat ---------------------------------------------------------

  function startHeartbeat() {
    const ping = () => fetch(`/rooms/${encodeURIComponent(roomID)}/ping`, {
      method: 'POST', keepalive: true,
    }).catch(() => {});
    ping();
    heartbeatTimer = setInterval(ping, 10000);
  }

  // ----- peer plumbing -----------------------------------------------------

  const polite = (otherKey) => otherKey > myKey;

  function ensurePeer(key, name) {
    let entry = peers.get(key);
    if (entry) return entry;
    const tile = makeTile(key, name, false);
    videoGrid.appendChild(tile.card);
    const pc = new RTCPeerConnection({ iceServers });
    entry = {
      pc, video: tile.video, card: tile.card, name,
      senders: { audio: null, video: null },
      // Perfect-negotiation bookkeeping: either side can initiate after
      // adding a track later (camera toggle, screenshare). On glare, the
      // polite side rolls its local offer back and accepts the remote one.
      makingOffer: false,
      ignoreOffer: false,
      iAmPolite: polite(key),
    };
    peers.set(key, entry);

    if (local.audio) entry.senders.audio = pc.addTrack(local.audio.track, local.audio.stream);
    if (local.video) entry.senders.video = pc.addTrack(local.video.track, local.video.stream);

    pc.ontrack = (ev) => {
      let remote = tile.video.srcObject;
      if (!(remote instanceof MediaStream)) {
        remote = new MediaStream();
        tile.video.srcObject = remote;
      }
      remote.addTrack(ev.track);
    };
    pc.onicecandidate = (ev) => {
      if (!ev.candidate) return;
      sendSignal(key, 'ice', JSON.stringify(ev.candidate));
    };
    pc.onnegotiationneeded = async () => {
      try {
        entry.makingOffer = true;
        await pc.setLocalDescription();
        sendSignal(key, 'offer', JSON.stringify(pc.localDescription));
      } catch (e) {
        console.warn('negotiation failed', e);
      } finally {
        entry.makingOffer = false;
      }
    };
    return entry;
  }

  function closePeer(key) {
    const entry = peers.get(key);
    if (!entry) return;
    try { entry.pc.close(); } catch {}
    entry.card?.remove();
    peers.delete(key);
  }

  // ----- signaling ---------------------------------------------------------

  function openSignaling() {
    signalSrc = new EventSource(`/rooms/${encodeURIComponent(roomID)}/signal/stream`);
    signalSrc.addEventListener('sig', async (e) => {
      let msg;
      try { msg = JSON.parse(e.data); } catch { return; }
      if (msg.kind === 'hello') return;
      if (msg.kind === 'bye') { closePeer(msg.from); return; }
      const entry = ensurePeer(msg.from, msg.from);
      const pc = entry.pc;
      try {
        if (msg.kind === 'offer') {
          const desc = JSON.parse(msg.payload);
          const offerCollision =
            entry.makingOffer || pc.signalingState !== 'stable';
          entry.ignoreOffer = !entry.iAmPolite && offerCollision;
          if (entry.ignoreOffer) return;
          if (offerCollision) {
            // Polite side: roll back its pending offer and take theirs.
            await Promise.all([
              pc.setLocalDescription({ type: 'rollback' }).catch(() => {}),
              pc.setRemoteDescription(desc),
            ]);
          } else {
            await pc.setRemoteDescription(desc);
          }
          await pc.setLocalDescription();
          sendSignal(msg.from, 'answer', JSON.stringify(pc.localDescription));
        } else if (msg.kind === 'answer') {
          await pc.setRemoteDescription(JSON.parse(msg.payload));
        } else if (msg.kind === 'ice') {
          try { await pc.addIceCandidate(JSON.parse(msg.payload)); }
          catch (e) { if (!entry.ignoreOffer) throw e; }
        }
      } catch (e) { console.warn('signal handle failed', msg.kind, e); }
    });
  }

  function sendSignal(to, kind, payload) {
    fetch(`/rooms/${encodeURIComponent(roomID)}/signal/send`, {
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
      if (!peers.has(key)) ensurePeer(key, name);
    }
    for (const key of [...peers.keys()]) {
      if (!wanted.has(key)) closePeer(key);
    }
  }

  function makeTile(key, name, isLocal) {
    const card = document.createElement('div');
    card.className = 'rooms-video-tile' + (isLocal ? ' rooms-video-tile-self' : '');
    card.dataset.peerKey = key;
    const video = document.createElement('video');
    video.autoplay = true;
    video.playsInline = true;
    if (isLocal) video.muted = true;
    const label = document.createElement('div');
    label.className = 'rooms-video-label';
    label.textContent = name;
    card.appendChild(video);
    card.appendChild(label);
    return { card, video };
  }

  // ----- boot --------------------------------------------------------------

  openSignaling();
  startHeartbeat();
})();
