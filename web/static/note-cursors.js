// note-cursors.js — render the OTHER editors' carets over the markdown textarea.
//
// The server tracks each editor's caret offset (notes.Presence) and pushes the
// others' carets as the `_note_cursors` signal (a JSON string of {id,name,color,
// pos}) over the collab stream. fcNoteRenderCursors() places a colored caret +
// name label at each offset, computed with the classic mirror-div technique
// (a hidden div styled exactly like the textarea; a marker span at the offset
// gives pixel coordinates). fcCaretPos() reports our own caret to the server.

(function () {
  'use strict';

  function ta() { return document.querySelector('.note-md'); }
  function overlay() { return document.querySelector('.note-cursors-overlay'); }

  window.fcCaretPos = function () { const t = ta(); return t ? t.selectionStart : 0; };

  // Properties copied onto the mirror so its text wraps identically to the textarea.
  const MIRROR_PROPS = ['boxSizing', 'width', 'height', 'overflowX', 'overflowY',
    'borderTopWidth', 'borderRightWidth', 'borderBottomWidth', 'borderLeftWidth',
    'paddingTop', 'paddingRight', 'paddingBottom', 'paddingLeft',
    'fontStyle', 'fontVariant', 'fontWeight', 'fontStretch', 'fontSize', 'fontSizeAdjust',
    'lineHeight', 'fontFamily', 'textAlign', 'textTransform', 'textIndent',
    'textDecoration', 'letterSpacing', 'wordSpacing', 'tabSize', 'MozTabSize'];
  let mirror;

  function caretCoords(el, pos) {
    if (!mirror) { mirror = document.createElement('div'); mirror.setAttribute('aria-hidden', 'true'); document.body.appendChild(mirror); }
    const cs = getComputedStyle(el), s = mirror.style;
    s.whiteSpace = 'pre-wrap'; s.wordWrap = 'break-word';
    s.position = 'absolute'; s.visibility = 'hidden'; s.top = '0'; s.left = '-9999px';
    MIRROR_PROPS.forEach(function (p) { s[p] = cs[p]; });
    mirror.textContent = el.value.substring(0, pos);
    const span = document.createElement('span');
    span.textContent = el.value.substring(pos) || '.';
    mirror.appendChild(span);
    let h = parseInt(cs.lineHeight, 10);
    if (!h) h = Math.round(parseFloat(cs.fontSize) * 1.4);
    const coords = {
      top: span.offsetTop + parseInt(cs.borderTopWidth, 10),
      left: span.offsetLeft + parseInt(cs.borderLeftWidth, 10),
      height: h,
    };
    mirror.textContent = '';
    return coords;
  }

  let lastCursors = [];
  // fcNoteRenderCursors(json?) — json is the _note_cursors signal (a JSON string
  // or array); omit to re-draw with the last set (after a scroll / text change).
  window.fcNoteRenderCursors = function (json) {
    if (json !== undefined) {
      try { lastCursors = typeof json === 'string' ? JSON.parse(json || '[]') : (json || []); }
      catch (e) { return; }
    }
    draw();
  };

  function draw() {
    const t = ta(), ov = overlay();
    if (!t || !ov) return;
    ov.innerHTML = '';
    lastCursors.forEach(function (c) {
      const pos = Math.min(Math.max(0, c.pos | 0), t.value.length);
      const co = caretCoords(t, pos);
      const top = co.top - t.scrollTop, left = co.left - t.scrollLeft;
      if (top < -co.height || top > t.clientHeight) return; // scrolled out of view
      const caret = document.createElement('div');
      caret.className = 'note-remote-caret';
      caret.style.top = top + 'px';
      caret.style.left = left + 'px';
      caret.style.height = co.height + 'px';
      caret.style.background = c.color || '#888';
      const label = document.createElement('span');
      label.className = 'note-remote-label';
      label.textContent = c.name || 'editor';
      label.style.background = c.color || '#888';
      caret.appendChild(label);
      ov.appendChild(caret);
    });
  }

  // Re-draw when the textarea scrolls or the window resizes (coords shift).
  document.addEventListener('scroll', function (e) {
    if (e.target && e.target.classList && e.target.classList.contains('note-md')) draw();
  }, true);
  window.addEventListener('resize', draw);
})();
