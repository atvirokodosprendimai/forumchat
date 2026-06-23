package uploads

import (
	"context"
	"fmt"
)

// MigrateCommunity copies a community's not-yet-migrated upload bytes from the
// default platform store to dst (the community's own bucket) and stamps each row
// store_key=StoreKeyCommunity so future reads resolve there. Idempotent: only
// store_key=” rows are moved, so a re-run resumes. Content-addressed dedup means
// each distinct rel_path is copied once (dst.Put is a no-op if present).
//
// Originals on the platform store are NOT pruned — that's a separate, after-
// verified step. A half-migrated community still reads correctly because every
// row knows its own store via store_key.
func (s *Store) MigrateCommunity(ctx context.Context, communityID string, dst Blobstore) (migrated int, err error) {
	if dst == nil {
		return 0, fmt.Errorf("uploads: nil destination store")
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, rel_path FROM uploads
		WHERE community_id = ? AND COALESCE(store_key,'') = ''`, communityID)
	if err != nil {
		return 0, err
	}
	type row struct{ id, rel string }
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.rel); err != nil {
			rows.Close()
			return migrated, err
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return migrated, err
	}

	src := s.blob()
	for _, r := range pending {
		if ctx.Err() != nil {
			return migrated, ctx.Err()
		}
		rc, err := src.Open(ctx, r.rel)
		if err != nil {
			// Missing source bytes (already pruned / never written) — stamp it
			// anyway so the row isn't retried forever; the read will 404 cleanly.
			_, _ = s.DB.ExecContext(ctx, `UPDATE uploads SET store_key = ? WHERE id = ?`, StoreKeyCommunity, r.id)
			continue
		}
		putErr := dst.Put(ctx, r.rel, rc)
		rc.Close()
		if putErr != nil {
			return migrated, fmt.Errorf("copy %s: %w", r.rel, putErr)
		}
		if _, err := s.DB.ExecContext(ctx, `UPDATE uploads SET store_key = ? WHERE id = ?`, StoreKeyCommunity, r.id); err != nil {
			return migrated, err
		}
		migrated++
	}
	return migrated, nil
}
