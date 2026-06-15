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

	switch strings.ToLower(strings.TrimSpace(encoding)) {
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
// Falls back to raw bytes when the encoding is empty / unrecognised
// (7bit / 8bit / binary all pass through). On decode failure we ALSO
// return raw so the user gets SOMETHING — better an oddly-encoded
// download than an empty file.
func decodeAttachmentBytes(raw []byte, encoding string) []byte {
	if len(raw) == 0 {
		return raw
	}
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(string(stripWhitespace(raw)))
		if err != nil {
			return raw
		}
		return decoded
	case "quoted-printable":
		r := quotedprintable.NewReader(bytes.NewReader(raw))
		decoded, err := io.ReadAll(r)
		if err != nil {
			return raw
		}
		return decoded
	}
	return raw
}
