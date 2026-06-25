// Package docs holds the first-party developer documentation served at
// /dev/docs. Every Markdown file in this directory is compiled into the binary
// via embed.FS, so the docs ship with the app and need no external store, CMS,
// or network call to render.
//
// The Manifest is the single source of truth. It drives BOTH the /dev/docs
// index listing AND per-slug routing: a slug absent from the Manifest is a
// 404, so there is no way to read an arbitrary embedded file by guessing a
// path. Adding a page is therefore two edits — drop a Markdown file here and
// add one Manifest entry — and nothing else in the codebase needs to change.
//
// The content is TRUSTED, first-party Markdown (authored in-repo, reviewed,
// compiled in). The renderer in internal/devdocs deliberately does not run it
// through the user-content sanitizer, which lets heading anchors survive for
// the table of contents. Never wire user-submitted Markdown through this
// package.
package docs

import (
	"embed"
	"sort"
)

// files embeds the Markdown sources beside this file. The pattern matches only
// top-level *.md, so sibling directories (e.g. diagrams/) are not included.
// embed requires at least one match at build time, which the committed docs
// satisfy.
//
//go:embed *.md
var files embed.FS

// Doc is one published documentation page: the URL slug it lives at, the
// metadata shown on the index, and the embedded file that backs it.
type Doc struct {
	// Slug is the URL segment under /dev/docs/<slug>. It is matched against the
	// Manifest, never used to build a filesystem path from user input.
	Slug string
	// Title is the page <title> and the index card heading.
	Title string
	// Summary is the one-line description shown on the index card.
	Summary string
	// Icon is a short glyph shown on the index card (decorative).
	Icon string
	// File is the Markdown file name within the embedded FS.
	File string
	// Order positions the doc in the index and navigation (ascending).
	Order int
}

// Manifest is the ordered registry of published docs. Order, not slice
// position, decides display order — keep entries grouped logically here and let
// Order sort them. Editing this slice is how a doc is published or retired.
var Manifest = []Doc{
	{
		Slug:    "index",
		Title:   "Developer documentation",
		Summary: "Build on top of a community: integration surfaces, auth, and SDKs.",
		Icon:    "◇",
		File:    "index.md",
		Order:   0,
	},
	{
		Slug:    "connectors",
		Title:   "External connectors",
		Summary: "Run an outside program as a human chat member over a signed SSE stream.",
		Icon:    "🔌",
		File:    "connectors.md",
		Order:   1,
	},
	{
		Slug:    "webhooks",
		Title:   "Webhooks",
		Summary: "Stateless inbound bot messages and outbound event relay over plain HTTP.",
		Icon:    "🪝",
		File:    "webhooks.md",
		Order:   2,
	},
}

// List returns the published docs in display order (ascending Order). It
// returns a fresh sorted copy so callers cannot mutate the Manifest.
func List() []Doc {
	out := make([]Doc, len(Manifest))
	copy(out, Manifest)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

// Get returns the Doc registered for slug and its raw Markdown source. ok is
// false when the slug is not in the Manifest, which the handler maps to a 404.
// Because the file name comes from the trusted Manifest (never from the slug
// directly), there is no path-traversal surface here.
func Get(slug string) (doc Doc, src []byte, ok bool) {
	for _, d := range Manifest {
		if d.Slug == slug {
			b, err := files.ReadFile(d.File)
			if err != nil {
				// A Manifest entry whose file is missing is a build/authoring
				// bug, not a user error; report not-found rather than panic.
				return Doc{}, nil, false
			}
			return d, b, true
		}
	}
	return Doc{}, nil, false
}
