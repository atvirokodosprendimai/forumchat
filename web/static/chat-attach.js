// chat-attach.js — drag-anywhere chat composer uploader.
//
// Three jobs:
//   1. File picker (📎 button) accepts multiple files and queues them.
//   2. Drag-over the whole .chat-layout shows a "Drop to attach"
//      overlay; drop queues the files. dragover preventDefault stops
//      Chrome from navigating to the file when the user misses the
//      overlay.
//   3. Each queued file gets a row in #composer-pending with a small
//      thumbnail / MIME icon, filename, progress bar, and ✕ cancel.
//      On success the row fades out and the upload's id is staged in
//      the $attachment_ids signal so the next chat/send POST carries
//      it. On failure the row goes red with a "retry" link.
//
// Phase 4 will hand off the bubble-side render (image / video / audio
// / pdf inline previews). Phase 3 keeps the bubble side as the chip
// shipped in Phase 2.
(() => {
  const composer = document.querySelector('.composer');
  const pickerInput = composer && composer.querySelector('input[type="file"]');
  const messagesEl = document.querySelector('#messages');
  const layout = document.querySelector('.chat-layout');
  const overlay = document.querySelector('#chat-drop-overlay');
  const pendingHost = document.querySelector('#composer-pending');
  if (!composer || !pickerInput || !messagesEl || !layout || !pendingHost) return;

  const slug = messagesEl.dataset.communitySlug || '';
  if (!slug) return;

  // ---- Hoisted state ----
  let dragDepth = 0; // dragenter / dragleave fire per child element, count to know when to hide
  let rowSeq = 0;
  const rows = new Map(); // rowId → { id, file, xhr, state }

  // ---- 1. File picker ----
  pickerInput.setAttribute('multiple', 'multiple');
  pickerInput.setAttribute('accept', '*/*');
  pickerInput.addEventListener('change', (evt) => {
    const files = Array.from(evt.target.files || []);
    evt.target.value = '';
    files.forEach(queueFile);
  });

  // ---- 2. Drag-anywhere overlay ----
  layout.addEventListener('dragenter', (evt) => {
    if (!hasFiles(evt)) return;
    dragDepth++;
    overlay.classList.add('chat-drop-overlay-active');
  });
  layout.addEventListener('dragleave', (evt) => {
    if (!hasFiles(evt)) return;
    dragDepth--;
    if (dragDepth <= 0) {
      dragDepth = 0;
      overlay.classList.remove('chat-drop-overlay-active');
    }
  });
  layout.addEventListener('dragover', (evt) => {
    if (hasFiles(evt)) evt.preventDefault();
  });
  layout.addEventListener('drop', (evt) => {
    if (!hasFiles(evt)) return;
    evt.preventDefault();
    dragDepth = 0;
    overlay.classList.remove('chat-drop-overlay-active');
    const files = Array.from(evt.dataTransfer.files || []);
    files.forEach(queueFile);
  });

  // ---- 3. queueFile → row + upload ----
  function queueFile(file) {
    const rowId = 'row-' + (++rowSeq);
    const row = renderRow(rowId, file);
    pendingHost.appendChild(row);
    startUpload(rowId, file, row);
  }

  function renderRow(rowId, file) {
    const div = document.createElement('div');
    div.className = 'composer-pending-row';
    div.dataset.rowId = rowId;
    div.innerHTML = `
      <span class="composer-pending-icon">${escapeHtml(iconForFile(file))}</span>
      <span class="composer-pending-meta">
        <span class="composer-pending-name">${escapeHtml(file.name)}</span>
        <span class="composer-pending-size">${humanSize(file.size)}</span>
      </span>
      <span class="composer-pending-bar"><span class="composer-pending-fill"></span></span>
      <button type="button" class="composer-pending-cancel" aria-label="Cancel">✕</button>
    `;
    div.querySelector('.composer-pending-cancel').addEventListener('click', () => cancelRow(rowId));
    return div;
  }

  function startUpload(rowId, file, row) {
    const url = `/c/${encodeURIComponent(slug)}/chat/upload`;
    const xhr = new XMLHttpRequest();
    xhr.open('POST', url, true);
    xhr.withCredentials = true;
    xhr.responseType = 'text';
    const fillEl = row.querySelector('.composer-pending-fill');
    xhr.upload.addEventListener('progress', (evt) => {
      if (!evt.lengthComputable) return;
      const pct = Math.round((evt.loaded / evt.total) * 100);
      fillEl.style.width = pct + '%';
    });
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        try {
          const j = JSON.parse(xhr.responseText);
          rows.set(rowId, { ...rows.get(rowId), state: 'done', uploadID: j.id });
          stageID(j.id);
          row.classList.add('composer-pending-done');
          fillEl.style.width = '100%';
          // Fade the row out after a moment so the staged-count UI takes over.
          setTimeout(() => { row.remove(); rows.delete(rowId); }, 600);
        } catch (e) {
          failRow(rowId, row, 'bad json from server');
        }
      } else {
        failRow(rowId, row, (xhr.responseText || xhr.statusText || ('http ' + xhr.status)).trim());
      }
    };
    xhr.onerror = () => failRow(rowId, row, 'network error');
    xhr.onabort  = () => { /* cancel path handles UI */ };
    const fd = new FormData();
    fd.append('file', file, file.name);
    rows.set(rowId, { id: rowId, file, xhr, state: 'uploading' });
    xhr.send(fd);
  }

  function failRow(rowId, row, msg) {
    rows.set(rowId, { ...rows.get(rowId), state: 'failed' });
    row.classList.add('composer-pending-fail');
    let errEl = row.querySelector('.composer-pending-error');
    if (!errEl) {
      errEl = document.createElement('span');
      errEl.className = 'composer-pending-error';
      row.appendChild(errEl);
    }
    errEl.innerHTML = `${escapeHtml(msg || 'failed')} — <button type="button" class="link composer-pending-retry">retry</button>`;
    errEl.querySelector('.composer-pending-retry').addEventListener('click', () => {
      const e = rows.get(rowId);
      if (!e) return;
      row.classList.remove('composer-pending-fail');
      errEl.remove();
      row.querySelector('.composer-pending-fill').style.width = '0%';
      startUpload(rowId, e.file, row);
    });
  }

  function cancelRow(rowId) {
    const e = rows.get(rowId);
    if (!e) return;
    if (e.state === 'uploading' && e.xhr) {
      try { e.xhr.abort(); } catch {}
    }
    if (e.state === 'done' && e.uploadID) {
      unstageID(e.uploadID);
    }
    const row = pendingHost.querySelector(`[data-row-id="${rowId}"]`);
    if (row) row.remove();
    rows.delete(rowId);
  }

  // ---- Signal bridge ----
  function stageID(id) {
    const ids = readIDs();
    if (!ids.includes(id)) ids.push(id);
    writeIDs(ids);
  }
  function unstageID(id) {
    const ids = readIDs().filter(x => x !== id);
    writeIDs(ids);
  }
  function readIDs() {
    const host = document.querySelector('[data-bind="attachment_ids"]');
    if (!host) return [];
    try { return JSON.parse(host.value || '[]'); } catch { return []; }
  }
  function writeIDs(ids) {
    const host = document.querySelector('[data-bind="attachment_ids"]');
    if (!host) return;
    host.value = JSON.stringify(ids);
    host.dispatchEvent(new Event('input', { bubbles: true }));
  }

  // ---- helpers ----
  function hasFiles(evt) {
    if (!evt.dataTransfer) return false;
    const types = evt.dataTransfer.types || [];
    for (let i = 0; i < types.length; i++) if (types[i] === 'Files') return true;
    return false;
  }
  function iconForFile(f) {
    const t = f.type || '';
    if (t.startsWith('image/')) return '🖼';
    if (t.startsWith('video/')) return '🎬';
    if (t.startsWith('audio/')) return '🎵';
    if (t === 'application/pdf') return '📄';
    return '📎';
  }
  function humanSize(n) {
    const KiB = 1024, MiB = 1024 * KiB;
    if (n >= MiB) return (n / MiB).toFixed(1) + ' MB';
    if (n >= KiB) return Math.round(n / KiB) + ' KB';
    return n + ' B';
  }
  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, c => ({
      '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
    }[c]));
  }

  // Public hook retained from Phase 2 — callers can still inject a
  // FileList directly (test hooks, future paste integration).
  window.fcChatStageFiles = function (files) {
    Array.from(files).forEach(queueFile);
  };
})();
