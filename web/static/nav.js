// Mobile left-drawer toggle. Sidebar visibility on >=900px is pure
// CSS — this script only handles the mobile slide-in: tap hamburger
// to open, tap overlay or any sidebar link to close. Toggles a
// `nav-open` class on <body> that the CSS keys off.
// Also marks the sidebar link whose href best matches the current path
// with aria-current="page" so the CSS active state lights up.
(() => {
  function markActive() {
    const here = location.pathname.replace(/\/+$/, '') || '/';
    const links = document.querySelectorAll('.sidebar nav a[href]');
    let best = null, bestLen = -1;
    links.forEach(a => {
      a.removeAttribute('aria-current');
      let href = a.getAttribute('href') || '';
      try { href = new URL(href, location.href).pathname.replace(/\/+$/, '') || '/'; } catch (_) {}
      if (here === href || (href !== '/' && here.startsWith(href + '/'))) {
        if (href.length > bestLen) { best = a; bestLen = href.length; }
      }
    });
    if (best) best.setAttribute('aria-current', 'page');
  }
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', markActive);
  } else {
    markActive();
  }
  function setOpen(open) {
    document.body.classList.toggle('nav-open', open);
  }
  function setPresenceOpen(open) {
    document.body.classList.toggle('presence-open', open);
  }
  document.addEventListener('click', (ev) => {
    const t = ev.target;
    if (!(t instanceof Element)) return;
    if (t.closest('[data-nav-toggle]')) {
      ev.preventDefault();
      setOpen(!document.body.classList.contains('nav-open'));
      return;
    }
    if (t.closest('[data-nav-overlay]')) {
      setOpen(false);
      return;
    }
    if (t.closest('.sidebar a, .sidebar button')) {
      setOpen(false);
      return;
    }
    if (t.closest('[data-presence-toggle]')) {
      ev.preventDefault();
      setPresenceOpen(!document.body.classList.contains('presence-open'));
      return;
    }
    if (t.closest('[data-presence-overlay]')) {
      setPresenceOpen(false);
      return;
    }
  });
  // Close on Escape — handy for keyboard users + accidental opens.
  document.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape') {
      setOpen(false);
      setPresenceOpen(false);
    }
  });

  // Strip data-cloak globally + on any future patches.
  //
  // `[data-cloak] { display: none !important }` is the FOUC guard for
  // initial page render. The body's data-init runs ONCE at mount and
  // strips it — but server SSE patches (PatchElementTempl etc.) ship
  // new HTML that re-injects data-cloak. Without a re-stripper, those
  // patched elements stay invisible forever.
  //
  // We watch the whole document for any node insertions AND any
  // attribute changes that set data-cloak, and strip it everywhere.
  function stripCloak(root) {
    if (!root) return;
    if (root.nodeType === 1 && root.hasAttribute && root.hasAttribute('data-cloak')) {
      root.removeAttribute('data-cloak');
    }
    if (root.querySelectorAll) {
      root.querySelectorAll('[data-cloak]').forEach(e => e.removeAttribute('data-cloak'));
    }
  }
  function bootCloakStripper() {
    stripCloak(document.body);
    const mo = new MutationObserver((records) => {
      for (const r of records) {
        if (r.type === 'attributes' && r.attributeName === 'data-cloak') {
          if (r.target.hasAttribute('data-cloak')) {
            r.target.removeAttribute('data-cloak');
          }
        }
        for (const n of r.addedNodes) stripCloak(n);
      }
    });
    mo.observe(document.body, {
      childList: true,
      subtree: true,
      attributes: true,
      attributeFilter: ['data-cloak'],
    });
  }
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', bootCloakStripper);
  } else {
    bootCloakStripper();
  }
})();
