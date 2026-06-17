// chat-attach.js — Phase-2 file picker uploader for chat.
//
// Hooks the composer's hidden multi-file <input type="file" multiple>
// and the existing 📎 button so picking N files kicks off N XHR
// uploads in parallel against POST /c/<slug>/chat/upload. The server
// responds with JSON {id, mime, kind, size, filename}; we collect the
// returned IDs into the global $attachment_ids signal so the next
// chat/send POST submits them alongside body / reply_to_id.
//
// Phase-3 adds the drag-anywhere overlay, per-row progress UI, cancel,
// and retry. Phase 2 is intentionally bare so the schema + send path
// can land first.
(() => {
  const composer = document.querySelector('.composer');
  const pickerInput = composer && composer.querySelector('input[type="file"]');
  const messagesEl = document.querySelector('#messages');
  if (!composer || !pickerInput || !messagesEl) return;

  const slug = messagesEl.dataset.communitySlug || '';
  if (!slug) return;

  // Phase 2: forward the picker's change event to the uploader.
  // (paste.js owns image_data via fcPickImage — only run here when
  // multi-file pick is enabled, marked by `multiple` on the input.)
  if (!pickerInput.hasAttribute('multiple')) return;

  pickerInput.addEventListener('change', async (evt) => {
    const files = Array.from(evt.target.files || []);
    if (files.length === 0) return;
    evt.target.value = ''; // allow re-picking the same file
    await window.fcChatStageFiles(files);
  });

  // Public hook reused by Phase-3 drag-anywhere drop handler.
  window.fcChatStageFiles = async function (files) {
    const ids = readSignalArray('attachment_ids');
    for (const f of files) {
      try {
        const j = await uploadOne(f);
        ids.push(j.id);
      } catch (err) {
        console.error('[chat-attach] upload failed', f.name, err);
        alert('Upload failed: ' + f.name + ' — ' + (err && err.message ? err.message : err));
      }
    }
    writeSignalArray('attachment_ids', ids);
  };

  function uploadOne(file) {
    return new Promise((resolve, reject) => {
      const url = `/c/${encodeURIComponent(slug)}/chat/upload`;
      const xhr = new XMLHttpRequest();
      xhr.open('POST', url, true);
      xhr.withCredentials = true;
      xhr.responseType = 'text';
      xhr.onload = () => {
        if (xhr.status >= 200 && xhr.status < 300) {
          try { resolve(JSON.parse(xhr.responseText)); }
          catch (e) { reject(new Error('bad json: ' + e.message)); }
        } else {
          reject(new Error(xhr.status + ' ' + (xhr.responseText || xhr.statusText)));
        }
      };
      xhr.onerror = () => reject(new Error('network error'));
      const fd = new FormData();
      fd.append('file', file, file.name);
      xhr.send(fd);
    });
  }

  // ---- Datastar signal bridge ----
  // We mutate $attachment_ids via a hidden input bound to the signal,
  // matching the project pattern. The bridge takes JSON in / JSON out
  // since arrays don't round-trip through plain input values.
  function readSignalArray(name) {
    const host = document.querySelector(`[data-bind="${name}"]`);
    if (!host) return [];
    try { return JSON.parse(host.value || '[]'); }
    catch { return []; }
  }
  function writeSignalArray(name, arr) {
    let host = document.querySelector(`[data-bind="${name}"]`);
    if (!host) return;
    host.value = JSON.stringify(arr);
    host.dispatchEvent(new Event('input', { bubbles: true }));
  }
})();
