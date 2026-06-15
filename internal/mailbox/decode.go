package mailbox

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime/quotedprintable"
	"strings"

	gmcharset "github.com/emersion/go-message/charset"
)

// decodeTextBody converts raw IMAP body bytes for a text/* part into
// UTF-8 text. It chains the right Content-Transfer-Encoding reader
// (quoted-printable / base64 / passthrough) with go-message/charset
// for the charset transcode. Errors during transcode degrade to the
// raw input — search still gets SOMETHING even on weird emails — so
// the caller never gets an empty body just because the decoding
// pipeline hiccupped.
func decodeTextBody(raw []byte, encoding, charset string) string {
	if len(raw) == 0 {
		return ""
	}
	var r io.Reader = bytes.NewReader(raw)

	enc := strings.ToLower(strings.TrimSpace(encoding))
	// Empty / "7bit" / "8bit" / "binary" → no transfer-decode declared.
	// Some IMAP servers omit Content-Transfer-Encoding on the BODYSTRUCTURE
	// even when the part on the wire is base64-wrapped (Microsoft 365 has
	// been observed doing this on auto-forwarded mail) OR quoted-printable
	// (Outlook RE: chains on Lithuanian / accented text). Sniff the bytes
	// in order: base64 first (mod-4 alphabet check) then QP (=XX hex
	// sequences). Wrong sniff is harmless because both decoders pass
	// non-matching bytes through unchanged.
	if enc == "" || enc == "7bit" || enc == "8bit" || enc == "binary" {
		switch {
		case looksLikeBase64(raw):
			enc = "base64"
		case looksLikeQuotedPrintable(raw):
			enc = "quoted-printable"
		}
	}
	switch enc {
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, bytes.NewReader(stripWhitespace(raw)))
	}

	if cs := strings.ToLower(strings.TrimSpace(charset)); cs != "" && cs != "utf-8" && cs != "us-ascii" && cs != "ascii" {
		if cr, err := gmcharset.Reader(charset, r); err == nil {
			r = cr
		}
	}

	out, err := io.ReadAll(r)
	if err != nil || len(out) == 0 {
		return string(raw)
	}
	return string(out)
}

func stripWhitespace(raw []byte) []byte {
	out := make([]byte, 0, len(raw))
	for _, b := range raw {
		if b == '\r' || b == '\n' || b == ' ' || b == '\t' {
			continue
		}
		out = append(out, b)
	}
	return out
}

// decodeAttachmentBytes returns the decoded binary representation of a
// MIME part's raw IMAP bytes. Attachments arrive over the wire in
// whatever Content-Transfer-Encoding the sender picked — overwhelmingly
// base64 for binaries (PDF, image, SVG, zip). Saving the raw bytes to
// uploads produced "corrupted" downloads: the file was the base64 text
// envelope, not the actual binary.
//
// When encoding is empty (legacy rows ingested before migration 00025)
// we auto-detect: if the bytes look like base64 (matches the alphabet
// + reasonable padding) we attempt the decode. Same fallback for q-p.
// On any decode failure we return raw — better an oddly-encoded
// download than an empty file.
func decodeAttachmentBytes(raw []byte, encoding string) []byte {
	if len(raw) == 0 {
		return raw
	}
	enc := strings.ToLower(strings.TrimSpace(encoding))
	switch enc {
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(string(stripWhitespace(raw)))
		if err == nil {
			return decoded
		}
		return raw
	case "quoted-printable":
		r := quotedprintable.NewReader(bytes.NewReader(raw))
		if decoded, err := io.ReadAll(r); err == nil {
			return decoded
		}
		return raw
	case "":
		// Legacy row (migration 00025 default) — try base64.
		if looksLikeBase64(raw) {
			if decoded, err := base64.StdEncoding.DecodeString(string(stripWhitespace(raw))); err == nil {
				return decoded
			}
		}
		return raw
	}
	return raw
}

// TryDecodeBase64Text attempts to decode a body string that LOOKS like
// base64-wrapped UTF-8 text. Returns (decoded, true) on success, or
// (original, false) when the input isn't base64 or decoding produces
// non-text bytes. Used by the one-shot CLI repair pass that fixes
// rows ingested before the transfer-encoding decode was wired up.
func TryDecodeBase64Text(body string) (string, bool) {
	raw := []byte(body)
	if !looksLikeBase64(raw) {
		return body, false
	}
	decoded, err := base64.StdEncoding.DecodeString(string(stripWhitespace(raw)))
	if err != nil || len(decoded) == 0 {
		return body, false
	}
	// Reject decodes that produced binary garbage — body_text is always
	// UTF-8 text; a binary result means our base64 detection was wrong.
	if !isMostlyText(decoded) {
		return body, false
	}
	return string(decoded), true
}

// isMostlyText returns true when the byte slice looks like UTF-8 text
// (no NULs, mostly printable). A few control bytes are allowed for
// tab / newline / carriage return.
func isMostlyText(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return true
}

// RewriteCIDImages replaces every `cid:<contentID>` reference in body
// with a markdown image pointing at the corresponding uploaded copy.
// Resolved as `![inline](upload://<uploadID>)`; the view-time helper
// `render.ResolveUploadURLs` converts the upload:// scheme to a signed
// download URL per viewer. References whose contentID has no entry in
// cidToUpload are left alone so a missing inline asset doesn't break
// the body text outright.
func RewriteCIDImages(body string, cidToUpload map[string]string) string {
	if len(cidToUpload) == 0 || body == "" {
		return body
	}
	for cid, uploadID := range cidToUpload {
		// HTML's <img src="cid:X">, Markdown literal `cid:X`, plus the
		// `[cid:X]` form html2text produces from `<img alt="" src="cid:X">`.
		// Replace each in turn — order doesn't matter, all variants land
		// on the same upload:// destination.
		repl := "![inline](upload://" + uploadID + ")"
		variants := []string{
			"[cid:" + cid + "]",
			"<cid:" + cid + ">",
			"(cid:" + cid + ")",
			"cid:" + cid,
		}
		for _, v := range variants {
			body = strings.ReplaceAll(body, v, repl)
		}
	}
	return body
}

// looksLikeQuotedPrintable returns true when the byte stream contains
// at least one well-formed `=XX` hex escape (where XX is two hex digits)
// — a strong tell that the body is quoted-printable even when the
// BODYSTRUCTURE doesn't declare it. False positives on pasted hex
// strings are tolerable: quotedprintable.NewReader passes through
// non-conforming `=` sequences unchanged.
func looksLikeQuotedPrintable(raw []byte) bool {
	for i := 0; i+2 < len(raw); i++ {
		if raw[i] != '=' {
			continue
		}
		if isHex(raw[i+1]) && isHex(raw[i+2]) {
			return true
		}
	}
	return false
}

func isHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'A' && b <= 'F') || (b >= 'a' && b <= 'f')
}

// looksLikeBase64 cheaply checks whether the byte stream is plausible
// base64: every non-whitespace byte must be from the alphabet, with at
// least one block-worth of payload. Avoids decoding plaintext SVGs
// that genuinely contained 7bit content (those rare cases pass through).
func looksLikeBase64(raw []byte) bool {
	if len(raw) < 8 {
		return false
	}
	nonWS := 0
	for _, b := range raw {
		if b == '\r' || b == '\n' || b == ' ' || b == '\t' {
			continue
		}
		if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '+' || b == '/' || b == '=' {
			nonWS++
			continue
		}
		return false
	}
	return nonWS >= 8 && nonWS%4 == 0
}
