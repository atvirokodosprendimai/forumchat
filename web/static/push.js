// push.js — client side of forumchat push notifications.
//
// Runs on the /c/{slug}/notifications page. Reads the VAPID public key
// + community slug from data-* attributes on the .notif-card host, and
// wires the Enable/Disable buttons to PushManager.subscribe and the
// POST /push/subscribe and /push/unsubscribe endpoints.
//
// Settings (the per-event checkboxes) are saved via the page's own
// "Save" button which @posts /c/{slug}/notifications/save — this script
// only handles the subscription lifecycle.
(() => {
  const root = document.querySelector('.notif-card');
  if (!root) return;

  const publicKey = root.dataset.vapidPubkey || '';
  const slug = root.dataset.communityId || '';
  const errorBox = document.getElementById('notif-error');
  const statusBox = document.getElementById('notif-status');

  if (!('serviceWorker' in navigator) || !('PushManager' in window)) {
    showError('Your browser does not support push notifications.');
    disableButtons();
    return;
  }
  if (!publicKey) {
    showError('Push is not configured on this server.');
    disableButtons();
    return;
  }

  function showError(msg) {
    if (errorBox) errorBox.textContent = msg;
  }
  function showStatus(msg) {
    if (statusBox) statusBox.textContent = msg;
  }
  function disableButtons() {
    document.querySelectorAll('[data-notif-enable], [data-notif-disable]').forEach(b => {
      b.setAttribute('disabled', 'disabled');
    });
  }

  // urlBase64ToUint8Array — the canonical helper. Push subscription
  // applicationServerKey wants raw bytes, not the base64 string.
  function urlBase64ToUint8Array(base64String) {
    const padding = '='.repeat((4 - base64String.length % 4) % 4);
    const base64 = (base64String + padding).replace(/-/g, '+').replace(/_/g, '/');
    const raw = atob(base64);
    const out = new Uint8Array(raw.length);
    for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
    return out;
  }

  function bufToBase64Url(buf) {
    const bytes = new Uint8Array(buf);
    let s = '';
    for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
    return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }

  async function registerSW() {
    // Must register from a path whose URL allows scope '/'. The server
    // exposes a dedicated /sw.js handler that adds Service-Worker-Allowed: /
    // and ServeFiles the same file from web/static/sw.js.
    const reg = await navigator.serviceWorker.register('/sw.js', { scope: '/' });
    // Wait for it to be active so subscribe() doesn't race.
    if (reg.installing) {
      await new Promise(res => {
        const t = reg.installing;
        t.addEventListener('statechange', () => {
          if (t.state === 'activated') res();
        });
      });
    }
    return reg;
  }

  async function readCurrentSettings() {
    // Pull whatever the page rendered into data-signals; if the
    // checkboxes were toggled before Enable was pressed we still
    // ship the current DOM state.
    const get = (name) => document.querySelector(`input[data-bind="${name}"]`)?.checked ?? true;
    return {
      mention:     get('notif_mention'),
      report:      get('notif_report'),
      project_new: get('notif_project_new'),
      issue_new:   get('notif_issue_new'),
      comment_new: get('notif_comment_new'),
      thread_new:  get('notif_thread_new'),
    };
  }

  function readDigestMinutes() {
    const sel = document.querySelector('select[data-bind="notif_digest_minutes"]');
    if (!sel) return 0;
    const n = parseInt(sel.value, 10);
    return Number.isFinite(n) && n >= 0 ? n : 0;
  }

  async function enable() {
    try {
      showError('');
      showStatus('Asking the browser for permission…');
      const perm = await Notification.requestPermission();
      if (perm !== 'granted') {
        showError('Permission denied. Re-enable from your browser settings.');
        showStatus('');
        return;
      }
      const reg = await registerSW();
      showStatus('Subscribing…');
      const sub = await reg.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: urlBase64ToUint8Array(publicKey),
      });
      const body = {
        community_id: slug,                              // Server resolves slug → ID server-side via context.
        endpoint: sub.endpoint,
        p256dh: bufToBase64Url(sub.getKey('p256dh')),
        auth_key: bufToBase64Url(sub.getKey('auth')),
        user_agent: navigator.userAgent || '',
        settings: await readCurrentSettings(),
        digest_minutes: readDigestMinutes(),
      };
      const res = await fetch('/push/subscribe', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!res.ok) {
        showError('Server rejected the subscription: ' + res.status);
        return;
      }
      showStatus('Push enabled on this device. Reload to update the button.');
    } catch (e) {
      console.error('[push] enable failed', e);
      showError('Could not enable: ' + (e?.message || e));
    }
  }

  async function disable() {
    try {
      showError('');
      showStatus('Disabling…');
      const reg = await navigator.serviceWorker.getRegistration('/')
        || await navigator.serviceWorker.getRegistration('/sw.js');
      const sub = reg ? await reg.pushManager.getSubscription() : null;
      if (sub) {
        try { await sub.unsubscribe(); } catch (_) {}
        await fetch('/push/unsubscribe', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ endpoint: sub.endpoint }),
        }).catch(() => {});
      }
      showStatus('Push disabled. Reload to update the button.');
    } catch (e) {
      console.error('[push] disable failed', e);
      showError('Could not disable: ' + (e?.message || e));
    }
  }

  document.querySelectorAll('[data-notif-enable]').forEach(b => b.addEventListener('click', enable));
  document.querySelectorAll('[data-notif-disable]').forEach(b => b.addEventListener('click', disable));

  // IMPORTANT: the server takes a community_id but the page only has
  // the slug. We pass the slug here; the server's subscribe handler
  // resolves it to the actual community id via the community-context
  // middleware that runs on this route.
})();
