// nav.js — data-cloak re-stripper.
//
// The mobile drawer (hamburger / overlay / presence panel) is now pure
// datastar: signals `_nav_open` / `_presence_open` toggle the `nav-open` /
// `presence-open` classes on <body> via data-class in layout.templ, and Escape
// / outside-click are data-on:*__window there too. Active-link highlighting is
// server-rendered (aria-current in layout.templ via navActive). This file is
// left with the one thing datastar can't express declaratively:
//
// `[data-cloak] { display: none !important }` is the FOUC guard for initial
// page render. The INITIAL strip is owned by datastar's data-init on <body>
// (layout.templ) — not this file — because data-init cannot fire until datastar
// has loaded, so the cloak stays (hiding the element) for the whole CDN-load
// window and is removed in the same hydration pass that applies data-show. No
// gap, no flash. (Stripping here on DOMContentLoaded raced datastar: nav.js is
// a local defer script and ran first, unhiding fixed full-screen lightboxes
// ~0.5s before data-show could take over.)
//
// This file only re-strips data-cloak that server SSE patches (PatchElementTempl
// etc.) re-inject — those land after datastar is already up, so there's no race.
// Without a re-stripper, patched elements would stay invisible forever.
(() => {
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
