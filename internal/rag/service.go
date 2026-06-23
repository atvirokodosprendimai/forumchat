package rag

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// Service ties the embedder, store, and chunker together. It is the write-side
// orchestrator (process one outbox job) and the read-side query (Search). It
// holds no HTTP/SSE concerns — handlers and the worker drive it.
type Service struct {
	Repo     *Repo
	Embedder Embedder
	Store    Store
	Chunk    ChunkConfig
	Log      *slog.Logger
	// EmbedderFor resolves a community's own embedder (model / Ollama host /
	// dim). Optional — nil uses the single Embedder (self-host, unchanged). Set
	// in main.go from community.ResolveRAG for the SaaS per-community path. A
	// community whose model differs from another gets a different vector size,
	// which the per-community Qdrant collection is sized to on first upsert.
	EmbedderFor func(ctx context.Context, communityID string) (Embedder, error)
}

// embedder returns the embedder for a community: the per-community resolver when
// set, else the single Service-wide embedder.
func (s *Service) embedder(ctx context.Context, communityID string) (Embedder, error) {
	if s.EmbedderFor != nil {
		return s.EmbedderFor(ctx, communityID)
	}
	return s.Embedder, nil
}

// NewService builds a Service.
func NewService(repo *Repo, embedder Embedder, store Store, chunk ChunkConfig, log *slog.Logger) *Service {
	return &Service{Repo: repo, Embedder: embedder, Store: store, Chunk: chunk, Log: log}
}

// Search embeds the query and returns the nearest community-public chunks,
// deduplicated to one hit per source row (best-scoring chunk wins). Overfetches
// before dedup so a row with several matching chunks doesn't crowd out others.
func (s *Service) Search(ctx context.Context, communityID, query string, limit int) ([]Hit, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 8
	}
	emb, err := s.embedder(ctx, communityID)
	if err != nil {
		return nil, fmt.Errorf("resolve embedder: %w", err)
	}
	vecs, err := emb.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(vecs) == 0 {
		return nil, nil
	}
	raw, err := s.Store.Query(ctx, communityID, vecs[0], limit*3)
	if err != nil {
		return nil, fmt.Errorf("vector query: %w", err)
	}
	return dedupByRef(raw, limit), nil
}

// dedupByRef collapses multiple chunk hits from the same source row into one
// (results arrive sorted by similarity, so the first seen is the best), capped
// at limit.
func dedupByRef(hits []Hit, limit int) []Hit {
	seen := make(map[string]struct{}, len(hits))
	out := make([]Hit, 0, limit)
	for _, h := range hits {
		key := h.Kind + ":" + h.RefID
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, h)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// process applies one outbox job: a delete removes the row's vectors; an upsert
// re-reads the row (the loader re-checks community-visibility), chunks, embeds,
// and replaces its vectors. A row that no longer qualifies is treated as a
// delete.
func (s *Service) process(ctx context.Context, it OutboxItem) error {
	if it.Op == OpDelete {
		return s.Store.DeleteByRef(ctx, it.Kind, it.RefID)
	}
	doc, ok, err := s.Repo.LoadDoc(ctx, it.Kind, it.RefID)
	if err != nil {
		return err
	}
	if !ok {
		return s.Store.DeleteByRef(ctx, it.Kind, it.RefID)
	}
	parts := chunk(doc.Title, doc.Body, s.Chunk)
	if len(parts) == 0 {
		return s.Store.DeleteByRef(ctx, it.Kind, it.RefID)
	}
	emb, err := s.embedder(ctx, doc.CommunityID)
	if err != nil {
		return fmt.Errorf("resolve embedder for %s: %w", doc.CommunityID, err)
	}
	vecs, err := emb.Embed(ctx, parts)
	if err != nil {
		return err
	}
	if len(vecs) != len(parts) {
		return fmt.Errorf("embed %s %s: %d vectors for %d chunks", it.Kind, it.RefID, len(vecs), len(parts))
	}
	stored := make([]StoredChunk, len(parts))
	for i, part := range parts {
		stored[i] = StoredChunk{
			ID:        it.Kind + ":" + it.RefID + ":" + strconv.Itoa(i),
			Content:   part,
			Embedding: vecs[i],
			Metadata: map[string]string{
				"community_id": doc.CommunityID,
				"kind":         doc.Kind,
				"ref_id":       doc.RefID,
				"title":        doc.Title,
				"created_at":   itoa64(doc.CreatedAt),
			},
		}
	}
	return s.Store.Upsert(ctx, doc.CommunityID, it.Kind, it.RefID, stored)
}

// ReindexAll drops the whole vector index and re-queues every community-public
// row. Use after switching the vector backend (e.g. chromem → qdrant). The
// worker does the actual embedding asynchronously; the returned count is the
// resulting queue depth.
func (s *Service) ReindexAll(ctx context.Context) (int, error) {
	if err := s.Store.DropAll(ctx); err != nil {
		return 0, fmt.Errorf("drop index: %w", err)
	}
	return s.Repo.EnqueueAll(ctx)
}

// ReindexCommunity drops and re-queues one community's content.
func (s *Service) ReindexCommunity(ctx context.Context, communityID string) (int, error) {
	if err := s.Store.DropCommunity(ctx, communityID); err != nil {
		return 0, fmt.Errorf("drop community index: %w", err)
	}
	return s.Repo.EnqueueCommunity(ctx, communityID)
}
