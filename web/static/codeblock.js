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
