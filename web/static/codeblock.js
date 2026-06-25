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

// fcCodeBlockText returns the raw source text of the fenced code block a Preview
// button belongs to. The caller assigns it into the FE-only $_html_src signal and
// flips $_html_open in the same Datastar expression; the global preview iframe
// binds its src to a data: URL built from those signals (web/templ/layout.templ),
// so there is no imperative iframe write here — an earlier imperative-set hybrid
// raced the reactive clear and could leave the overlay open over a blank frame.
window.fcCodeBlockText = function (btn) {
  const fig = btn.closest('.codeblock');
  const code = fig && fig.querySelector('pre code');
  return code ? (code.textContent || '') : '';
};

// ── Whole-message raw source (SourceTools) ──────────────────────────────────
// The raw markdown lives in a hidden <pre class="body-source"> sibling of the
// rendered body. fcSourceHost finds the enclosing bubble that owns both.
function fcSourceHost(btn) {
  return btn.closest('.agent-bubble, article, .paste-card');
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

// fcSourceText is exposed so the Preview button can assign the raw source into the
// FE-only $_html_src signal (see fcCodeBlockText for why the iframe is driven by a
// reactive data: URL on src, not an imperative write).
window.fcSourceText = fcSourceText;

window.fcDownloadSource = function (btn, filename) {
  const mime = /\.html?$/i.test(filename || '') ? 'text/html' : 'text/markdown';
  window.fcDownloadText(fcSourceText(btn), filename || 'source.txt', mime);
};

// fcDownloadReply (ReplyDownload) saves a whole prose reply as a file. Both the
// raw Markdown and the server-converted standalone HTML ride hidden in the
// bubble; fmt selects which to save. Same host lookup as the SourceTools helpers.
window.fcDownloadReply = function (btn, fmt, base) {
  const host = fcSourceHost(btn);
  if (!host) return;
  const html = fmt === 'html';
  const node = host.querySelector(html ? '.dl-html code' : '.dl-md code');
  if (!node) return;
  const name = (base || 'reply') + (html ? '.html' : '.md');
  window.fcDownloadText(node.textContent || '', name, html ? 'text/html' : 'text/markdown');
};
