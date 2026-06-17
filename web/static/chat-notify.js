// chat-notify.js — client-side polish for the chat page.
//
// Three jobs, each cheap and independent:
//
//   1. Ping + (optional) in-tab Notification when a new message from
//      someone else arrives AND the window/tab is not the user's focus.
//      "Not the focus" = document.hidden OR !document.hasFocus() — covers
//      both another tab on top AND the PWA window being in the background.
//
//   2. Mark-read POST on focus / visibility change / new own-message,
//      debounced so a focus-pulse storm doesn't hammer the server.
//
//   3. Lazy permission ask — we only call Notification.requestPermission
//      after the first user-initiated send, so the prompt feels earned.
(() => {
  const messages = document.querySelector('#messages');
  if (!messages) return;

  const currentUserID = messages.dataset.currentUserId || '';
  const slug          = messages.dataset.communitySlug || '';
  if (!currentUserID || !slug) return;

  let lastSeenID = newestMsgId();

  // ------- 1. observe new bubbles -------
  const obs = new MutationObserver(() => {
    const newest = newestMsgId();
    if (!newest || newest === lastSeenID) return;
    const article = messages.querySelector(`article[data-id="${cssEscape(newest)}"]`);
    if (!article) { lastSeenID = newest; return; }

    // Walk back from the newest article to figure out the author —
    // grouped runs put author info on the FIRST sibling in the run
    // (.bubble-group header). For the simple "is it mine" check we
    // check the parent group's class.
    const group = article.closest('.bubble-group');
    const isOwn = group && group.classList.contains('own');

    if (!isOwn && (document.hidden || !document.hasFocus())) {
      pingSound();
      maybeShowToast();
    }
    if (document.hasFocus() && !document.hidden) {
      scheduleMarkRead();
    }
    lastSeenID = newest;
  });
  obs.observe(messages, { childList: true, subtree: true });

  // ------- 2. focus / visibility → mark-read -------
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) scheduleMarkRead();
  });
  window.addEventListener('focus', scheduleMarkRead);
  // First-mount sync — Datastar's data-init scrolls us to the bottom on
  // load, treat that as "seen up to here" immediately.
  scheduleMarkRead();

  // ------- 3. lazy permission ask -------
  // Hook the composer Send to ask once, after the first user-initiated send.
  const composerSend = document.querySelector('.composer button');
  if (composerSend) {
    composerSend.addEventListener('click', requestPermissionOnce, { once: true });
  }
  document.addEventListener('keydown', (evt) => {
    if (evt.key !== 'Enter' || evt.shiftKey) return;
    const ta = evt.target;
    if (ta && ta.classList && ta.classList.contains('composer-text')) {
      requestPermissionOnce();
    }
  }, true);

  // ---------- helpers ----------
  function newestMsgId() {
    // .bubble-group children are stacked oldest→newest inside #messages.
    // The newest article is the LAST .bubble article (not .system).
    const arts = messages.querySelectorAll('article.bubble[data-id]');
    if (arts.length === 0) return '';
    return arts[arts.length - 1].getAttribute('data-id') || '';
  }

  let markTimer = 0;
  function scheduleMarkRead() {
    if (markTimer) clearTimeout(markTimer);
    markTimer = setTimeout(postMarkRead, 1000);
  }

  function postMarkRead() {
    markTimer = 0;
    const id = newestMsgId();
    if (!id) return;
    // Datastar reads signals from the request body as JSON. We don't
    // have a Datastar action helper here, so post directly — server's
    // datastar.ReadSignals accepts plain JSON bodies.
    fetch(`/c/${encodeURIComponent(slug)}/chat/read`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ last_id: id }),
      credentials: 'same-origin',
      keepalive: true,
    }).catch(() => {});
  }

  // Beep via WebAudio so we don't ship a mp3 asset. Two short tones
  // ~80ms apart, soft sine, peaks at ~0.18 so we don't blast headphones.
  let audioCtx = null;
  function pingSound() {
    try {
      if (!audioCtx) {
        const AC = window.AudioContext || window.webkitAudioContext;
        if (!AC) return;
        audioCtx = new AC();
      }
      const now = audioCtx.currentTime;
      playTone(audioCtx, 880, now,        0.07);
      playTone(audioCtx, 1320, now + 0.08, 0.10);
    } catch (_) {}
  }
  function playTone(ctx, freq, when, dur) {
    const osc = ctx.createOscillator();
    const gain = ctx.createGain();
    osc.type = 'sine';
    osc.frequency.value = freq;
    gain.gain.setValueAtTime(0.0001, when);
    gain.gain.exponentialRampToValueAtTime(0.18, when + 0.01);
    gain.gain.exponentialRampToValueAtTime(0.0001, when + dur);
    osc.connect(gain).connect(ctx.destination);
    osc.start(when);
    osc.stop(when + dur + 0.02);
  }

  function maybeShowToast() {
    if (!('Notification' in window)) return;
    if (Notification.permission !== 'granted') return;
    try {
      const n = new Notification('New message — forumchat', {
        body: 'Open the chat to read.',
        icon: '/static/icon-192.png',
        tag: 'forumchat-chat',
        renotify: true,
      });
      n.onclick = () => { window.focus(); n.close(); };
    } catch (_) {}
  }

  function requestPermissionOnce() {
    if (!('Notification' in window)) return;
    if (Notification.permission !== 'default') return;
    try { Notification.requestPermission().catch(() => {}); } catch (_) {}
  }

  // CSS.escape polyfill — Safari 16 has it but older browsers don't.
  function cssEscape(s) {
    if (window.CSS && CSS.escape) return CSS.escape(s);
    return String(s).replace(/[^a-zA-Z0-9_-]/g, (c) => '\\' + c);
  }
})();
