package dataexport

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
)

// Service builds an export ZIP for a community: dumps every manifest table to a
// JSON file and copies the community's media bytes. It owns no broadcast or HTTP
// concerns — the worker drives it, the handler serves the result.
type Service struct {
	Repo  *Repo
	DB    *sql.DB
	Media *uploads.Store // reads the community's upload bytes; nil = skip media
	Dir   string         // directory the .zip files live in (e.g. <uploads>/exports)
	Log   *slog.Logger
}

// Request enqueues a pending export for the community. The worker picks it up.
func (s *Service) Request(ctx context.Context, communityID, requestedBy string) (Export, error) {
	return s.Repo.Request(ctx, communityID, requestedBy)
}

// ZipPath is the absolute path of an export's artifact, or "" when the stored
// RelPath would escape the export directory. RelPath is read from the DB
// (written by Build as "<uuid>.zip"); validating containment here (FIX1 M21) is
// defense-in-depth so a corrupted or hostile rel_path like "../../etc/passwd"
// can never be handed to http.ServeFile. The caller treats "" as not-found.
func (s *Service) ZipPath(e Export) string {
	full := filepath.Join(s.Dir, e.RelPath) // Join cleans, resolving any ".."
	dir := filepath.Clean(s.Dir)
	if full != dir && !strings.HasPrefix(full, dir+string(os.PathSeparator)) {
		return ""
	}
	return full
}

// Build assembles the ZIP for a building export, then marks it ready (or failed).
// It is idempotent enough to retry: a stale .part file is overwritten. The
// finished file is named <id>.zip under Dir.
func (s *Service) Build(ctx context.Context, e Export) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return s.fail(ctx, e, fmt.Errorf("mkdir exports: %w", err))
	}
	rel := e.ID + ".zip"
	part := filepath.Join(s.Dir, e.ID+".part")
	f, err := os.Create(part)
	if err != nil {
		return s.fail(ctx, e, fmt.Errorf("create archive: %w", err))
	}
	// Best-effort cleanup of the temp file on any early return.
	defer os.Remove(part)

	zw := zip.NewWriter(f)
	if err := s.writeArchive(ctx, zw, e.CommunityID); err != nil {
		zw.Close()
		f.Close()
		return s.fail(ctx, e, err)
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return s.fail(ctx, e, fmt.Errorf("finalize archive: %w", err))
	}
	if err := f.Close(); err != nil {
		return s.fail(ctx, e, fmt.Errorf("close archive: %w", err))
	}
	if err := os.Rename(part, filepath.Join(s.Dir, rel)); err != nil {
		return s.fail(ctx, e, fmt.Errorf("place archive: %w", err))
	}
	fi, err := os.Stat(filepath.Join(s.Dir, rel))
	if err != nil {
		return s.fail(ctx, e, fmt.Errorf("stat archive: %w", err))
	}
	token, err := newToken()
	if err != nil {
		return s.fail(ctx, e, err)
	}
	if err := s.Repo.MarkReady(ctx, e.ID, token, rel, fi.Size()); err != nil {
		return fmt.Errorf("mark ready: %w", err)
	}
	return nil
}

// writeArchive dumps every manifest table + the media folder + a manifest.json.
func (s *Service) writeArchive(ctx context.Context, zw *zip.Writer, communityID string) error {
	files := make([]string, 0, len(manifest)+1)
	for _, spec := range manifest {
		rows, err := s.dumpTable(ctx, spec, communityID)
		if err != nil {
			return fmt.Errorf("dump %s: %w", spec.table, err)
		}
		if err := writeJSON(zw, spec.jsonPath(), rows); err != nil {
			return err
		}
		files = append(files, spec.jsonPath())
	}
	mediaCount, err := s.writeMedia(ctx, zw, communityID)
	if err != nil {
		return err
	}
	return writeJSON(zw, "manifest.json", map[string]any{
		"community_id": communityID,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"format":       "one JSON array per table, grouped by business function; raw files under media/",
		"ttl_days":     int(TTL / (24 * time.Hour)),
		"media_files":  mediaCount,
		"files":        files,
		"excluded": []string{
			"agent system prompts and model config (platform property)",
			"RAG vectors / embeddings (platform property)",
			"all secrets (passwords, tokens, signing secrets, API keys)",
			"sessions, verification/signup tokens, push subscriptions, read-state, OAuth identities",
			"private direct messages (involve other members)",
		},
	})
}

// dumpTable runs SELECT * scoped to the community and returns each row as a map,
// dropping secret + per-spec skipped columns. spec.table/where are internal
// constants, so the interpolation is injection-free. Every "?" binds the cid.
func (s *Service) dumpTable(ctx context.Context, spec tableSpec, communityID string) ([]map[string]any, error) {
	nph := strings.Count(spec.where, "?")
	args := make([]any, nph)
	for i := range args {
		args[i] = communityID
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT * FROM "+spec.table+" WHERE "+spec.where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	skip := make(map[string]bool, len(spec.skip))
	for _, c := range spec.skip {
		skip[c] = true
	}
	out := []map[string]any{}
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			if skip[c] || redactColumn(c) {
				continue
			}
			m[c] = normalize(cells[i])
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// writeMedia copies each of the community's upload blobs into media/. A blob
// that can't be opened is logged and skipped (a missing file shouldn't fail the
// whole export). Returns the number of files written.
func (s *Service) writeMedia(ctx context.Context, zw *zip.Writer, communityID string) (int, error) {
	if s.Media == nil {
		return 0, nil
	}
	ups, err := s.Media.ListByCommunity(ctx, communityID)
	if err != nil {
		return 0, fmt.Errorf("list media: %w", err)
	}
	n := 0
	for _, u := range ups {
		rc, err := s.Media.OpenBlob(ctx, u)
		if err != nil {
			s.logf("dataexport: open media", "upload", u.ID, "err", err)
			continue
		}
		w, err := zw.Create("media/" + mediaName(u))
		if err != nil {
			rc.Close()
			return n, err
		}
		if _, err := io.Copy(w, rc); err != nil {
			rc.Close()
			return n, fmt.Errorf("copy media %s: %w", u.ID, err)
		}
		rc.Close()
		n++
	}
	return n, nil
}

// mediaName builds a stable, collision-free archive name for an upload: the
// upload id prefixes the original filename (or the content-addressed basename).
func mediaName(u uploads.Upload) string {
	name := u.Filename
	if name == "" {
		name = filepath.Base(u.RelPath)
	}
	return u.ID + "-" + name
}

// writeJSON encodes v as indented JSON into a new ZIP entry at name.
func writeJSON(zw *zip.Writer, name string, v any) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	return nil
}

// normalize turns a driver-scanned cell into a JSON-friendly value: []byte (how
// modernc returns TEXT/BLOB) becomes a string; everything else passes through.
func normalize(v any) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}

func (s *Service) fail(ctx context.Context, e Export, cause error) error {
	s.logf("dataexport: build failed", "export", e.ID, "community", e.CommunityID, "err", cause)
	if err := s.Repo.MarkFailed(ctx, e.ID, cause.Error()); err != nil {
		return fmt.Errorf("%w (and mark-failed: %v)", cause, err)
	}
	return cause
}

func (s *Service) logf(msg string, args ...any) {
	if s.Log != nil {
		s.Log.Warn(msg, args...)
	}
}
