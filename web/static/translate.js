// /translate composer typeahead helpers — slash-command detect + apply.
//
// fcTranslateDetect inspects the composer textarea: when its full value is
// "/translate <text>" it writes <text> into the `translate_query` signal (via
// the hidden bound input) and returns true so the caller fires the backend
// GET in the same Datastar expression. Otherwise it clears the signal and
// returns false. The bool signal `_translate_open` is flipped in the Datastar
// expression / by the server — JS only touches the string signal.
//
// fcApplyTranslation replaces the whole composer value with the chosen English
// translation and dispatches an input event so Datastar's data-bind syncs the
// `body` signal. The caller then clicks #composer-send, so the translation
// ships as the viewer through the normal send path (which clears the composer).

(function () {
  // Anchored at the start of the value; everything after "/translate " is the
  // text to translate. The trailing space is required so "/translate" alone (a
  // user still typing the command) doesn't fire a request.
  const TRANSLATE_RE = /^\/translate\s+([\s\S]+)$/i;

  function writeBoundString(name, value) {
    const host = document.querySelector('[data-bind="' + name + '"]');
    if (!host) return;
    host.value = value == null ? '' : String(value);
    host.dispatchEvent(new Event('input', { bubbles: true }));
  }

  window.fcTranslateDetect = function (el) {
    if (!el || typeof el.value !== 'string') {
      writeBoundString('translate_query', '');
      return false;
    }
    const m = TRANSLATE_RE.exec(el.value);
    if (!m) {
      writeBoundString('translate_query', '');
      return false;
    }
    const q = m[1].trim();
    writeBoundString('translate_query', q);
    return q.length >= 1;
  };

  window.fcApplyTranslation = function (text) {
    const el = document.querySelector('.composer-text');
    if (!el) return;
    el.value = text == null ? '' : String(text);
    el.dispatchEvent(new Event('input', { bubbles: true }));
    el.style.height = 'auto';
    writeBoundString('translate_query', '');
  };
})();
