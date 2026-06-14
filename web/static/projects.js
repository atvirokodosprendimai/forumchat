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

  // Track zones (and their inputs) that have already had listeners
  // attached. The SSE stream morphs sibling panels (header / todos /
  // comments / activity) too, each triggering our MutationObserver. We
  // were re-running bindZone on the SAME dropzone elements every morph,
  // stacking change/drop listeners. One file pick then fired the POST
  // 4-6 times. WeakSet keeps the bookkeeping per-element so morphdom-
  // preserved nodes only get bound once across the page lifetime.
  const boundZones = new WeakSet();
  const boundInputs = new WeakSet();

  function bindZone(zone) {
    if (boundZones.has(zone)) return;
    boundZones.add(zone);
    const url = zone.dataset.roomsProjectsDropzone;
    if (!url) return;
    const input = zone.querySelector('input[type="file"]');

    if (input && !boundInputs.has(input)) {
      boundInputs.add(input);
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

  // The issue dropzone is a thinner variant — same listener set, but
  // the data attribute is different so we have a separate select.
  function bindIssueZone(zone) {
    if (boundZones.has(zone)) return;
    boundZones.add(zone);
    const url = zone.dataset.roomsProjectsIssueDropzone;
    if (!url) return;
    const input = zone.querySelector('input[type="file"]');
    if (input && !boundInputs.has(input)) {
      boundInputs.add(input);
      input.addEventListener('change', async () => {
        if (!input.files || input.files.length === 0) return;
        flashUploading(zone, true);
        try { await postFiles(url, input.files); }
        finally { flashUploading(zone, false); input.value = ''; }
      });
    }
    zone.addEventListener('click', (ev) => {
      if (ev.target !== input) input?.click();
    });
    zone.addEventListener('dragover', (ev) => { ev.preventDefault(); flashDragOver(zone, true); });
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

  // Datastar morphs the dropzone subtree on every SSE attachments event.
  // morphdom keeps the same DOM nodes when their ids match (WithModeOuter
  // on #proj-attachments), so re-bind only fires on truly new zones.
  function bindAll() {
    document.querySelectorAll('[data-rooms-projects-dropzone]').forEach(bindZone);
    document.querySelectorAll('[data-rooms-projects-issue-dropzone]').forEach(bindIssueZone);
  }

  bindAll();
  const panel = document.querySelector('#proj-attachments');
  if (panel) {
    new MutationObserver(bindAll).observe(panel.parentNode, {
      childList: true, subtree: true,
    });
  }
  // For issue pages, the dropzone lives on a different parent.
  const issueRoot = document.querySelector('.project-panel-issue');
  if (issueRoot) {
    new MutationObserver(bindAll).observe(issueRoot, {
      childList: true, subtree: true,
    });
  }
})();
