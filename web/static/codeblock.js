// Download a fenced code block as a file. The button lives in a
// <figcaption class="codeblock-bar"> appended by render.DownloadableCode; it
// reads the sibling <pre><code> textContent and saves it via a Blob. Pure
// client-side — no server round-trip — and re-binds after every Datastar morph
// because the button is part of the morphed fragment.
// fcDownloadText saves arbitrary text as a file. Shared by the code-block
// download button and any other "save this raw source" affordance.
window.fcDownloadText = function (text, filename, mime) {
  const blob = new Blob([text || ''], { type: mime || 'text/plain' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename || 'download.txt';
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(function () { URL.revokeObjectURL(url); }, 0);
};

window.fcDownloadCode = function (btn) {
  const fig = btn.closest('.codeblock');
  const code = fig && fig.querySelector('pre code');
  if (!code) return;
  window.fcDownloadText(code.textContent || '', 'snippet.' + (btn.dataset.ext || 'txt'), btn.dataset.mime);
};

// fcOpenHtmlPreview loads raw HTML into the global sandboxed preview iframe.
// The caller flips $_html_open in the same Datastar expression to show it.
window.fcOpenHtmlPreview = function (html) {
  const frame = document.getElementById('fc-html-frame');
  if (frame) frame.srcdoc = html || '';
};

window.fcPreviewCode = function (btn) {
  const fig = btn.closest('.codeblock');
  const code = fig && fig.querySelector('pre code');
  if (!code) return;
  window.fcOpenHtmlPreview(code.textContent || '');
};

// ── Whole-message raw source (SourceTools) ──────────────────────────────────
// The raw markdown lives in a hidden <pre class="body-source"> sibling of the
// rendered body. fcSourceHost finds the enclosing bubble that owns both.
function fcSourceHost(btn) {
  return btn.closest('.agent-bubble, article');
}
function fcSourceText(btn) {
  const host = fcSourceHost(btn);
  const code = host && host.querySelector('.body-source code');
  return code ? (code.textContent || '') : '';
}

// fcToggleSource swaps the bubble between the rendered view and its raw source.
window.fcToggleSource = function (btn) {
  const host = fcSourceHost(btn);
  if (!host) return;
  const on = host.classList.toggle('show-source');
  btn.innerHTML = on ? 'Rendered' : '&lt;/&gt; Source';
};

window.fcPreviewSource = function (btn) {
  window.fcOpenHtmlPreview(fcSourceText(btn));
};

window.fcDownloadSource = function (btn, filename) {
  const mime = /\.html?$/i.test(filename || '') ? 'text/html' : 'text/markdown';
  window.fcDownloadText(fcSourceText(btn), filename || 'source.txt', mime);
};
