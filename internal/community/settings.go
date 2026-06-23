package community

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/secretbox"
)

// Settings is one community's tenant configuration (the community_settings row,
// migration 00055). Every field is "unset by default": *bool nil and ""/0 mean
// "fall back to the platform env default" — see resolve.go. Secret fields hold
// PLAINTEXT in memory; SaveSettings seals them and Settings opens them via the
// repo's secretbox.
type Settings struct {
	CommunityID string

	AIEnabled *bool

	RAGEnabled      *bool
	RAGEmbedBaseURL string
	RAGEmbedModel   string
	RAGEmbedDim     int
	RAGQdrantURL    string
	RAGQdrantAPIKey string // sealed at rest
	RAGQdrantColl   string

	TranslateEnabled *bool
	TranslateBaseURL string
	TranslateModel   string

	StorageBackend    string // "" | disk | s3
	S3Endpoint        string
	S3Region          string
	S3Bucket          string
	S3AccessKey       string // sealed at rest
	S3SecretKey       string // sealed at rest
	StorageMigratedAt int64

	JoinPolicy string // "" | open | request
}

// box returns the repo's secretbox, or a passthrough box when none is wired
// (dev / tests without SECRETS_KEY).
func (r *Repo) box() *secretbox.Box {
	if r.Secrets != nil {
		return r.Secrets
	}
	return &secretbox.Box{}
}

// Settings loads a community's tenant settings. A missing row is not an error —
// it returns a zero Settings (everything unset, resolver falls back to env).
func (r *Repo) Settings(ctx context.Context, communityID string) (Settings, error) {
	s := Settings{CommunityID: communityID}
	var (
		aiEnabled, ragEnabled, trEnabled sql.NullInt64
		ragDim, migAt                    sql.NullInt64
		ragBase, ragModel, ragQURL       sql.NullString
		ragQKeyEnc, ragQColl             sql.NullString
		trBase, trModel                  sql.NullString
		stBackend, s3Endpoint, s3Region  sql.NullString
		s3Bucket, s3AccEnc, s3SecEnc     sql.NullString
		joinPolicy                       sql.NullString
	)
	err := r.DB.QueryRowContext(ctx, `
		SELECT ai_enabled,
		       rag_enabled, rag_embed_base_url, rag_embed_model, rag_embed_dim,
		       rag_qdrant_url, rag_qdrant_api_key_enc, rag_qdrant_collection,
		       translate_enabled, translate_base_url, translate_model,
		       storage_backend, storage_s3_endpoint, storage_s3_region,
		       storage_s3_bucket, storage_s3_access_key_enc, storage_s3_secret_key_enc,
		       storage_migrated_at, join_policy
		FROM community_settings WHERE community_id = ?`, communityID).
		Scan(&aiEnabled,
			&ragEnabled, &ragBase, &ragModel, &ragDim,
			&ragQURL, &ragQKeyEnc, &ragQColl,
			&trEnabled, &trBase, &trModel,
			&stBackend, &s3Endpoint, &s3Region,
			&s3Bucket, &s3AccEnc, &s3SecEnc,
			&migAt, &joinPolicy)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return Settings{}, err
	}

	s.AIEnabled = nullBool(aiEnabled)
	s.RAGEnabled = nullBool(ragEnabled)
	s.RAGEmbedBaseURL = ragBase.String
	s.RAGEmbedModel = ragModel.String
	s.RAGEmbedDim = int(ragDim.Int64)
	s.RAGQdrantURL = ragQURL.String
	s.RAGQdrantColl = ragQColl.String
	s.TranslateEnabled = nullBool(trEnabled)
	s.TranslateBaseURL = trBase.String
	s.TranslateModel = trModel.String
	s.StorageBackend = stBackend.String
	s.S3Endpoint = s3Endpoint.String
	s.S3Region = s3Region.String
	s.S3Bucket = s3Bucket.String
	s.StorageMigratedAt = migAt.Int64
	s.JoinPolicy = joinPolicy.String

	// Tolerate decrypt failures (e.g. a rotated SECRETS_KEY orphans old
	// ciphertext): leave the secret empty rather than failing the whole load, so
	// the owner Settings page still renders and the secret can be re-entered.
	// Open returns "" on error, so discarding it yields an empty field.
	box := r.box()
	s.RAGQdrantAPIKey, _ = box.Open(ragQKeyEnc.String)
	s.S3AccessKey, _ = box.Open(s3AccEnc.String)
	s.S3SecretKey, _ = box.Open(s3SecEnc.String)
	return s, nil
}

// SaveSettings upserts a community's settings, sealing secret fields at rest.
func (r *Repo) SaveSettings(ctx context.Context, s Settings) error {
	box := r.box()
	qKey, err := box.Seal(s.RAGQdrantAPIKey)
	if err != nil {
		return err
	}
	s3Acc, err := box.Seal(s.S3AccessKey)
	if err != nil {
		return err
	}
	s3Sec, err := box.Seal(s.S3SecretKey)
	if err != nil {
		return err
	}
	_, err = r.DB.ExecContext(ctx, `
		INSERT INTO community_settings (
			community_id, ai_enabled,
			rag_enabled, rag_embed_base_url, rag_embed_model, rag_embed_dim,
			rag_qdrant_url, rag_qdrant_api_key_enc, rag_qdrant_collection,
			translate_enabled, translate_base_url, translate_model,
			storage_backend, storage_s3_endpoint, storage_s3_region,
			storage_s3_bucket, storage_s3_access_key_enc, storage_s3_secret_key_enc,
			storage_migrated_at, join_policy, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(community_id) DO UPDATE SET
			ai_enabled=excluded.ai_enabled,
			rag_enabled=excluded.rag_enabled,
			rag_embed_base_url=excluded.rag_embed_base_url,
			rag_embed_model=excluded.rag_embed_model,
			rag_embed_dim=excluded.rag_embed_dim,
			rag_qdrant_url=excluded.rag_qdrant_url,
			rag_qdrant_api_key_enc=excluded.rag_qdrant_api_key_enc,
			rag_qdrant_collection=excluded.rag_qdrant_collection,
			translate_enabled=excluded.translate_enabled,
			translate_base_url=excluded.translate_base_url,
			translate_model=excluded.translate_model,
			storage_backend=excluded.storage_backend,
			storage_s3_endpoint=excluded.storage_s3_endpoint,
			storage_s3_region=excluded.storage_s3_region,
			storage_s3_bucket=excluded.storage_s3_bucket,
			storage_s3_access_key_enc=excluded.storage_s3_access_key_enc,
			storage_s3_secret_key_enc=excluded.storage_s3_secret_key_enc,
			storage_migrated_at=excluded.storage_migrated_at,
			join_policy=excluded.join_policy,
			updated_at=excluded.updated_at`,
		s.CommunityID, boolToNull(s.AIEnabled),
		boolToNull(s.RAGEnabled), strToNull(s.RAGEmbedBaseURL), strToNull(s.RAGEmbedModel), intToNull(s.RAGEmbedDim),
		strToNull(s.RAGQdrantURL), strToNull(qKey), strToNull(s.RAGQdrantColl),
		boolToNull(s.TranslateEnabled), strToNull(s.TranslateBaseURL), strToNull(s.TranslateModel),
		strToNull(s.StorageBackend), strToNull(s.S3Endpoint), strToNull(s.S3Region),
		strToNull(s.S3Bucket), strToNull(s3Acc), strToNull(s3Sec),
		intToNull64(s.StorageMigratedAt), strToNull(s.JoinPolicy), time.Now().Unix())
	return err
}

func nullBool(n sql.NullInt64) *bool {
	if !n.Valid {
		return nil
	}
	v := n.Int64 != 0
	return &v
}

func boolToNull(p *bool) any {
	if p == nil {
		return nil
	}
	if *p {
		return 1
	}
	return 0
}

func strToNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func intToNull(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func intToNull64(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
