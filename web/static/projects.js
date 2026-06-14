// projects.js — drag-and-drop attachment uploader for /c/{slug}/projects/{id}.
//
// One concern only for now: take files dropped onto (or chosen via the
// hidden input inside) the attachment dropzone, POST them as multipart
// to the URL on the zone's data-rooms-projects-dropzone attribute, and
// let the SSE stream morph the list when the server publishes.
//
// We intentionally do NOT add an optimistic file row — the SSE morph
// arrives in well under a second, and faking the row would mean having
// to remove it on failure. The dropzone shows a brief "Uploading…"
// state instead.
(() => {
  function postFiles(url, files) {
    const fd = new FormData();
    for (const f of files) fd.append('files', f, f.name);
    return fetch(url, {
      method: 'POST',
      body: fd,
      credentials: 'same-origin',
    });
  }

  function flashUploading(zone, on) {
    zone.classList.toggle('project-dropzone-busy', on);
  }

  function flashDragOver(zone, on) {
    zone.classList.toggle('project-dropzone-over', on);
  }

  function bindZone(zone) {
    const url = zone.dataset.roomsProjectsDropzone;
    if (!url) return;
    const input = zone.querySelector('input[type="file"]');

    if (input) {
      input.addEventListener('change', async () => {
        if (!input.files || input.files.length === 0) return;
        flashUploading(zone, true);
        try { await postFiles(url, input.files); }
        finally { flashUploading(zone, false); input.value = ''; }
      });
    }

    zone.addEventListener('dragover', (ev) => {
      ev.preventDefault();
      flashDragOver(zone, true);
    });
    zone.addEventListener('dragleave', () => flashDragOver(zone, false));
    zone.addEventListener('drop', async (ev) => {
      ev.preventDefault();
      flashDragOver(zone, false);
      const files = ev.dataTransfer?.files;
      if (!files || files.length === 0) return;
      flashUploading(zone, true);
      try { await postFiles(url, files); }
      finally { flashUploading(zone, false); }
    });
  }

  // Datastar morphs the dropzone subtree on every SSE attachments event,
  // which throws away any listeners we attached the first time. A
  // MutationObserver on the panel re-binds the new dropzone instance.
  function bindAll() {
    document.querySelectorAll('[data-rooms-projects-dropzone]').forEach(bindZone);
  }

  bindAll();
  const panel = document.querySelector('#proj-attachments');
  if (panel) {
    new MutationObserver(bindAll).observe(panel.parentNode, {
      childList: true, subtree: true,
    });
  }
})();
