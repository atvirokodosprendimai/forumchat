package docs

import "testing"

// TestManifestResolves guards against a broken docs build: every Manifest entry
// must point at a file that actually exists in the embedded FS. Without this a
// missing file silently serves a 404 (Get returns ok=false) instead of failing
// loudly — a build/authoring bug that would otherwise ship unnoticed.
func TestManifestResolves(t *testing.T) {
	if len(Manifest) == 0 {
		t.Fatal("Manifest is empty")
	}
	seen := make(map[string]bool, len(Manifest))
	for _, d := range Manifest {
		if d.Slug == "" || d.Title == "" || d.File == "" {
			t.Errorf("incomplete Manifest entry: %+v", d)
		}
		if seen[d.Slug] {
			t.Errorf("duplicate slug %q", d.Slug)
		}
		seen[d.Slug] = true

		got, src, ok := Get(d.Slug)
		if !ok {
			t.Errorf("Get(%q) not found — file %q missing from embed?", d.Slug, d.File)
			continue
		}
		if got.Slug != d.Slug {
			t.Errorf("Get(%q) returned slug %q", d.Slug, got.Slug)
		}
		if len(src) == 0 {
			t.Errorf("doc %q has empty source", d.Slug)
		}
	}
}

// TestGetUnknownSlug confirms an unregistered slug is a clean miss (the 404
// path), never a panic.
func TestGetUnknownSlug(t *testing.T) {
	if _, _, ok := Get("does-not-exist"); ok {
		t.Error("Get(unknown) reported ok=true")
	}
}

// TestListSorted confirms List returns docs in ascending Order and is a copy
// the caller cannot use to mutate the Manifest.
func TestListSorted(t *testing.T) {
	got := List()
	for i := 1; i < len(got); i++ {
		if got[i-1].Order > got[i].Order {
			t.Errorf("List not sorted by Order at index %d: %d > %d", i, got[i-1].Order, got[i].Order)
		}
	}
	if len(got) > 0 {
		got[0].Slug = "mutated"
		if Manifest[0].Slug == "mutated" {
			t.Error("List did not return a defensive copy — Manifest was mutated")
		}
	}
}
