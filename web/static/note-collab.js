// note-collab.js — collaborative markdown editing for the notes editor.
//
// Differential synchronization (Neil Fraser) with the server as sequencer:
//   - The textarea is the LOCAL text. `shadow` is the last canonical we agreed
//     with the server on.
//   - On edit (debounced) fcNoteCollabPatch() diffs shadow→local, returns a
//     diff-match-patch patch (sent to /sync as the `note_patch` signal), and
//     advances shadow optimistically so the next diff is only the new delta.
//   - The server fuzzy-applies that patch onto the canonical body (merging
//     concurrent edits), bumps version, and pushes the new canonical back as the
//     `_note_canon` / `_note_ver` signals over the collab SSE stream.
//   - fcNoteCollabApply() merges the server's change since our shadow into the
//     local textarea, preserving the caret, then sets shadow = canonical.
//
// The patch text format is the diff-match-patch standard, identical on both
// sides (go-diff on the server, diff_match_patch.js here), so they interop.

(function () {
  'use strict';
  if (typeof diff_match_patch === 'undefined') return; // library not loaded

  const state = { dmp: new diff_match_patch(), shadow: '', ver: 0 };
  window.__fcNote = state;

  function textarea() { return document.querySelector('.note-md'); }
  function fireInput(ta) { ta.dispatchEvent(new Event('input', { bubbles: true })); }

  // Seed shadow from the server-rendered textarea (== the canonical at render
  // time), so the first canonical push either matches (no-op) or merges cleanly,
  // and any edits typed before the stream connects diff against it (not lost).
  (function seed() {
    const ta = textarea();
    if (ta) state.shadow = ta.value;
    else document.addEventListener('DOMContentLoaded', function () { const t = textarea(); if (t) state.shadow = t.value; });
  })();

  // mapCaret returns where `pos` in oldText lands in newText, by walking the diff
  // and shifting the caret by net inserts/deletes before it.
  function mapCaret(oldText, newText, pos) {
    const diffs = state.dmp.diff_main(oldText, newText);
    let oldIdx = 0, newIdx = 0;
    for (let i = 0; i < diffs.length; i++) {
      const op = diffs[i][0], data = diffs[i][1];
      if (op === 0) { // equal
        if (oldIdx + data.length >= pos) return newIdx + (pos - oldIdx);
        oldIdx += data.length; newIdx += data.length;
      } else if (op === -1) { // delete (present in old, gone in new)
        if (oldIdx + data.length > pos) return newIdx; // caret was inside the deletion
        oldIdx += data.length;
      } else { // insert (new only)
        newIdx += data.length;
      }
    }
    return newIdx;
  }

  // fcNoteCollabPatch: compute the patch for the local edits since `shadow`, set
  // by the textarea's input handler into the note_patch signal. Returns '' when
  // nothing changed (so the caller skips the /sync post).
  window.fcNoteCollabPatch = function () {
    const ta = textarea();
    if (!ta) return '';
    const local = ta.value;
    if (local === state.shadow) return '';
    const patches = state.dmp.patch_make(state.shadow, local);
    state.shadow = local; // optimistic — the server echo reconciles
    return state.dmp.patch_toText(patches);
  };

  // fcNoteCollabApply: a server canonical arrived (initial sync, our own echo, or
  // someone else's edit). Merge it into the local textarea preserving the caret.
  window.fcNoteCollabApply = function (canon, ver) {
    if (typeof canon !== 'string') return;
    const ta = textarea();
    if (canon === state.shadow) { state.ver = ver; return; } // nothing new server-side
    if (!ta) { state.shadow = canon; state.ver = ver; return; }
    const local = ta.value;
    const patches = state.dmp.patch_make(state.shadow, canon);
    const applied = state.dmp.patch_apply(patches, local);
    const merged = applied[0];
    const pos = ta.selectionStart;
    const focused = document.activeElement === ta;
    state.shadow = canon; state.ver = ver;
    if (merged === local) return; // server change didn't touch our text
    const newPos = mapCaret(local, merged, pos);
    ta.value = merged;
    if (focused) { try { ta.setSelectionRange(newPos, newPos); } catch (e) {} }
    fireInput(ta); // keep datastar's note_body signal + preview in step
    if (window.fcNoteRenderCursors) window.fcNoteRenderCursors(); // re-place remote carets
  };
})();
