// Package rag implements semantic (vector) search over a community's public
// content — the asynchronous sibling of the FTS5 search_fts index. SQL triggers
// (migration 00039) enqueue changed rows into embed_outbox; a background Worker
// drains the queue, embeds each row's text via an Embedder (Ollama bge-m3), and
// upserts the resulting chunks into a Store (chromem-go now, qdrant later).
//
// The two collaborators are interfaces so the embedding model and the vector
// backend can each be swapped without touching the orchestration: Embedder and
// Store. The agent's internal MCP exposes Service.Search as a `rag_search` tool.
//
// Authorization is structural and lives in the loaders (source.go): only
// community-public content is ever indexed (e.g. AI messages only from shared
// threads), and every query is filtered by community_id. The vector store has no
// concept of users — keeping private content out of it is the loaders' job.
package rag

import "context"

// Content kinds. Each maps to one loader in source.go and one set of triggers in
// migration 00039. Adding a kind is: a loader + three triggers + a backfill row.
const (
	KindChat            = "chat"
	KindThread          = "thread"
	KindPost            = "post"
	KindIssue           = "issue"
	KindIssueComment    = "issue_comment"
	KindDiscussion      = "discussion"
	KindDiscussionReply = "discussion_reply"
	KindProject         = "project"
	KindAI              = "ai"
	KindPaste           = "paste"
)

// Outbox ops.
const (
	OpUpsert = "upsert"
	OpDelete = "delete"
)

// Doc is one indexable content row, resolved by a loader from (kind, ref_id).
// Title is optional context (prepended to the embedded text); Body is the main
// content. CommunityID picks the metadata partition the chunks are stored under.
type Doc struct {
	CommunityID string
	Kind        string
	RefID       string
	Title       string
	Body        string
	CreatedAt   int64
}

// StoredChunk is one embedded slice of a Doc, ready to upsert.
type StoredChunk struct {
	ID        string
	Content   string
	Metadata  map[string]string
	Embedding []float32
}

// Hit is one semantic-search result. It mirrors agent.SearchHit (the internal
// MCP shape) but rag stays a leaf package — main.go maps Hit → agent.SearchHit.
type Hit struct {
	Kind      string
	RefID     string
	Title     string
	Snippet   string
	CreatedAt int64
	Score     float32
}

// Embedder turns text into vectors. Embed processes a batch and returns one
// vector per input, in order. Dim reports the vector dimensionality (for store
// init / qdrant). Implementations must respect ctx.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dim() int
	Model() string
}

// Store is a vector index. It is a single logical collection partitioned by the
// community_id metadata field — Query and DropCommunity filter on it; DeleteByRef
// needs only (kind, ref_id) because ids are deterministic across communities.
type Store interface {
	// Upsert replaces all chunks of (kind, refID) with the given set. An empty
	// set is equivalent to DeleteByRef.
	Upsert(ctx context.Context, communityID, kind, refID string, chunks []StoredChunk) error
	// DeleteByRef removes every chunk belonging to one content row.
	DeleteByRef(ctx context.Context, kind, refID string) error
	// Query returns the nearest chunks within one community.
	Query(ctx context.Context, communityID string, embedding []float32, limit int) ([]Hit, error)
	// DropCommunity removes every chunk for one community (per-community reindex).
	DropCommunity(ctx context.Context, communityID string) error
	// DropAll empties the whole index (global reindex / backend reset).
	DropAll(ctx context.Context) error
	Close() error
}
