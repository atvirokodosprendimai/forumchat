package uploads

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Blobstore is the pluggable byte backend behind uploads.Store. The Store keeps
// all the DB metadata, signing, MIME sniffing and content-addressed dedup; only
// the raw bytes move through here. Keys are the content-addressed rel_path
// (`<sha[:2]>/<sha><ext>`) the Store already computes — so dedup is preserved
// and switching backends needs no key rewrite.
type Blobstore interface {
	// Put stores bytes under key. It is idempotent for content-addressed keys:
	// if key already exists it is a no-op (same content → same key).
	Put(ctx context.Context, key string, r io.Reader) error
	// Open returns a reader for key's bytes. Caller closes it.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	// Remove deletes key. A missing key is not an error.
	Remove(ctx context.Context, key string) error
	// Exists reports whether key is present.
	Exists(ctx context.Context, key string) (bool, error)
	// LocalPath returns a filesystem path for disk-backed stores (ok=true) so
	// the handler can http.ServeFile (HTTP Range / video seeking). Remote
	// stores return ("", false) and the handler streams via Open.
	LocalPath(key string) (string, bool)
}

// diskBlobs is the local-filesystem Blobstore — today's behaviour, extracted.
type diskBlobs struct{ dir string }

// NewDiskBlobstore returns a Blobstore rooted at dir.
func NewDiskBlobstore(dir string) Blobstore { return diskBlobs{dir: dir} }

func (d diskBlobs) path(key string) string { return filepath.Join(d.dir, key) }

func (d diskBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	dst := d.path(key)
	if _, err := os.Stat(dst); err == nil {
		return nil // already present — content-addressed, identical bytes
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// Write to a sibling temp then rename for atomic placement.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".put-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

func (d diskBlobs) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	return os.Open(d.path(key))
}

func (d diskBlobs) Remove(ctx context.Context, key string) error {
	err := os.Remove(d.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (d diskBlobs) Exists(ctx context.Context, key string) (bool, error) {
	_, err := os.Stat(d.path(key))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (d diskBlobs) LocalPath(key string) (string, bool) { return d.path(key), true }
