package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// QdrantStore is a rag.Store backed by Qdrant over its REST API (no client
// dependency — same rationale as the Ollama embedder). Each community gets its
// OWN collection, created on demand and sized to the community's embedding model
// (the dimension is read from the first vector, so a model with a different
// vector size just makes a different-sized collection). This is the SaaS path;
// chromem stays the single-tenant backend.
//
// Per-community isolation: Upsert/Query/DropCommunity carry the community id and
// route to forumchat_<communityID>. The standalone DeleteByRef (an outbox delete
// whose source row is already gone, so the community can't be re-read) has no
// community id, so it deletes by payload filter across the platform Qdrant's
// forumchat_* collections.
type QdrantStore struct {
	// Resolve maps a community to its Qdrant connection. Optional — when nil
	// every community uses Default with collection forumchat_<id>. Lets a
	// community BYO its own Qdrant URL/key (falling back to Default for blanks).
	Resolve func(ctx context.Context, communityID string) QdrantConn
	Default QdrantConn
	HTTP    *http.Client
	Log     *slog.Logger

	mu      sync.Mutex
	ensured map[string]bool // "url\x00collection" already created at some dim
}

// QdrantConn locates one community's vectors.
type QdrantConn struct {
	URL        string // e.g. http://localhost:6333
	APIKey     string
	Collection string // forumchat_<communityID>
}

// pointNamespace makes chunk-id → UUID deterministic so re-upserting the same
// chunk is idempotent (Qdrant point ids must be uint or UUID).
var pointNamespace = uuid.MustParse("6f9619ff-8b86-d011-b42d-00cf4fc964ff")

// NewQdrantStore builds a Qdrant-backed Store. defaultURL/apiKey are the platform
// Qdrant used for communities that didn't BYO one (and for DropAll).
func NewQdrantStore(defaultURL, apiKey string, log *slog.Logger) *QdrantStore {
	return &QdrantStore{
		Default: QdrantConn{URL: strings.TrimRight(defaultURL, "/"), APIKey: apiKey},
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		Log:     log,
		ensured: map[string]bool{},
	}
}

func (q *QdrantStore) conn(ctx context.Context, communityID string) QdrantConn {
	c := QdrantConn{URL: q.Default.URL, APIKey: q.Default.APIKey, Collection: collectionName(communityID)}
	if q.Resolve != nil {
		r := q.Resolve(ctx, communityID)
		if r.URL != "" {
			c.URL = strings.TrimRight(r.URL, "/")
		}
		if r.APIKey != "" {
			c.APIKey = r.APIKey
		}
		if r.Collection != "" {
			c.Collection = r.Collection
		}
	}
	return c
}

// collectionName derives a Qdrant-safe collection name for a community.
func collectionName(communityID string) string {
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, communityID)
	return "forumchat_" + safe
}

// --- REST plumbing -----------------------------------------------------------

func (q *QdrantStore) do(ctx context.Context, conn QdrantConn, method, path string, body any, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, conn.URL+path, rdr)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if conn.APIKey != "" {
		req.Header.Set("api-key", conn.APIKey)
	}
	resp, err := q.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return resp.StatusCode, fmt.Errorf("qdrant %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(buf)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// ensureCollection creates conn.Collection sized to dim if it doesn't exist.
func (q *QdrantStore) ensureCollection(ctx context.Context, conn QdrantConn, dim int) error {
	cacheKey := conn.URL + "\x00" + conn.Collection
	q.mu.Lock()
	done := q.ensured[cacheKey]
	q.mu.Unlock()
	if done {
		return nil
	}
	// Exists?
	if code, _ := q.do(ctx, conn, http.MethodGet, "/collections/"+conn.Collection, nil, nil); code == http.StatusOK {
		q.markEnsured(cacheKey)
		return nil
	}
	body := map[string]any{"vectors": map[string]any{"size": dim, "distance": "Cosine"}}
	if _, err := q.do(ctx, conn, http.MethodPut, "/collections/"+conn.Collection, body, nil); err != nil {
		return err
	}
	q.markEnsured(cacheKey)
	return nil
}

func (q *QdrantStore) markEnsured(k string) {
	q.mu.Lock()
	q.ensured[k] = true
	q.mu.Unlock()
}

// --- Store interface ---------------------------------------------------------

func (q *QdrantStore) Upsert(ctx context.Context, communityID, kind, refID string, chunks []StoredChunk) error {
	conn := q.conn(ctx, communityID)
	// Replace semantics: drop the row's existing points first.
	if err := q.deleteByRefInCollection(ctx, conn, kind, refID); err != nil {
		return err
	}
	if len(chunks) == 0 {
		return nil
	}
	if err := q.ensureCollection(ctx, conn, len(chunks[0].Embedding)); err != nil {
		return err
	}
	points := make([]map[string]any, len(chunks))
	for i, c := range chunks {
		payload := map[string]any{"content": c.Content}
		for k, v := range c.Metadata {
			payload[k] = v
		}
		points[i] = map[string]any{
			"id":      uuid.NewSHA1(pointNamespace, []byte(c.ID)).String(),
			"vector":  c.Embedding,
			"payload": payload,
		}
	}
	_, err := q.do(ctx, conn, http.MethodPut, "/collections/"+conn.Collection+"/points?wait=true",
		map[string]any{"points": points}, nil)
	return err
}

func (q *QdrantStore) deleteByRefInCollection(ctx context.Context, conn QdrantConn, kind, refID string) error {
	filter := map[string]any{"filter": map[string]any{"must": []any{
		map[string]any{"key": "kind", "match": map[string]any{"value": kind}},
		map[string]any{"key": "ref_id", "match": map[string]any{"value": refID}},
	}}}
	code, err := q.do(ctx, conn, http.MethodPost, "/collections/"+conn.Collection+"/points/delete?wait=true", filter, nil)
	if code == http.StatusNotFound {
		return nil // collection not created yet — nothing to delete
	}
	return err
}

func (q *QdrantStore) DeleteByRef(ctx context.Context, kind, refID string) error {
	// No community id (outbox delete of an already-gone row): delete by payload
	// filter across every forumchat_* collection on the platform Qdrant.
	cols, err := q.listCollections(ctx, q.Default)
	if err != nil {
		return err
	}
	for _, col := range cols {
		if err := q.deleteByRefInCollection(ctx, QdrantConn{URL: q.Default.URL, APIKey: q.Default.APIKey, Collection: col}, kind, refID); err != nil {
			return err
		}
	}
	return nil
}

func (q *QdrantStore) Query(ctx context.Context, communityID string, embedding []float32, limit int) ([]Hit, error) {
	conn := q.conn(ctx, communityID)
	body := map[string]any{"vector": embedding, "limit": limit, "with_payload": true}
	var out struct {
		Result []struct {
			Score   float32           `json:"score"`
			Payload map[string]string `json:"payload"`
		} `json:"result"`
	}
	code, err := q.do(ctx, conn, http.MethodPost, "/collections/"+conn.Collection+"/points/search", body, &out)
	if code == http.StatusNotFound {
		return nil, nil // community has no collection yet
	}
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(out.Result))
	for _, r := range out.Result {
		hits = append(hits, Hit{
			Kind:      r.Payload["kind"],
			RefID:     r.Payload["ref_id"],
			Title:     r.Payload["title"],
			Snippet:   snippet(r.Payload["content"], 240),
			CreatedAt: atoi64(r.Payload["created_at"]),
			Score:     r.Score,
		})
	}
	return hits, nil
}

func (q *QdrantStore) DropCommunity(ctx context.Context, communityID string) error {
	conn := q.conn(ctx, communityID)
	q.mu.Lock()
	delete(q.ensured, conn.URL+"\x00"+conn.Collection)
	q.mu.Unlock()
	code, err := q.do(ctx, conn, http.MethodDelete, "/collections/"+conn.Collection, nil, nil)
	if code == http.StatusNotFound {
		return nil
	}
	return err
}

func (q *QdrantStore) DropAll(ctx context.Context) error {
	cols, err := q.listCollections(ctx, q.Default)
	if err != nil {
		return err
	}
	q.mu.Lock()
	q.ensured = map[string]bool{}
	q.mu.Unlock()
	for _, col := range cols {
		if _, err := q.do(ctx, q.Default, http.MethodDelete, "/collections/"+col, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

func (q *QdrantStore) Close() error { return nil }

// listCollections returns the forumchat_* collection names on conn.
func (q *QdrantStore) listCollections(ctx context.Context, conn QdrantConn) ([]string, error) {
	var out struct {
		Result struct {
			Collections []struct {
				Name string `json:"name"`
			} `json:"collections"`
		} `json:"result"`
	}
	if _, err := q.do(ctx, conn, http.MethodGet, "/collections", nil, &out); err != nil {
		return nil, err
	}
	var names []string
	for _, c := range out.Result.Collections {
		if strings.HasPrefix(c.Name, "forumchat_") {
			names = append(names, c.Name)
		}
	}
	return names, nil
}

func snippet(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}
