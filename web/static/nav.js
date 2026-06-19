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
// page render. The body's data-init strips it once at mount — but server SSE
// patches (PatchElementTempl etc.) ship new HTML that re-injects data-cloak.
// Without a re-stripper, those patched elements stay invisible forever. We
// watch the document for node insertions AND data-cloak attribute changes and
// strip it everywhere.
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
