package uploads

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrTooLarge   = errors.New("uploads: too large")
	ErrBadMIME    = errors.New("uploads: unsupported MIME type")
	ErrNotFound   = errors.New("uploads: not found")
	ErrBadSig     = errors.New("uploads: bad signature")
	ErrCrossComm  = errors.New("uploads: cross-community access denied")
)

var allowedMIME = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

type Upload struct {
	ID          string
	OwnerID     string
	CommunityID string
	SHA256      string
	MIME        string
	Size        int64
	RelPath     string
	CreatedAt   time.Time
}

type Store struct {
	DB      *sql.DB
	Dir     string
	MaxSize int64
	SignKey []byte
}

func NewStore(db *sql.DB, dir string, max int64, signKey string) *Store {
	return &Store{DB: db, Dir: dir, MaxSize: max, SignKey: []byte(signKey)}
}

func (s *Store) Save(ctx context.Context, ownerID, communityID, mime string, r io.Reader) (Upload, error) {
	ext, ok := allowedMIME[mime]
	if !ok {
		return Upload{}, ErrBadMIME
	}
	tmp, err := os.CreateTemp(s.Dir, "up-*.tmp")
	if err != nil {
		if err := os.MkdirAll(s.Dir, 0o755); err != nil {
			return Upload{}, err
		}
		tmp, err = os.CreateTemp(s.Dir, "up-*.tmp")
		if err != nil {
			return Upload{}, err
		}
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	h := sha256.New()
	mw := io.MultiWriter(tmp, h)
	n, err := io.CopyN(mw, r, s.MaxSize+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return Upload{}, fmt.Errorf("copy: %w", err)
	}
	if n > s.MaxSize {
		return Upload{}, ErrTooLarge
	}
	digest := hex.EncodeToString(h.Sum(nil))
	rel := filepath.Join(digest[:2], digest+ext)
	dst := filepath.Join(s.Dir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return Upload{}, err
	}
	if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(tmp.Name(), dst); err != nil {
			return Upload{}, err
		}
	}

	u := Upload{
		ID: uuid.NewString(), OwnerID: ownerID, CommunityID: communityID,
		SHA256: digest, MIME: mime, Size: n, RelPath: rel, CreatedAt: time.Now(),
	}
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO uploads (id, owner_id, community_id, sha256, mime, size, rel_path, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.OwnerID, u.CommunityID, u.SHA256, u.MIME, u.Size, u.RelPath, u.CreatedAt.Unix()); err != nil {
		return Upload{}, err
	}
	return u, nil
}

func (s *Store) Get(ctx context.Context, id string) (Upload, error) {
	var u Upload
	var created int64
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, owner_id, community_id, sha256, mime, size, rel_path, created_at
		FROM uploads WHERE id = ?`, id).
		Scan(&u.ID, &u.OwnerID, &u.CommunityID, &u.SHA256, &u.MIME, &u.Size, &u.RelPath, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Upload{}, ErrNotFound
	}
	if err != nil {
		return Upload{}, err
	}
	u.CreatedAt = time.Unix(created, 0)
	return u, nil
}

func (s *Store) Sign(id, viewerID string, exp time.Time) string {
	mac := hmac.New(sha256.New, s.SignKey)
	mac.Write([]byte(id))
	mac.Write([]byte{0})
	mac.Write([]byte(viewerID))
	mac.Write([]byte{0})
	mac.Write([]byte(strconv.FormatInt(exp.Unix(), 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Store) Verify(id, viewerID, sig string, expUnix int64) error {
	if time.Now().Unix() > expUnix {
		return ErrBadSig
	}
	expected := s.Sign(id, viewerID, time.Unix(expUnix, 0))
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return ErrBadSig
	}
	return nil
}

// SignedURL builds a relative URL for the given viewer with a TTL.
func (s *Store) SignedURL(id, viewerID string, ttl time.Duration) string {
	exp := time.Now().Add(ttl)
	sig := s.Sign(id, viewerID, exp)
	return fmt.Sprintf("/uploads/%s?exp=%d&sig=%s", id, exp.Unix(), sig)
}

// PathFor returns the absolute filesystem path for the upload.
func (s *Store) PathFor(u Upload) string {
	return filepath.Join(s.Dir, u.RelPath)
}

// ExtForMIME returns the canonical extension for an allowed MIME, or "" if not.
func ExtForMIME(mime string) string {
	return allowedMIME[strings.ToLower(mime)]
}

// Delete removes the upload row and, if no other row still references the
// underlying file (same content hash → same rel_path), deletes the file too.
// Missing rows / missing files are not an error.
func (s *Store) Delete(ctx context.Context, id string) error {
	u, err := s.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM uploads WHERE id = ?`, id); err != nil {
		return err
	}
	var cnt int
	_ = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM uploads WHERE rel_path = ?`, u.RelPath).Scan(&cnt)
	if cnt == 0 {
		_ = os.Remove(filepath.Join(s.Dir, u.RelPath))
	}
	return nil
}

// SaveDataURL decodes a "data:<mime>;base64,XXXX" string, enforces maxBytes
// on the decoded payload, and persists it via Save. Used by the paste-image
// path on chat and forum forms.
func (s *Store) SaveDataURL(ctx context.Context, ownerID, communityID, dataURL string, maxBytes int64) (Upload, error) {
	if !strings.HasPrefix(dataURL, "data:") {
		return Upload{}, errors.New("uploads: not a data URL")
	}
	comma := strings.IndexByte(dataURL, ',')
	if comma < 0 {
		return Upload{}, errors.New("uploads: bad data URL")
	}
	header := dataURL[5:comma]
	payload := dataURL[comma+1:]
	parts := strings.Split(header, ";")
	if len(parts) < 2 || parts[len(parts)-1] != "base64" {
		return Upload{}, errors.New("uploads: only base64 data URLs supported")
	}
	mime := strings.ToLower(parts[0])
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return Upload{}, fmt.Errorf("uploads: decode base64: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return Upload{}, ErrTooLarge
	}
	return s.Save(ctx, ownerID, communityID, mime, bytes.NewReader(data))
}

// SaveAttachment persists a file under the uploads table WITHOUT the
// image-only MIME whitelist that Save enforces. Used by feature areas
// (e.g. projects) that need to accept arbitrary documents — PDFs,
// spreadsheets, archives. The original filename is used to derive an
// on-disk extension; the persisted MIME is honored when streaming back
// so the browser gets the right Content-Type.
func (s *Store) SaveAttachment(ctx context.Context, ownerID, communityID, mime, filename string, r io.Reader) (Upload, error) {
	ext := filepath.Ext(filename)
	if ext == "" {
		if e, ok := allowedMIME[mime]; ok {
			ext = e
		} else {
			ext = ".bin"
		}
	}
	tmp, err := os.CreateTemp(s.Dir, "att-*.tmp")
	if err != nil {
		if err := os.MkdirAll(s.Dir, 0o755); err != nil {
			return Upload{}, err
		}
		tmp, err = os.CreateTemp(s.Dir, "att-*.tmp")
		if err != nil {
			return Upload{}, err
		}
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	h := sha256.New()
	mw := io.MultiWriter(tmp, h)
	n, err := io.CopyN(mw, r, s.MaxSize+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return Upload{}, fmt.Errorf("copy: %w", err)
	}
	if n > s.MaxSize {
		return Upload{}, ErrTooLarge
	}
	digest := hex.EncodeToString(h.Sum(nil))
	rel := filepath.Join(digest[:2], digest+ext)
	dst := filepath.Join(s.Dir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return Upload{}, err
	}
	if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(tmp.Name(), dst); err != nil {
			return Upload{}, err
		}
	}

	u := Upload{
		ID: uuid.NewString(), OwnerID: ownerID, CommunityID: communityID,
		SHA256: digest, MIME: mime, Size: n, RelPath: rel, CreatedAt: time.Now(),
	}
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO uploads (id, owner_id, community_id, sha256, mime, size, rel_path, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.OwnerID, u.CommunityID, u.SHA256, u.MIME, u.Size, u.RelPath, u.CreatedAt.Unix()); err != nil {
		return Upload{}, err
	}
	return u, nil
}

// MIMEFromHeader picks the best MIME from a multipart Content-Type, falling back
// to a sniffed type from the leading bytes. Caller passes the first N bytes.
func MIMEFromHeader(declared string, sniff []byte) string {
	declared = strings.ToLower(strings.TrimSpace(declared))
	if _, ok := allowedMIME[declared]; ok {
		return declared
	}
	return http.DetectContentType(sniff)
}
