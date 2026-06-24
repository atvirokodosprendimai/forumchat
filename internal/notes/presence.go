package notes

import (
	"hash/fnv"
	"sync"
)

// CursorInfo is one editor's caret in a note, sent to the OTHER editors so they
// can render a remote caret. JSON tags match what note-cursors.js reads.
type CursorInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
	Pos   int    `json:"pos"`
}

// Presence tracks, per note, each open editor's caret position. In-process only
// (one app node owns its editors' carets); NATS already fans the broadcast across
// nodes, and each node pushes its own editors' carets. Mirrors the per-note Bus.
type Presence struct {
	mu sync.Mutex
	m  map[string]map[string]CursorInfo // noteID -> editorID -> cursor
}

func NewPresence() *Presence { return &Presence{m: map[string]map[string]CursorInfo{}} }

// Set records (or refreshes) an editor's caret in a note.
func (p *Presence) Set(noteID, editorID, name string, pos int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	set, ok := p.m[noteID]
	if !ok {
		set = map[string]CursorInfo{}
		p.m[noteID] = set
	}
	set[editorID] = CursorInfo{ID: editorID, Name: name, Color: cursorColor(editorID), Pos: pos}
}

// Remove drops an editor when their collab stream disconnects.
func (p *Presence) Remove(noteID, editorID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if set, ok := p.m[noteID]; ok {
		delete(set, editorID)
		if len(set) == 0 {
			delete(p.m, noteID)
		}
	}
}

// Others returns every editor's caret in a note EXCEPT exceptID (the viewer
// doesn't render their own remote caret).
func (p *Presence) Others(noteID, exceptID string) []CursorInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]CursorInfo, 0, len(p.m[noteID]))
	for id, c := range p.m[noteID] {
		if id == exceptID {
			continue
		}
		out = append(out, c)
	}
	return out
}

// cursorPalette is a set of distinct, readable caret colors.
var cursorPalette = []string{
	"#e6194b", "#3cb44b", "#4363d8", "#f58231", "#911eb4",
	"#008080", "#f032e6", "#9a6324", "#800000", "#808000",
}

// cursorColor maps an editor id to a stable palette colour.
func cursorColor(editorID string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(editorID))
	return cursorPalette[int(h.Sum32())%len(cursorPalette)]
}
