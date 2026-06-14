// @mention typeahead helpers — caret-aware token scan + insert.
//
// fcMentionDetect inspects the textarea's caret position, looks backward for
// the most recent unbounded "@token", writes it into the `mention_query`
// signal via a hidden bound input, and returns true so the caller can
// trigger the backend search GET in the same Datastar expression.
//
// fcInsertMention replaces the active @token with `@DisplayName ` (trailing
// space so the next char doesn't extend the mention) and dispatches an
// input event so Datastar syncs the `body` signal.
//
// Bool signals like `mention_open` are flipped in the Datastar expression
// itself (`$mention_open=true`) — JS only touches the string signal.

(function () {
  const TOKEN_RE = /(^|\s)@([A-Za-z0-9_\-]{0,32})$/;

  function activeMention(el) {
    if (!el || typeof el.selectionStart !== 'number') return null;
    const caret = el.selectionStart;
    const upto = el.value.slice(0, caret);
    const m = TOKEN_RE.exec(upto);
    if (!m) return null;
    const tokenStart = caret - m[2].length - 1; // include the '@'
    return { token: m[2], start: tokenStart, end: caret };
  }

  function writeBoundString(name, value) {
    const host = document.querySelector('[data-bind="' + name + '"]');
    if (!host) return;
    host.value = value == null ? '' : String(value);
    host.dispatchEvent(new Event('input', { bubbles: true }));
  }

  window.fcMentionDetect = function (el) {
    const hit = activeMention(el);
    if (!hit) {
      writeBoundString('mention_query', '');
      if (el) el.__fcMentionRange = null;
      return false;
    }
    writeBoundString('mention_query', hit.token);
    el.__fcMentionRange = { start: hit.start, end: hit.end };
    // Fire the lookup only once the user has typed at least one char after
    // the `@` — an empty token would spam the server with empty queries.
    return hit.token.length >= 1;
  };

  window.fcInsertMention = function (name) {
    const el = document.querySelector('.composer-text');
    if (!el) return;
    const range = el.__fcMentionRange;
    if (!range) return;
    const before = el.value.slice(0, range.start);
    const after = el.value.slice(range.end);
    const insert = '@' + name + ' ';
    el.value = before + insert + after;
    const caret = (before + insert).length;
    el.focus();
    el.setSelectionRange(caret, caret);
    el.dispatchEvent(new Event('input', { bubbles: true }));
    el.__fcMentionRange = null;
    writeBoundString('mention_query', '');
  };
})();
