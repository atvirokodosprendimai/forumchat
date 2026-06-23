package rag

import (
	"context"
	"testing"
)

func TestCollectionName(t *testing.T) {
	if got := collectionName("abc-123_DEF"); got != "forumchat_abc-123_DEF" {
		t.Fatalf("collectionName = %q", got)
	}
	// Unsafe chars are mapped to '_'.
	if got := collectionName("a/b.c"); got != "forumchat_a_b_c" {
		t.Fatalf("collectionName sanitise = %q", got)
	}
}

func TestQdrantConnResolution(t *testing.T) {
	q := NewQdrantStore("http://platform:6333/", "platform-key", nil)

	// No resolver → default URL/key + derived collection.
	c := q.conn(context.Background(), "c1")
	if c.URL != "http://platform:6333" || c.APIKey != "platform-key" || c.Collection != "forumchat_c1" {
		t.Fatalf("default conn = %+v", c)
	}

	// BYO resolver overrides URL/key/collection; blanks fall back to default.
	q.Resolve = func(ctx context.Context, communityID string) QdrantConn {
		return QdrantConn{URL: "http://tenant:6333", Collection: ""} // key blank → fallback
	}
	c = q.conn(context.Background(), "c2")
	if c.URL != "http://tenant:6333" || c.APIKey != "platform-key" || c.Collection != "forumchat_c2" {
		t.Fatalf("byo conn = %+v", c)
	}
}
