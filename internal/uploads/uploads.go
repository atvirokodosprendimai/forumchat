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

// allowedMIME is the legacy image-only extension map. Still used by
// ExtForMIME for paste / data-URL paths that mint a sha-named file
// and need a canonical extension when the original filename is empty.
// Adding rich-media MIMEs here is welcome — they all get the same
// fallback ext lookup.
var allowedMIME = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
	"video/mp4":  ".mp4",
	"video/webm": ".webm",
	"video/quicktime": ".mov",
	"audio/mpeg": ".mp3",
	"audio/mp4":  ".m4a",
	"audio/wav":  ".wav",
	"audio/ogg":  ".ogg",
	"application/pdf": ".pdf",
}

// denyMIME is the executable / script denylist. Any MIME in this set
// is rejected at Save time regardless of how it was sniffed or
// declared. Adding to this list shrinks what users can upload; it
// should never be expanded to "everything that isn't an image" — that
// was the bug Phase 1 of the chat-attachments plan fixed.
var denyMIME = map[string]struct{}{
	"application/x-msdownload":     {},
	"application/x-msdos-program":  {},
	"application/x-dosexec":        {},
	"application/x-mach-binary":    {},
	"application/x-executable":     {},
	"application/x-sh":             {},
	"application/x-bsh":            {},
	"application/x-csh":            {},
	"application/x-shellscript":    {},
	"text/x-shellscript":           {},
	"application/x-perl":           {},
	"application/x-python":         {},
	"application/x-python-code":    {},
	"application/x-php":            {},
	"application/x-httpd-php":      {},
	"application/x-bat":            {},
}

// isAllowedMIME returns true when the MIME (lower-cased) is not on
// the denylist. Empty MIMEs default to true and let the sniffer + DB
// owner take the call — Save() always sniffs and re-checks.
func isAllowedMIME(mime string) bool {
	if mime == "" {
		return true
	}
	_, denied := denyMIME[strings.ToLower(mime)]
	return !denied
}

// sniffMIME is a thin wrapper over http.DetectContentType that also
// catches executable signatures Go's sniffer doesn't recognise
// (Windows PE/EXE, ELF, Mach-O). Those map onto denylisted MIMEs so
// the deny check downstream rejects them, regardless of what the
// client declared.
func sniffMIME(head []byte) string {
	if len(head) >= 2 && head[0] == 'M' && head[1] == 'Z' {
		return "application/x-msdownload"
	}
	if len(head) >= 4 && head[0] == 0x7f && head[1] == 'E' && head[2] == 'L' && head[3] == 'F' {
		return "application/x-executable"
	}
	// Mach-O 32 / 64 / fat (big & little endian) magic bytes.
	if len(head) >= 4 {
		m := uint32(head[0])<<24 | uint32(head[1])<<16 | uint32(head[2])<<8 | uint32(head[3])
		switch m {
		case 0xFEEDFACE, 0xFEEDFACF, 0xCEFAEDFE, 0xCFFAEDFE, 0xCAFEBABE, 0xBEBAFECA:
			return "application/x-mach-binary"
		}
	}
	// Shebang scripts — anything starting with `#!`.
	if len(head) >= 2 && head[0] == '#' && head[1] == '!' {
		return "application/x-shellscript"
	}
	return http.DetectContentType(head)
}

// sanitiseFilename trims path components, control bytes, and trailing
// whitespace so a hostile filename can't escape its bucket or break
// HTTP headers. Empty input → empty output (no synthetic name).
func sanitiseFilename(name string) string {
	name = filepath.Base(name)
	if name == "." || name == string(filepath.Separator) {
		return ""
	}
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			continue
		}
		if r == '/' || r == '\\' || r == 0 {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

type Upload struct {
	ID          string
	OwnerID     string
	CommunityID string
	SHA256      string
	MIME        string
	Size        int64
	RelPath     string
	Filename    string
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

// Save persists a single upload. mime is the caller-declared MIME
// (e.g. from a multipart Content-Type or an inferred data: URL prefix);
// the first 512 bytes are sniffed and the sniff wins over the
// declared value when it disagrees. The denylist is consulted on the
// final MIME; matches → ErrBadMIME. filename is the user-supplied
// display name (empty for paste / data-URL paths) and is sanitised
// before storage.
func (s *Store) Save(ctx context.Context, ownerID, communityID, mime, filename string, r io.Reader) (Upload, error) {
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
	// Snip the leading 512 bytes for the MIME sniff while streaming
	// to disk + hash. We can't seek the reader, so we hold the first
	// chunk in memory and re-emit it via a multi-source reader.
	sniffBuf := &bytes.Buffer{}
	headLimit := io.LimitReader(r, 512)
	if _, err := io.Copy(sniffBuf, headLimit); err != nil && !errors.Is(err, io.EOF) {
		return Upload{}, fmt.Errorf("sniff: %w", err)
	}
	sniffed := sniffMIME(sniffBuf.Bytes())
	declared := strings.ToLower(strings.TrimSpace(mime))
	// Prefer the sniff unless the declared MIME matches the
	// sniff-family (e.g. both image/* or both video/*). This lets a
	// generic application/octet-stream from a browser pick up a
	// real PDF / MP4 / etc.
	finalMIME := sniffed
	if declared != "" && declared != "application/octet-stream" {
		if sniffed == "application/octet-stream" || mimeFamily(declared) == mimeFamily(sniffed) {
			finalMIME = declared
		}
	}
	if !isAllowedMIME(finalMIME) {
		return Upload{}, ErrBadMIME
	}

	mw := io.MultiWriter(tmp, h)
	if _, err := mw.Write(sniffBuf.Bytes()); err != nil {
		return Upload{}, fmt.Errorf("write sniff: %w", err)
	}
	headBytes := int64(sniffBuf.Len())
	// MaxSize is the BODY cap; allow one more byte to trip ErrTooLarge.
	remaining := s.MaxSize + 1 - headBytes
	if remaining < 0 {
		remaining = 0
	}
	tail, err := io.CopyN(mw, r, remaining)
	if err != nil && !errors.Is(err, io.EOF) {
		return Upload{}, fmt.Errorf("copy: %w", err)
	}
	n := headBytes + tail
	if n > s.MaxSize {
		return Upload{}, ErrTooLarge
	}

	digest := hex.EncodeToString(h.Sum(nil))
	ext := extFor(finalMIME, filename)
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

	cleanName := sanitiseFilename(filename)
	u := Upload{
		ID: uuid.NewString(), OwnerID: ownerID, CommunityID: communityID,
		SHA256: digest, MIME: finalMIME, Size: n, RelPath: rel,
		Filename: cleanName, CreatedAt: time.Now(),
	}
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO uploads (id, owner_id, community_id, sha256, mime, size, rel_path, filename, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.OwnerID, u.CommunityID, u.SHA256, u.MIME, u.Size, u.RelPath, u.Filename, u.CreatedAt.Unix()); err != nil {
		return Upload{}, err
	}
	return u, nil
}

// mimeFamily returns the slash-prefix of a MIME (or the whole value
// if no slash). Used to compare declared vs sniffed for the "agree
// enough to trust the declared" check.
func mimeFamily(m string) string {
	if i := strings.IndexByte(m, '/'); i > 0 {
		return m[:i]
	}
	return m
}

// extFor picks a canonical extension. Filename's extension wins when
// non-empty + ≤ 10 bytes, so PDFs come back as .pdf even when the
// MIME is octet-stream. Falls back to the allowedMIME map, then ".bin".
func extFor(mime, filename string) string {
	if filename != "" {
		ext := strings.ToLower(filepath.Ext(filename))
		if ext != "" && len(ext) <= 10 {
			return ext
		}
	}
	if e, ok := allowedMIME[strings.ToLower(mime)]; ok {
		return e
	}
	return ".bin"
}

func (s *Store) Get(ctx context.Context, id string) (Upload, error) {
	var u Upload
	var created int64
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, owner_id, community_id, sha256, mime, size, rel_path, filename, created_at
		FROM uploads WHERE id = ?`, id).
		Scan(&u.ID, &u.OwnerID, &u.CommunityID, &u.SHA256, &u.MIME, &u.Size, &u.RelPath, &u.Filename, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Upload{}, ErrNotFound
	}
	if err != nil {
		return Upload{}, err
	}
	u.CreatedAt = time.Unix(created, 0)
	return u, nil
}

// Sign computes the HMAC over (upload_id || viewer_id || exp). Used for
// per-viewer URLs. The shared variant below omits viewer_id so any
// authenticated viewer can verify the signature.
func (s *Store) Sign(id, viewerID string, exp time.Time) string {
	mac := hmac.New(sha256.New, s.SignKey)
	mac.Write([]byte(id))
	mac.Write([]byte{0})
	mac.Write([]byte(viewerID))
	mac.Write([]byte{0})
	mac.Write([]byte(strconv.FormatInt(exp.Unix(), 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

// SignShared computes the HMAC over (upload_id || exp). The resulting
// URL is viewable by any caller that GetFile admits — i.e. any auth
// user or any active share-link guest. This is what we want for
// chat / forum / discussion image embeds where every member should be
// able to see the same image.
func (s *Store) SignShared(id string, exp time.Time) string {
	mac := hmac.New(sha256.New, s.SignKey)
	mac.Write([]byte(id))
	mac.Write([]byte{0})
	mac.Write([]byte(strconv.FormatInt(exp.Unix(), 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify accepts either the new shared format (preferred) or the
// legacy per-viewer format (still in existing URLs). The handler
// passes its current viewerID so legacy URLs continue to work for
// the originally-signed viewer until they expire.
func (s *Store) Verify(id, viewerID, sig string, expUnix int64) error {
	if time.Now().Unix() > expUnix {
		return ErrBadSig
	}
	expShared := s.SignShared(id, time.Unix(expUnix, 0))
	if hmac.Equal([]byte(expShared), []byte(sig)) {
		return nil
	}
	expPer := s.Sign(id, viewerID, time.Unix(expUnix, 0))
	if hmac.Equal([]byte(expPer), []byte(sig)) {
		return nil
	}
	return ErrBadSig
}

// SignedURL builds a relative URL that any authenticated viewer can
// load. viewerID is intentionally unused — kept in the signature for
// backward compatibility with existing call sites.
//
// The expiry is bucketed (see stableExpiry) so that re-signing the same
// upload within a window yields a byte-identical URL. This matters for
// the chat fat-morph: #messages is re-rendered on every event, and a
// fresh exp/sig on each render would change every <img src>, making
// idiomorph swap the node and the browser re-download every image. A
// stable URL lets idiomorph see no change and keep the loaded image.
func (s *Store) SignedURL(id, viewerID string, ttl time.Duration) string {
	_ = viewerID
	exp := stableExpiry(time.Now(), ttl)
	sig := s.SignShared(id, exp)
	return fmt.Sprintf("/uploads/%s?exp=%d&sig=%s", id, exp.Unix(), sig)
}

// stableExpiry rounds the expiry onto a fixed grid so repeated calls
// within the same window return an identical timestamp — and therefore
// an identical signed URL. The grid step is ttl/12 (floored at one
// minute); the returned expiry is always at least ttl in the future, so
// callers keep their intended minimum validity. The URL only changes
// once per step (e.g. every 2h for a 24h ttl), so images reload at most
// that often instead of on every render.
func stableExpiry(now time.Time, ttl time.Duration) time.Time {
	step := ttl / 12
	if step < time.Minute {
		step = time.Minute
	}
	return now.Truncate(step).Add(step + ttl)
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
	return s.Save(ctx, ownerID, communityID, mime, "", bytes.NewReader(data))
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

	cleanName := sanitiseFilename(filename)
	u := Upload{
		ID: uuid.NewString(), OwnerID: ownerID, CommunityID: communityID,
		SHA256: digest, MIME: mime, Size: n, RelPath: rel,
		Filename: cleanName, CreatedAt: time.Now(),
	}
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO uploads (id, owner_id, community_id, sha256, mime, size, rel_path, filename, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.OwnerID, u.CommunityID, u.SHA256, u.MIME, u.Size, u.RelPath, u.Filename, u.CreatedAt.Unix()); err != nil {
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
