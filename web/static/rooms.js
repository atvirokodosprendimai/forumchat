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
  const communitySlug = root.dataset.communitySlug;
  const roomBase = `/c/${encodeURIComponent(communitySlug)}/rooms/${encodeURIComponent(roomID)}`;
  const myKey = root.dataset.myKey;
  const myName = root.dataset.myName || 'me';
  let iceServers = [];
  try { iceServers = JSON.parse(root.dataset.iceServers || '[]'); } catch { /* noop */ }
  if (iceServers.length === 0) {
    iceServers = [{ urls: 'stun:stun.l.google.com:19302' }];
  }

  const videoGrid = root.querySelector('[data-video-grid]');
  const stageVideo = root.querySelector('[data-rooms-stage-video]');
  const stageEmpty = root.querySelector('[data-rooms-stage-empty]');
  const stageLabel = root.querySelector('[data-rooms-stage-label]');
  const micBtn = root.querySelector('[data-rooms-mic]');
  const camBtn = root.querySelector('[data-rooms-cam]');
  const screenBtn = root.querySelector('[data-rooms-screen]');
  const leaveBtn = root.querySelector('[data-rooms-leave]');
  let focusedKey = null;

  // ----- center stage focus ------------------------------------------------

  function focusTile(key) {
    focusedKey = key;
    // Highlight the focused thumbnail in the strip.
    videoGrid.querySelectorAll('.rooms-video-tile').forEach(el => {
      el.classList.toggle('focused', el.dataset.peerKey === key);
    });
    let stream = null;
    let label = '';
    if (key === myKey) {
      stream = tileForSelf.video.srcObject;
      label = myName + ' (you)';
    } else {
      const entry = peers.get(key);
      stream = entry?.video?.srcObject || null;
      label = entry?.name || 'peer';
    }
    stageVideo.srcObject = stream;
    stageVideo.classList.toggle('empty', !stream);
    stageEmpty?.classList.toggle('hidden', !!stream);
    if (stageLabel) stageLabel.textContent = stream ? label : '';
    if (stream) stageVideo.play?.().catch(() => {});
  }

  function clearStageIfFocused(key) {
    if (focusedKey === key) {
      focusedKey = null;
      stageVideo.srcObject = null;
      stageVideo.classList.add('empty');
      stageEmpty?.classList.remove('hidden');
      if (stageLabel) stageLabel.textContent = '';
    }
  }

  function refreshStageIfFocused(key) {
    if (focusedKey === key) focusTile(key);
  }

  const peers = new Map(); // key -> { pc, video, card, name, senders: {audio?, video?} }
  // Each kind tracks: { track, stream } when on; undefined when off.
  const local = { audio: null, video: null };
  let screenStream = null;
  let heartbeatTimer = null;
  // leaving gates outbound POSTs after the user clicks Leave / page unloads.
  // The server's EnsureMember would otherwise re-admit them on any in-flight
  // ICE candidate / chat send that races the /leave call — leaving them
  // stuck as a ghost admin even after a refresh.
  let leaving = false;

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
    if (screenStream) {
      tileForSelf.video.srcObject = screenStream;
    } else if (!local.audio && !local.video) {
      tileForSelf.video.srcObject = null;
    } else {
      const ms = new MediaStream();
      if (local.video) ms.addTrack(local.video.track);
      // Don't render local audio (would cause echo even though muted=true).
      tileForSelf.video.srcObject = ms;
    }
    refreshStageIfFocused(myKey);
  }

  // ----- enable / disable media -------------------------------------------
  //
  // On enable: getUserMedia for just this kind, attach to every PC,
  // trigger renegotiation by the impolite side.
  // On disable: stop the track, remove from each PC, renegotiate.

  async function enableMedia(kind) {
    if (local[kind]) return true;
    if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
      // Almost always means the page is on http:// with a non-localhost
      // host (insecure context). Surface it instead of failing silently —
      // the toggle button will just stay "off" without this, leaving the
      // user with no idea why their camera/mic won't engage.
      flashMediaError(kind, 'Camera/microphone require HTTPS (or localhost).');
      return false;
    }
    let stream;
    try {
      const constraints = kind === 'audio' ? { audio: true } : { video: { width: 640, height: 480 } };
      stream = await navigator.mediaDevices.getUserMedia(constraints);
    } catch (e) {
      console.warn('[rooms] getUserMedia denied', kind, e);
      flashMediaError(kind, e?.name ? `${e.name}: ${e.message || ''}` : String(e));
      return false;
    }
    const track = stream.getTracks().find(t => t.kind === kind);
    if (!track) { stream.getTracks().forEach(t => t.stop()); return false; }
    local[kind] = { track, stream };
    console.log('[rooms] media enabled', kind, 'peers:', peers.size);

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
    // Sharing screen is louder than a face — promote it to the stage
    // automatically so everyone notices.
    focusTile(myKey);
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
  //
  // Background tabs get setInterval throttled to ~1Hz at best, and
  // anything that breaks one HTTPS round-trip silently swallows a ping
  // (the catch is a noop). Three missed pings used to evict the member,
  // which broke every active PC for that key — now staleAfter is 60s on
  // the server, EnsureMember self-heals on the next POST, and we also
  // re-ping on tab-focus / visibility / connectivity recovery to be
  // belt-and-braces about it.

  function startHeartbeat() {
    const ping = () => {
      if (leaving) return;
      fetch(`${roomBase}/ping`, {
        method: 'POST', keepalive: true,
      }).catch(() => {});
    };
    ping();
    heartbeatTimer = setInterval(ping, 10000);
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'visible') ping();
    });
    window.addEventListener('focus', ping);
    window.addEventListener('online', ping);
  }

  // ----- peer plumbing -----------------------------------------------------

  const polite = (otherKey) => otherKey > myKey;

  function ensurePeer(key, name) {
    let entry = peers.get(key);
    if (entry) return entry;
    const tile = makeTile(key, name, false);
    videoGrid.appendChild(tile.card);
    const pc = new RTCPeerConnection({ iceServers });
    // Dedicated <audio> sink per peer. Tiny video tiles in the strip can
    // have their audio suppressed by browsers (they look hidden / off-
    // screen). A separate, always-attached <audio autoplay> sidesteps the
    // policy and gives us reliable voice even when the tile is invisible.
    const audio = document.createElement('audio');
    audio.autoplay = true;
    audio.style.display = 'none';
    document.body.appendChild(audio);

    entry = {
      pc, video: tile.video, audio, card: tile.card, name,
      senders: { audio: null, video: null },
      // Perfect-negotiation bookkeeping per MDN. Either side can initiate
      // after adding a track later (camera toggle, screenshare). On glare
      // the polite side rolls back its pending offer and accepts the
      // remote one; the impolite side discards the colliding incoming.
      makingOffer: false,
      ignoreOffer: false,
      isSettingRemoteAnswerPending: false,
      iAmPolite: polite(key),
    };
    peers.set(key, entry);

    if (local.audio) entry.senders.audio = pc.addTrack(local.audio.track, local.audio.stream);
    if (local.video) entry.senders.video = pc.addTrack(local.video.track, local.video.stream);

    pc.ontrack = (ev) => {
      console.log('[rooms] ontrack', { key, kind: ev.track.kind, streams: ev.streams.length });
      // Audio tracks go to the dedicated hidden <audio> sink (better
      // browser autoplay tolerance). Video tracks go to the tile <video>
      // (which is muted=false on the video element, but won't receive
      // audio tracks under this routing).
      if (ev.track.kind === 'audio') {
        if (!(entry.audio.srcObject instanceof MediaStream)) {
          entry.audio.srcObject = new MediaStream();
        }
        entry.audio.srcObject.addTrack(ev.track);
        const tryPlay = () => entry.audio.play?.()
          .then(() => console.log('[rooms] audio play OK', key))
          .catch((err) => console.warn('[rooms] audio play blocked', key, err?.name));
        tryPlay();
        // Race a retry — some browsers reject .play() before the first
        // packet arrives even when autoplay is allowed.
        setTimeout(tryPlay, 250);
        armGlobalAudioRecovery();
        const dropAudio = () => {
          try { entry.audio.srcObject?.removeTrack(ev.track); } catch {}
        };
        ev.track.addEventListener('mute', dropAudio);
        ev.track.addEventListener('ended', dropAudio);
        return;
      }
      let remote = tile.video.srcObject;
      if (!(remote instanceof MediaStream)) {
        remote = new MediaStream();
        tile.video.srcObject = remote;
      }
      remote.addTrack(ev.track);
      tile.video.play?.().catch(() => {});
      refreshStageIfFocused(key);
      // Auto-promote any incoming remote VIDEO to the center stage when
      // the stage is empty or only showing ourselves. That covers the
      // common case where a remote peer starts screenshare — without this
      // the screen lands in the small thumbnail and the big stage stays
      // black until someone clicks. We override "self focused" too because
      // a remote video is almost always more interesting than your own
      // preview, but we never override an explicit remote pick.
      if (ev.track.kind === 'video' && (!focusedKey || focusedKey === myKey)) {
        focusTile(key);
      }
      const drop = () => {
        try { remote.removeTrack(ev.track); } catch {}
        const ms = tile.video.srcObject;
        tile.video.srcObject = null;
        if (ms instanceof MediaStream && ms.getTracks().length > 0) {
          tile.video.srcObject = ms;
        }
        refreshStageIfFocused(key);
        // If the focused peer's only video track just vanished, release
        // the stage so the next incoming video can auto-promote itself.
        if (focusedKey === key && ev.track.kind === 'video') {
          const stillVideo = (ms instanceof MediaStream) && ms.getVideoTracks().length > 0;
          if (!stillVideo) clearStageIfFocused(key);
        }
      };
      ev.track.addEventListener('mute', drop);
      ev.track.addEventListener('ended', drop);
    };
    pc.onicecandidate = (ev) => {
      if (!ev.candidate) return;
      sendSignal(key, 'ice', JSON.stringify(ev.candidate));
    };
    // Diagnostic logging — silent failures were the #1 cause of "guest
    // exists but no video". The browser console now shows the actual ICE
    // verdict: relay-required without TURN, NAT type mismatch, etc.
    pc.oniceconnectionstatechange = () => {
      console.log('[rooms] ice state', key, pc.iceConnectionState);
      if (pc.iceConnectionState === 'failed') {
        try { pc.restartIce?.(); } catch {}
      }
    };
    pc.onconnectionstatechange = () => {
      console.log('[rooms] pc state', key, pc.connectionState);
    };
    pc.onicegatheringstatechange = () => {
      console.log('[rooms] gather state', key, pc.iceGatheringState);
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
    return entry;
  }

  function closePeer(key) {
    const entry = peers.get(key);
    if (!entry) return;
    try { entry.pc.close(); } catch {}
    entry.card?.remove();
    entry.audio?.remove();
    peers.delete(key);
    clearStageIfFocused(key);
  }

  // ----- signaling ---------------------------------------------------------
  //
  // Signaling envelopes ride the same SSE stream as datastar room events
  // (handler.go -> pushSignal -> sse.ExecuteScript -> window.__roomsSignal).
  // No separate EventSource: under HTTP/1.1, the per-origin connection cap
  // was being eaten by (messages SSE) + (room SSE) + (signal SSE) + the
  // ICE-candidate POST bursts at cam-on time, which silently killed live
  // chat / presence updates.

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
      if (!peers.has(key)) ensurePeer(key, name);
    }
    for (const key of [...peers.keys()]) {
      if (!wanted.has(key)) closePeer(key);
    }
  }

  // Browsers block <audio>.play() until the user has interacted with the
  // page. We arm a single click listener that replays every peer audio
  // element on the first click — recovers reliably whatever the user
  // touches first.
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

  // flashMediaError briefly surfaces a gUM / device error next to the
  // toolbar so users see why a camera/mic toggle did nothing.
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

  function makeTile(key, name, isLocal) {
    const card = document.createElement('div');
    card.className = 'rooms-video-tile' + (isLocal ? ' rooms-video-tile-self' : '');
    card.dataset.peerKey = key;
    card.title = `Click to focus ${name} on the center stage`;
    const video = document.createElement('video');
    video.autoplay = true;
    video.playsInline = true;
    if (isLocal) video.muted = true;
    const label = document.createElement('div');
    label.className = 'rooms-video-label';
    label.textContent = name;
    card.appendChild(video);
    card.appendChild(label);
    card.addEventListener('click', () => focusTile(key));
    return { card, video };
  }

  // ----- boot --------------------------------------------------------------
  //
  // No openSignaling() call — signaling is pushed by the datastar room SSE
  // stream that the templ opens via data-init. The handler invokes
  // window.__roomsSignal for each envelope.
  startHeartbeat();
})();
