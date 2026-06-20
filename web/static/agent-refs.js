// $-reference autocomplete for the agent composer (contenteditable editor).
//
// Typing "$" starts a query that CONTINUES through spaces (so multi-word thread
// titles can be searched). The query is sent to /agent/refs via the hidden
// `agent_ref_query` datastar input (debounced); the server patches the dropdown.
// Picking a result replaces the "$query" text with a single, non-editable,
// highlighted <span class="agent-ref">$Title</span> chunk.
(function () {
  function editor() { return document.getElementById('agent-editor'); }
  function dropdown() { return document.getElementById('agent-ref-results'); }
  function bodyInput() {
    var c = document.getElementById('agent-composer');
    return c ? c.querySelector('[data-bind="agent_body"]') : null;
  }
  function refQueryInput() { return document.getElementById('agent-ref-query'); }
  function refsInput() { return document.getElementById('agent-refs'); }

  // The editor's plain text — ref spans contribute their "$Title" text. NBSP is
  // normalised back to a regular space.
  function editorText(el) { return (el.textContent || '').replace(/ /g, ' '); }

  // Collect the inserted reference chunks as [{kind,id,title}] into the
  // agent_refs signal, so the server can expand them into thread content for
  // the model (the displayed message keeps the clean $Title).
  function syncRefs(el) {
    var inp = refsInput();
    if (!inp) return;
    var refs = [];
    el.querySelectorAll('.agent-ref').forEach(function (s) {
      if (s.dataset && s.dataset.id) {
        refs.push({ kind: s.dataset.kind || '', id: s.dataset.id, title: s.dataset.title || '' });
      }
    });
    inp.value = refs.length ? JSON.stringify(refs) : '';
    inp.dispatchEvent(new Event('input', { bubbles: true }));
  }

  function syncBody(el) {
    var inp = bodyInput();
    if (inp) {
      inp.value = editorText(el);
      inp.dispatchEvent(new Event('input', { bubbles: true }));
    }
    syncRefs(el);
  }

  // If the caret sits in a plain "$query" run (not inside a ref chunk), return
  // {node, start, end, query}; else null. The "$" must start the text or follow
  // whitespace.
  function activeQuery() {
    var sel = window.getSelection();
    if (!sel || sel.rangeCount === 0 || !sel.isCollapsed) return null;
    var node = sel.anchorNode;
    if (!node || node.nodeType !== Node.TEXT_NODE) return null;
    if (node.parentElement && node.parentElement.classList.contains('agent-ref')) return null;
    var text = node.textContent.slice(0, sel.anchorOffset);
    var dollar = text.lastIndexOf('$');
    if (dollar < 0) return null;
    if (dollar > 0 && !/\s/.test(text.charAt(dollar - 1))) return null;
    return { node: node, start: dollar, end: sel.anchorOffset, query: text.slice(dollar + 1) };
  }

  function open() { var d = dropdown(); if (d) d.classList.add('open'); }
  function close() { var d = dropdown(); if (d) { d.classList.remove('open'); clearActive(); } }
  function isOpen() { var d = dropdown(); return !!(d && d.classList.contains('open')); }
  function items() {
    var d = dropdown();
    return d ? Array.prototype.slice.call(d.querySelectorAll('.agent-ref-item')) : [];
  }
  function clearActive() { items().forEach(function (i) { i.classList.remove('active'); }); }
  function activeIndex() { var it = items(); for (var i = 0; i < it.length; i++) if (it[i].classList.contains('active')) return i; return -1; }
  function setActive(idx) {
    var it = items(); if (!it.length) return;
    idx = ((idx % it.length) + it.length) % it.length;
    clearActive(); it[idx].classList.add('active'); it[idx].scrollIntoView({ block: 'nearest' });
  }

  window.fcAgentEditorInput = function (el) {
    syncBody(el);
    var aq = activeQuery();
    if (aq && aq.query.length >= 1) {
      el._refRange = aq;
      var q = refQueryInput();
      if (q) { q.value = aq.query; q.dispatchEvent(new Event('input', { bubbles: true })); }
      open();
    } else {
      el._refRange = null;
      close();
    }
  };

  window.fcAgentEditorKeydown = function (evt, el) {
    if (evt.key === 'Escape') { if (isOpen()) { evt.preventDefault(); close(); } return; }
    if (isOpen() && items().length) {
      if (evt.key === 'ArrowDown') { evt.preventDefault(); setActive(activeIndex() + 1); return; }
      if (evt.key === 'ArrowUp') { evt.preventDefault(); setActive(activeIndex() - 1); return; }
      if (evt.key === 'Enter' || evt.key === 'Tab') {
        evt.preventDefault();
        var idx = activeIndex(); if (idx < 0) idx = 0;
        items()[idx].click();
        return;
      }
    }
    if (evt.key === 'Enter' && !evt.shiftKey) {
      evt.preventDefault();
      var btn = document.querySelector('#agent-composer .agent-send');
      if (btn) btn.click();
    }
  };

  window.fcAgentInsertRef = function (btn) {
    var el = editor();
    if (!el) return;
    var aq = el._refRange;
    var title = btn.getAttribute('data-title') || '';
    if (aq && aq.node && aq.node.parentNode) {
      var range = document.createRange();
      range.setStart(aq.node, aq.start);
      range.setEnd(aq.node, aq.end);
      range.deleteContents();
      var span = document.createElement('span');
      span.className = 'agent-ref';
      span.setAttribute('contenteditable', 'false');
      span.dataset.kind = btn.getAttribute('data-kind') || '';
      span.dataset.id = btn.getAttribute('data-id') || '';
      span.dataset.title = title;
      span.textContent = '$' + title;
      range.insertNode(span);
      var sp = document.createTextNode(' ');
      span.parentNode.insertBefore(sp, span.nextSibling);
      var r2 = document.createRange();
      r2.setStartAfter(sp); r2.collapse(true);
      var sel = window.getSelection(); sel.removeAllRanges(); sel.addRange(r2);
    }
    close();
    el._refRange = null;
    syncBody(el);
    el.focus();
  };

  // Cleared by datastar when agent_body is emptied after a send.
  window.fcAgentClearEditorIfNeeded = function () {
    var el = editor();
    if (el && el.textContent.trim() !== '') { el.innerHTML = ''; }
    close();
  };

  // Outside-click closes the dropdown.
  document.addEventListener('click', function (e) {
    if (!isOpen()) return;
    var d = dropdown(), ed = editor();
    if (d && d.contains(e.target)) return;
    if (ed && ed.contains(e.target)) return;
    close();
  });
})();
