// Paste + drop image helpers. Both write a data: URL into the hidden input
// bound to `signalName`; the dispatched 'input' event lets datastar sync the
// matching signal.
//
// Server side imposes the real limit (1 MiB after base64 decode); the client
// limit here is just an early UX bail-out.
const FC_PASTE_MAX = 1024 * 1024;

function fcWriteSignal(signalName, dataURL) {
  const input = document.querySelector('[data-bind="' + signalName + '"]');
  if (!input) return;
  input.value = dataURL;
  input.dispatchEvent(new Event('input', { bubbles: true }));
}

function fcLoadBlob(blob, signalName) {
  if (!blob) return;
  if (blob.size > FC_PASTE_MAX) {
    alert('Image too large — max ' + (FC_PASTE_MAX / 1024 / 1024) + ' MB.');
    return;
  }
  const reader = new FileReader();
  reader.onload = function () { fcWriteSignal(signalName, reader.result); };
  reader.readAsDataURL(blob);
}

window.fcPasteImage = function (evt, signalName) {
  const items =
    (evt.clipboardData && evt.clipboardData.items) ||
    (window.clipboardData && window.clipboardData.items);
  if (!items) return;
  for (const it of items) {
    if (it.kind === 'file' && it.type && it.type.startsWith('image/')) {
      evt.preventDefault();
      fcLoadBlob(it.getAsFile(), signalName);
      return;
    }
  }
};

window.fcDropImage = function (evt, signalName) {
  const dt = evt.dataTransfer;
  if (!dt) return;
  // Prefer the modern items API (gives type info before fetching the file).
  if (dt.items && dt.items.length) {
    for (const it of dt.items) {
      if (it.kind === 'file' && it.type && it.type.startsWith('image/')) {
        evt.preventDefault();
        fcLoadBlob(it.getAsFile(), signalName);
        return;
      }
    }
  }
  // Fallback for older browsers.
  if (dt.files && dt.files.length) {
    for (const f of dt.files) {
      if (f.type && f.type.startsWith('image/')) {
        evt.preventDefault();
        fcLoadBlob(f, signalName);
        return;
      }
    }
  }
};
