// note.js — inline-comment affordances for the notes reader.
//
// The server renders #note-body with each top-level block tagged data-nb="<i>"
// (render.AnnotateBlocks) and a comments rail #note-comments. This script layers
// the interaction on top, emitting one custom event — fc:note-comment, detail
// {block, quote} — which a single Datastar listener on #note-reader consumes to
// open the composer (EDA). It never talks to the server directly.
//
// Three affordances:
//   1. Select text in a block  → a floating "💬 Comment" button → range comment.
//   2. Hover a block           → a gutter "+" button            → line comment.
//   3. Blocks that already have comments get a "💬 N" badge; clicking a badge
//      (or a rail card's anchor) highlights the block + its rail card.
//
// Re-runs after every fat-morph of #note-reader via a MutationObserver, so live
// comment patches re-attach the badges.

(function () {
  'use strict';

  function blockOf(node) {
    while (node && node !== document) {
      if (node.nodeType === 1 && node.hasAttribute && node.hasAttribute('data-nb')) return node;
      node = node.parentNode;
    }
    return null;
  }

  function emit(block, quote) {
    const target = document.getElementById('note-reader');
    if (!target) return;
    target.dispatchEvent(new CustomEvent('fc:note-comment', {
      bubbles: true,
      detail: { block: block, quote: quote || '' },
    }));
  }

  // --- floating selection button -------------------------------------------
  let selBtn = null;
  function selectionButton() {
    if (selBtn) return selBtn;
    selBtn = document.createElement('button');
    selBtn.type = 'button';
    selBtn.className = 'note-sel-btn';
    selBtn.textContent = '💬 Comment';
    selBtn.style.display = 'none';
    selBtn.addEventListener('mousedown', function (e) { e.preventDefault(); }); // keep selection
    selBtn.addEventListener('click', function () {
      const block = selBtn._block, quote = selBtn._quote;
      hideSelBtn();
      window.getSelection().removeAllRanges();
      emit(block, quote);
    });
    document.body.appendChild(selBtn);
    return selBtn;
  }
  function hideSelBtn() { if (selBtn) selBtn.style.display = 'none'; }

  function onSelection() {
    const body = document.getElementById('note-body');
    if (!body || body.getAttribute('data-can-comment') !== '1') return;
    const sel = window.getSelection();
    if (!sel || sel.isCollapsed || sel.rangeCount === 0) { hideSelBtn(); return; }
    const range = sel.getRangeAt(0);
    if (!body.contains(range.commonAncestorContainer)) { hideSelBtn(); return; }
    const text = sel.toString().trim();
    if (!text) { hideSelBtn(); return; }
    const block = blockOf(range.commonAncestorContainer);
    if (!block) { hideSelBtn(); return; }
    const rect = range.getBoundingClientRect();
    const btn = selectionButton();
    btn._block = Number(block.getAttribute('data-nb'));
    btn._quote = text.length > 280 ? text.slice(0, 280) : text;
    btn.style.display = 'block';
    btn.style.top = (window.scrollY + rect.top - 38) + 'px';
    btn.style.left = (window.scrollX + rect.left) + 'px';
  }

  // --- per-block gutter "+" + badges ---------------------------------------
  let observer = null;
  // paintBlocks mutates the DOM (adds gutter buttons + badges); guard against
  // the MutationObserver re-triggering it by detaching while we paint.
  function paint() {
    if (observer) observer.disconnect();
    try { paintBlocks(); } finally {
      if (observer) {
        const reader = document.getElementById('note-reader');
        if (reader) observer.observe(reader, { childList: true, subtree: true });
      }
    }
  }
  function paintBlocks() {
    const body = document.getElementById('note-body');
    const reader = document.getElementById('note-reader');
    if (!body) return;
    const canComment = body.getAttribute('data-can-comment') === '1';
    const counts = {};
    const raw = (reader && reader.getAttribute('data-comment-count')) || '';
    raw.split(',').forEach(function (s) { if (s !== '') counts[s] = (counts[s] || 0) + 1; });

    body.querySelectorAll('[data-nb]').forEach(function (block) {
      const i = block.getAttribute('data-nb');
      block.classList.add('note-block');
      // gutter add button (members only)
      if (canComment && !block.querySelector(':scope > .note-gutter-add')) {
        const add = document.createElement('button');
        add.type = 'button';
        add.className = 'note-gutter-add';
        add.textContent = '+';
        add.title = 'Comment on this line';
        add.setAttribute('contenteditable', 'false');
        add.addEventListener('click', function (e) {
          e.stopPropagation();
          emit(Number(i), '');
        });
        block.appendChild(add);
      }
      // count badge
      const existing = block.querySelector(':scope > .note-block-badge');
      if (counts[i]) {
        if (!existing) {
          const b = document.createElement('button');
          b.type = 'button';
          b.className = 'note-block-badge';
          b.addEventListener('click', function (e) { e.stopPropagation(); highlight(i); });
          block.appendChild(b);
          b.textContent = '💬 ' + counts[i];
        } else {
          const txt = '💬 ' + counts[i];
          if (existing.textContent !== txt) existing.textContent = txt;
        }
      } else if (existing) {
        existing.remove();
      }
    });

    // rail card → jump to block
    document.querySelectorAll('.note-comment-anchor[data-nb-jump]').forEach(function (a) {
      if (a._wired) return;
      a._wired = true;
      a.addEventListener('click', function () { highlight(a.getAttribute('data-nb-jump')); });
    });
  }

  function highlight(i) {
    const block = document.querySelector('#note-body [data-nb="' + i + '"]');
    if (block) {
      block.classList.add('note-block-hi');
      block.scrollIntoView({ block: 'center', behavior: 'smooth' });
      setTimeout(function () { block.classList.remove('note-block-hi'); }, 1600);
    }
    const card = document.querySelector('.note-comment[data-nb-ref="' + i + '"]');
    if (card) {
      card.classList.add('note-comment-hi');
      setTimeout(function () { card.classList.remove('note-comment-hi'); }, 1600);
    }
  }

  function init() {
    document.addEventListener('mouseup', function () { setTimeout(onSelection, 0); });
    document.addEventListener('scroll', hideSelBtn, true);
    // re-paint after fat-morphs of the reader (save / comment patches). The
    // observer is detached during paint() so our own badge/button writes don't
    // re-trigger it (that would be an infinite mutation loop).
    const reader = document.getElementById('note-reader');
    if (reader && window.MutationObserver) {
      observer = new MutationObserver(function () { paint(); });
    }
    paint();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
