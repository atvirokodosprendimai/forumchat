// Download a fenced code block as a file. The button lives in a
// <figcaption class="codeblock-bar"> appended by render.DownloadableCode; it
// reads the sibling <pre><code> textContent and saves it via a Blob. Pure
// client-side — no server round-trip — and re-binds after every Datastar morph
// because the button is part of the morphed fragment.
window.fcDownloadCode = function (btn) {
  const fig = btn.closest('.codeblock');
  const code = fig && fig.querySelector('pre code');
  if (!code) return;
  const text = code.textContent || '';
  const blob = new Blob([text], { type: btn.dataset.mime || 'text/plain' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = 'snippet.' + (btn.dataset.ext || 'txt');
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(function () { URL.revokeObjectURL(url); }, 0);
};
