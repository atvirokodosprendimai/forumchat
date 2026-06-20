package search

import "testing"

// fuse is the heart of the feature: Reciprocal Rank Fusion over two ranked lists.
// These tests pin the math and the merge semantics.

func TestFuseRanksByReciprocalRank(t *testing.T) {
	// A: top of FTS (rank 0) AND deep in semantic (rank 3) — should win on the
	// summed contribution. B: second in FTS only. C: top of semantic only.
	fts := []Hit{
		{Kind: "thread", RefID: "A"},
		{Kind: "thread", RefID: "B"},
	}
	vec := []Hit{
		{Kind: "issue", RefID: "C"},
		{Kind: "thread", RefID: "X"},
		{Kind: "thread", RefID: "Y"},
		{Kind: "thread", RefID: "A"},
	}

	got := fuse("", fts, vec)

	wantOrder := []string{"A", "C", "B", "X", "Y"}
	if len(got) != len(wantOrder) {
		t.Fatalf("got %d results, want %d: %+v", len(got), len(wantOrder), got)
	}
	for i, ref := range wantOrder {
		if got[i].RefID != ref {
			t.Fatalf("rank %d = %q, want %q (full order: %+v)", i, got[i].RefID, ref, refs(got))
		}
	}

	// A appears in both lists at ranks 0 (fts) and 3 (vec), weighted by its kind.
	wantA := (1.0/(rrfK+0) + 1.0/(rrfK+3)) * kindWeight("thread")
	if d := got[0].Score - wantA; d > 1e-9 || d < -1e-9 {
		t.Errorf("A score = %v, want %v", got[0].Score, wantA)
	}
	if !got[0].InFulltext || !got[0].InSemantic {
		t.Errorf("A should be tagged in both indexes: %+v", got[0])
	}
}

func TestFuseSemanticOnlyOrEmpty(t *testing.T) {
	// RAG disabled: only the FTS list is fused, order preserved.
	fts := []Hit{
		{Kind: "thread", RefID: "1"},
		{Kind: "post", RefID: "2"},
		{Kind: "chat", RefID: "3"},
	}
	got := fuse("", fts, nil)
	for i, ref := range []string{"1", "2", "3"} {
		if got[i].RefID != ref {
			t.Fatalf("rank %d = %q, want %q", i, got[i].RefID, ref)
		}
		if !got[i].InFulltext || got[i].InSemantic {
			t.Errorf("%q index tags wrong: %+v", ref, got[i])
		}
	}
}

func TestFusePrefersFTSSnippet(t *testing.T) {
	fts := []Hit{{Kind: "thread", RefID: "A", Snippet: "has «mark» highlight"}}
	vec := []Hit{{Kind: "thread", RefID: "A", Title: "Real Title", Snippet: "plain semantic"}}
	got := fuse("", fts, vec)
	if len(got) != 1 {
		t.Fatalf("expected merge to one result, got %d", len(got))
	}
	if got[0].Snippet != "has «mark» highlight" {
		t.Errorf("snippet = %q, want the FTS-highlighted one", got[0].Snippet)
	}
	if got[0].Title != "Real Title" {
		t.Errorf("title = %q, want the semantic title backfilled", got[0].Title)
	}
}

func TestTitleBonusFloatsExactMatch(t *testing.T) {
	// The reported bug: "test" exactly matches a project title, but the project
	// only surfaces at semantic rank 3 while a chat message matching "test" is at
	// FTS rank 0. Plain RRF ranks the chat first; the exact-title bonus + project
	// weight must float the project to the top.
	fts := []Hit{{Kind: "chat", RefID: "c1"}}
	vec := []Hit{
		{Kind: "thread", RefID: "t1", Title: "testing strategy"},
		{Kind: "issue", RefID: "i1", Title: "flaky CI"},
		{Kind: "post", RefID: "p1"},
		{Kind: "project", RefID: "pr1", Title: "Test"}, // exact (case-insensitive)
	}
	got := fuse("test", fts, vec)
	if got[0].RefID != "pr1" {
		t.Fatalf("want project pr1 first (exact title match), got %q (order %v)", got[0].RefID, refs(got))
	}
}

func TestKindWeightOrdersStructuredAboveChat(t *testing.T) {
	// Same query found a chat and a thread, both at rank 0 of their list, no
	// title match. The thread's kind weight must rank it above the chat.
	fts := []Hit{{Kind: "chat", RefID: "c1"}}
	vec := []Hit{{Kind: "thread", RefID: "t1", Title: "unrelated"}}
	got := fuse("zzz", fts, vec)
	if got[0].RefID != "t1" {
		t.Fatalf("want thread first by kind weight, got %q", got[0].RefID)
	}
}

func TestFTSQuery(t *testing.T) {
	cases := map[string]string{
		"hello world": `"hello" "world"`,
		"  spaced  ":  `"spaced"`,
		"":            "",
		`a"b`:         `"a""b"`, // embedded quote doubled, FTS5-safe
		"OR AND":      `"OR" "AND"`, // operators neutralized to literal terms
	}
	for in, want := range cases {
		if got := ftsQuery(in); got != want {
			t.Errorf("ftsQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func refs(rs []Result) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.RefID
	}
	return out
}
