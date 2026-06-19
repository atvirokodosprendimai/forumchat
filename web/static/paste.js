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

// Reads the file chosen from the native picker and pipes it into
// `signalName` via the existing FileReader path. Triggered by a
// <input type="file" onchange="fcPickImage(event,'image_data')"> whose
// label[for=...] opens the picker natively (no JS click needed).
window.fcPickImage = function (evt, signalName) {
  const f = evt.target.files && evt.target.files[0];
  if (!f) return;
  if (!f.type || !f.type.startsWith('image/')) {
    alert('Only image files are supported.');
    evt.target.value = '';
    return;
  }
  fcLoadBlob(f, signalName);
  evt.target.value = ''; // allow re-selecting the same file later
};

// NOTE: closing open <details class="msg-menu"> on outside-click / Escape used
// to live here as two global listeners. It's now declarative datastar on
// <body> in layout.templ (data-on:click__window / data-on:keydown__window),
// so this file is purely clipboard / file-picker / drag-drop image helpers.

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
