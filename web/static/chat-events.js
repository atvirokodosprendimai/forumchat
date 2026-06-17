// chat-events.js — cross-page chat notification ping.
//
// Loaded from layout.templ on every authed community page. The server
// streams `window.fcChatPing && window.fcChatPing()` ExecuteScript
// snippets via SSE; this file defines that global function and the
// politeness rules around it.
//
// On the /chat page itself, chat-notify.js handles the in-tab logic
// (which knows about own vs other, focus state, scroll position).
// fcChatPing is a no-op there to avoid double-pings.
(() => {
  // Hoisted state — see chat-notify.js TDZ note. Declared before any
  // function definition that closes over them.
  let audioCtx     = null;
  let lastPingAt   = 0;

  // Skip on the chat page — that page owns its own observer + ping
  // logic and would double-pong otherwise.
  const onChatPage = () => location.pathname.endsWith('/chat');

  window.fcChatPing = function fcChatPing() {
    if (onChatPage()) return;

    // Coalesce bursts — if 10 messages land in 200ms (e.g. forum-bridge
    // creating a thread_announce on top of a chat send), one ping is
    // enough.
    const now = Date.now();
    if (now - lastPingAt < 800) return;
    lastPingAt = now;

    pingSound();
    maybeShowToast();
  };

  function pingSound() {
    try {
      if (!audioCtx) {
        const AC = window.AudioContext || window.webkitAudioContext;
        if (!AC) return;
        audioCtx = new AC();
      }
      // Some browsers start the context in 'suspended' state until a
      // user gesture. resume() is fire-and-forget — if it fails, the
      // next user-gesture-driven call will succeed.
      if (audioCtx.state === 'suspended') audioCtx.resume().catch(() => {});
      const t0 = audioCtx.currentTime;
      playTone(audioCtx, 880,  t0,        0.07);
      playTone(audioCtx, 1320, t0 + 0.08, 0.10);
    } catch (_) {}
  }

  function playTone(ctx, freq, when, dur) {
    const osc  = ctx.createOscillator();
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
      const n = new Notification('New chat message — forumchat', {
        body: 'Click to open the chat.',
        icon: '/static/icon-192.png',
        tag: 'forumchat-chat',
        renotify: true,
      });
      n.onclick = () => {
        // Best-effort: focus the window, then navigate to chat.
        try { window.focus(); } catch (_) {}
        const slug = currentSlug();
        if (slug) location.assign('/c/' + encodeURIComponent(slug) + '/chat');
        n.close();
      };
    } catch (_) {}
  }

  // Read the community slug from the same data-attr chat.templ uses,
  // falling back to a regex over the path. Layout doesn't put the slug
  // on #messages (only the chat page does), so the path fallback is
  // the live source on every other page.
  function currentSlug() {
    const m = document.querySelector('#messages');
    if (m && m.dataset && m.dataset.communitySlug) return m.dataset.communitySlug;
    const match = location.pathname.match(/^\/c\/([^\/]+)/);
    return match ? match[1] : '';
  }
})();
