// fcPasteImage — called from data-on:paste handlers in chat and forum forms.
// Walks clipboardData for the first image item, reads it as a data URL, and
// writes it into the hidden input bound to `signalName`. The 'input' event
// dispatch lets datastar pick up the new value into the matching signal.
//
// Server side imposes the real limit (1 MB after base64 decode); the client
// limit here is just an early UX bail-out.
const FC_PASTE_MAX = 1024 * 1024;

window.fcPasteImage = function (evt, signalName) {
  const items =
    (evt.clipboardData && evt.clipboardData.items) ||
    (window.clipboardData && window.clipboardData.items);
  if (!items) return;
  for (const it of items) {
    if (it.kind === 'file' && it.type && it.type.startsWith('image/')) {
      evt.preventDefault();
      const blob = it.getAsFile();
      if (!blob) return;
      if (blob.size > FC_PASTE_MAX) {
        alert('Image too large — max ' + (FC_PASTE_MAX / 1024 / 1024) + ' MB.');
        return;
      }
      const reader = new FileReader();
      reader.onload = function () {
        const input = document.querySelector('[data-bind="' + signalName + '"]');
        if (!input) return;
        input.value = reader.result;
        input.dispatchEvent(new Event('input', { bubbles: true }));
      };
      reader.readAsDataURL(blob);
      return;
    }
  }
};
