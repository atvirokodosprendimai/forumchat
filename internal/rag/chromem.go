package rag

import (
	"context"
	"errors"
	"sync"

	chromem "github.com/philippgille/chromem-go"
)

// chromemCollection is the single logical collection name. Communities are
// partitioned by the community_id metadata field, not by separate collections —
// this keeps DeleteByRef community-agnostic and mirrors how the qdrant backend
// would shard by payload filter.
const chromemCollection = "forumchat"

// errPrecomputed is returned by the stub embedding func. It must never fire: we
// always supply embeddings on Add and query via QueryEmbedding, so chromem never
// needs to embed text itself.
var errPrecomputed = errors.New("rag: chromem store uses precomputed embeddings only")

// ChromemStore is a Store backed by chromem-go, a pure-Go embedded vector DB
// persisted to a directory. We use it as an index only — embeddings are computed
// by our Embedder and passed in, so chromem's own embedding function is a stub.
type ChromemStore struct {
	db  *chromem.DB
	mu  sync.Mutex // guards col across DropAll's delete+recreate
	col *chromem.Collection
}

// NewChromemStore opens (or creates) a persistent chromem DB at path. compress
// trades a little CPU for smaller on-disk vectors.
func NewChromemStore(path string) (*ChromemStore, error) {
	db, err := chromem.NewPersistentDB(path, true)
	if err != nil {
		return nil, err
	}
	col, err := db.GetOrCreateCollection(chromemCollection, nil, stubEmbed)
	if err != nil {
		return nil, err
	}
	return &ChromemStore{db: db, col: col}, nil
}

func stubEmbed(_ context.Context, _ string) ([]float32, error) { return nil, errPrecomputed }

func (s *ChromemStore) collection() *chromem.Collection {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.col
}

// Upsert replaces all chunks of (kind, refID): the old set is deleted first so a
// row whose content shrank doesn't leave stale trailing chunks behind.
func (s *ChromemStore) Upsert(ctx context.Context, communityID, kind, refID string, chunks []StoredChunk) error {
	if err := s.DeleteByRef(ctx, kind, refID); err != nil {
		return err
	}
	if len(chunks) == 0 {
		return nil
	}
	ids := make([]string, len(chunks))
	embeddings := make([][]float32, len(chunks))
	metadatas := make([]map[string]string, len(chunks))
	contents := make([]string, len(chunks))
	for i, c := range chunks {
		ids[i] = c.ID
		embeddings[i] = c.Embedding
		metadatas[i] = c.Metadata
		contents[i] = c.Content
	}
	return s.collection().Add(ctx, ids, embeddings, metadatas, contents)
}

func (s *ChromemStore) DeleteByRef(ctx context.Context, kind, refID string) error {
	return s.collection().Delete(ctx, map[string]string{"kind": kind, "ref_id": refID}, nil)
}

func (s *ChromemStore) DropCommunity(ctx context.Context, communityID string) error {
	return s.collection().Delete(ctx, map[string]string{"community_id": communityID}, nil)
}

// DropAll empties the index by deleting and recreating the collection.
func (s *ChromemStore) DropAll(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.db.DeleteCollection(chromemCollection); err != nil {
		return err
	}
	col, err := s.db.GetOrCreateCollection(chromemCollection, nil, stubEmbed)
	if err != nil {
		return err
	}
	s.col = col
	return nil
}

// Query returns the nearest chunks within one community. chromem rejects
// nResults greater than the collection size, so we clamp; an empty index yields
// no hits rather than an error.
func (s *ChromemStore) Query(ctx context.Context, communityID string, embedding []float32, limit int) ([]Hit, error) {
	col := s.collection()
	n := col.Count()
	if n == 0 || limit <= 0 {
		return nil, nil
	}
	if limit > n {
		limit = n
	}
	res, err := col.QueryEmbedding(ctx, embedding, limit, map[string]string{"community_id": communityID}, nil)
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(res))
	for _, r := range res {
		hits = append(hits, Hit{
			Kind:      r.Metadata["kind"],
			RefID:     r.Metadata["ref_id"],
			Title:     r.Metadata["title"],
			Snippet:   r.Content,
			CreatedAt: atoi64(r.Metadata["created_at"]),
			Score:     r.Similarity,
		})
	}
	return hits, nil
}

func (s *ChromemStore) Close() error { return nil }
