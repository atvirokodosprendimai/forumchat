package mailbox

import (
	"crypto/tls"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// imapClient holds every IMAP API call in the mailbox package so the
// read-only contract is enforced by a single greppable file. Anything
// in the rest of internal/mailbox/ that needs to talk to the server
// goes through these methods; the CI gate forbids direct imapclient.*
// calls elsewhere.
type imapClient struct {
	c *imapclient.Client
}

// dial opens an authenticated, read-only IMAP session. The TLS mode
// matches the spec's config knob: "tls" wraps the connection from the
// start, "starttls" upgrades after a plaintext greeting, "none" speaks
// plaintext (test-mailbox only — do not use against the public internet).
func dial(cfg AccountConfig) (*imapClient, error) {
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	var (
		c   *imapclient.Client
		err error
	)
	switch strings.ToLower(cfg.TLSMode) {
	case "", "tls":
		c, err = imapclient.DialTLS(addr, &imapclient.Options{
			TLSConfig: &tls.Config{ServerName: cfg.Host},
		})
	case "starttls":
		c, err = imapclient.DialStartTLS(addr, &imapclient.Options{
			TLSConfig: &tls.Config{ServerName: cfg.Host},
		})
	case "none":
		c, err = imapclient.DialInsecure(addr, nil)
	default:
		return nil, fmt.Errorf("imap: unknown tls mode %q", cfg.TLSMode)
	}
	if err != nil {
		return nil, fmt.Errorf("imap dial: %w", err)
	}
	if err := c.Login(cfg.Username, cfg.Password).Wait(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("imap login: %w", err)
	}
	return &imapClient{c: c}, nil
}

// close logs out and tears down the connection. Errors during logout
// are swallowed — the connection is going away anyway.
func (i *imapClient) close() {
	_ = i.c.Logout().Wait()
	_ = i.c.Close()
}

// listFolders enumerates every mailbox the user can SELECT. We deliberately
// do NOT filter by \Noselect — the consumer skips mailboxes that fail to
// examine, and a broken folder doesn't sink the whole cycle.
func (i *imapClient) listFolders() ([]string, error) {
	cmd := i.c.List("", "*", nil)
	datas, err := cmd.Collect()
	if err != nil {
		return nil, fmt.Errorf("imap list: %w", err)
	}
	out := make([]string, 0, len(datas))
	for _, d := range datas {
		if hasNoselect(d.Attrs) {
			continue
		}
		out = append(out, d.Mailbox)
	}
	return out, nil
}

func hasNoselect(attrs []imap.MailboxAttr) bool {
	for _, a := range attrs {
		if a == imap.MailboxAttrNoSelect {
			return true
		}
	}
	return false
}

// SelectInfo is the subset of imap.SelectData the poll loop cares
// about. Kept narrow so internal/mailbox/poll.go doesn't need to know
// about every field on the upstream struct.
type SelectInfo struct {
	UIDValidity uint32
	UIDNext     uint32 // 0 when server didn't send one
	NumMessages uint32
}

// examineReadOnly issues an EXAMINE on the named mailbox. The READ-ONLY
// flag is enforced here so callers never get to set it; combined with
// the CI grep gate, the read-only invariant is mechanically guaranteed.
func (i *imapClient) examineReadOnly(name string) (SelectInfo, error) {
	cmd := i.c.Select(name, &imap.SelectOptions{ReadOnly: true})
	data, err := cmd.Wait()
	if err != nil {
		return SelectInfo{}, fmt.Errorf("imap examine %q: %w", name, err)
	}
	return SelectInfo{
		UIDValidity: data.UIDValidity,
		UIDNext:     uint32(data.UIDNext),
		NumMessages: data.NumMessages,
	}, nil
}

// FetchedEnvelope is one message's envelope + attachment metadata, the
// poll worker's unit of work. TextPath is the BODYSTRUCTURE path of the
// best text part (text/plain preferred, else text/html), pre-resolved
// during envelopeFromBuffer so the caller can ask for body bytes with
// one targeted BODY.PEEK[<path>] round-trip and never has to walk the
// BS tree itself.
type FetchedEnvelope struct {
	UID          uint32
	FromAddr     string
	FromName     string
	Subject      string
	MessageID    string
	InternalDate time.Time
	TextPath     []int // empty when the message has no text part
	IsTextPlain  bool  // true when TextPath points at text/plain; false for text/html fallback
	Attachments  []ParsedPart
}

// ParsedPart describes one attachment part discovered in the
// BODYSTRUCTURE tree. Bytes are NOT downloaded — only metadata.
// MIMEPartID matches IMAP's body-part numbering (e.g. "2", "2.1") so
// the lazy-fetch handler can request exactly this part later with
// BODY.PEEK[2.1].
type ParsedPart struct {
	Filename   string
	MIME       string
	SizeBytes  int64
	MIMEPartID string
}

// fetchEnvelopesSince fetches envelope + BODYSTRUCTURE for every UID
// strictly greater than since. Returns ALL envelopes in one call —
// callers cannot issue another IMAP command while this stream is
// in-flight (single client = single command at a time), so streaming
// via callback was abandoned in favour of buffer-then-process. The
// per-message cursor save happens during the processing loop instead.
func (i *imapClient) fetchEnvelopesSince(since uint32) ([]FetchedEnvelope, error) {
	if since == ^uint32(0) {
		return nil, errors.New("imap: refusing to fetch with overflow since value")
	}
	set := imap.UIDSet{imap.UIDRange{Start: imap.UID(since + 1), Stop: 0}}
	cmd := i.c.Fetch(set, &imap.FetchOptions{
		UID:           true,
		Envelope:      true,
		InternalDate:  true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	})
	out := []FetchedEnvelope{}
	for {
		msg := cmd.Next()
		if msg == nil {
			break
		}
		buf, err := msg.Collect()
		if err != nil {
			return nil, fmt.Errorf("imap fetch envelope: %w", err)
		}
		out = append(out, envelopeFromBuffer(buf))
	}
	if err := cmd.Close(); err != nil {
		return nil, fmt.Errorf("imap fetch close: %w", err)
	}
	return out, nil
}

// walkAttachmentParts extracts attachment metadata from a parsed
// BODYSTRUCTURE tree. The protocol numbers parts depth-first starting at
// 1; we mirror that scheme so the resulting MIMEPartID is what IMAP's
// BODY.PEEK[...] command expects.
//
// An attachment is any leaf with either disposition=attachment OR a
// filename present in Content-Type's "name" param. Inline images and
// quoted-reply bodies fall through as text/* and are skipped here.
func walkAttachmentParts(bs imap.BodyStructure) []ParsedPart {
	out := []ParsedPart{}
	if bs == nil {
		return out
	}
	bs.Walk(func(path []int, part imap.BodyStructure) bool {
		sp, ok := part.(*imap.BodyStructureSinglePart)
		if !ok {
			return true
		}
		// Reject the "structural root" entry the library emits for a
		// pure singlepart message — Walk reports path [1] for both the
		// root and the only leaf in that case; we keep the leaf because
		// it's the only entry.
		mime := sp.MediaType()
		if strings.HasPrefix(mime, "multipart/") {
			return true
		}
		filename := sp.Filename()
		isAttachment := false
		if d := sp.Disposition(); d != nil && strings.EqualFold(d.Value, "attachment") {
			isAttachment = true
		}
		if filename != "" && !strings.HasPrefix(mime, "text/") {
			isAttachment = true
		}
		if !isAttachment {
			return true
		}
		out = append(out, ParsedPart{
			Filename:   filename,
			MIME:       mime,
			SizeBytes:  int64(sp.Size),
			MIMEPartID: formatPath(path),
		})
		return true
	})
	return out
}

func formatPath(path []int) string {
	if len(path) == 0 {
		return "1"
	}
	parts := make([]string, len(path))
	for i, n := range path {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ".")
}

// fetchTextBodies returns the decoded text/plain and text/html parts
// of one message, if present. Empty strings when the message doesn't
// carry that mime type. Used by the auto-issue path which prefers
// plaintext and falls back to HTML→text conversion.
func (i *imapClient) fetchTextBodies(uid uint32, bs imap.BodyStructure) (textBody, htmlBody string, err error) {
	if bs == nil {
		return "", "", nil
	}
	var textPath, htmlPath []int
	bs.Walk(func(path []int, part imap.BodyStructure) bool {
		sp, ok := part.(*imap.BodyStructureSinglePart)
		if !ok {
			return true
		}
		switch sp.MediaType() {
		case "text/plain":
			if textPath == nil {
				textPath = append([]int(nil), path...)
			}
		case "text/html":
			if htmlPath == nil {
				htmlPath = append([]int(nil), path...)
			}
		}
		return true
	})
	if textPath != nil {
		b, err := i.fetchPartPath(uid, textPath)
		if err != nil {
			return "", "", fmt.Errorf("fetch text/plain: %w", err)
		}
		textBody = string(b)
	}
	if htmlPath != nil {
		b, err := i.fetchPartPath(uid, htmlPath)
		if err != nil {
			return "", "", fmt.Errorf("fetch text/html: %w", err)
		}
		htmlBody = string(b)
	}
	return textBody, htmlBody, nil
}

func (i *imapClient) fetchPartPath(uid uint32, path []int) ([]byte, error) {
	section := &imap.FetchItemBodySection{Peek: true, Part: append([]int(nil), path...)}
	set := imap.UIDSet{imap.UIDRange{Start: imap.UID(uid), Stop: imap.UID(uid)}}
	cmd := i.c.Fetch(set, &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{section},
	})
	var data []byte
	for {
		msg := cmd.Next()
		if msg == nil {
			break
		}
		buf, err := msg.Collect()
		if err != nil {
			return nil, err
		}
		if got := buf.FindBodySection(section); got != nil {
			data = got
		}
	}
	if err := cmd.Close(); err != nil {
		return nil, err
	}
	return data, nil
}

// fetchEnvelopeWithBody fetches everything Phase 7 needs for a single
// new ingest: envelope, BODYSTRUCTURE, plus the text-body parts when
// the matched filter has to_issue=true. Saves one round-trip.
func (i *imapClient) fetchEnvelopeWithBody(uid uint32) (FetchedEnvelope, string, string, error) {
	set := imap.UIDSet{imap.UIDRange{Start: imap.UID(uid), Stop: imap.UID(uid)}}
	cmd := i.c.Fetch(set, &imap.FetchOptions{
		UID:           true,
		Envelope:      true,
		InternalDate:  true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	})
	var env FetchedEnvelope
	var bs imap.BodyStructure
	for {
		msg := cmd.Next()
		if msg == nil {
			break
		}
		buf, err := msg.Collect()
		if err != nil {
			return FetchedEnvelope{}, "", "", err
		}
		env = envelopeFromBuffer(buf)
		bs = buf.BodyStructure
	}
	if err := cmd.Close(); err != nil {
		return FetchedEnvelope{}, "", "", err
	}
	text, html, err := i.fetchTextBodies(uid, bs)
	if err != nil {
		return FetchedEnvelope{}, "", "", err
	}
	return env, text, html, nil
}

// fetchPart streams the bytes of a single BODYSTRUCTURE part by UID +
// MIMEPartID. Used by the lazy materialise path: when a user clicks
// "Move attachment to project", we open a short-lived IMAP session,
// SELECT the right folder, and request only the part we need via
// BODY.PEEK[partID]. The Peek=true flag guarantees \Seen is not
// affected — the user's mail client still sees the message as unread.
func (i *imapClient) fetchPart(uid uint32, partID string) ([]byte, error) {
	section := &imap.FetchItemBodySection{Peek: true}
	// Parse partID like "2.1" into the protocol path numbers.
	for _, segment := range strings.Split(partID, ".") {
		n, err := strconv.Atoi(segment)
		if err != nil {
			return nil, fmt.Errorf("imap: bad part id %q", partID)
		}
		section.Part = append(section.Part, n)
	}
	set := imap.UIDSet{imap.UIDRange{Start: imap.UID(uid), Stop: imap.UID(uid)}}
	cmd := i.c.Fetch(set, &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{section},
	})
	var data []byte
	for {
		msg := cmd.Next()
		if msg == nil {
			break
		}
		buf, err := msg.Collect()
		if err != nil {
			return nil, fmt.Errorf("imap fetch part: %w", err)
		}
		if got := buf.FindBodySection(section); got != nil {
			data = got
		}
	}
	if err := cmd.Close(); err != nil {
		return nil, fmt.Errorf("imap fetch part close: %w", err)
	}
	if data == nil {
		return nil, errors.New("imap: server returned no bytes for part")
	}
	return data, nil
}

func envelopeFromBuffer(buf *imapclient.FetchMessageBuffer) FetchedEnvelope {
	env := FetchedEnvelope{
		UID:          uint32(buf.UID),
		InternalDate: buf.InternalDate,
	}
	if buf.Envelope != nil {
		env.Subject = buf.Envelope.Subject
		env.MessageID = buf.Envelope.MessageID
		if len(buf.Envelope.From) > 0 {
			a := buf.Envelope.From[0]
			env.FromName = strings.TrimSpace(a.Name)
			env.FromAddr = strings.ToLower(strings.TrimSpace(a.Addr()))
		}
	}
	if buf.BodyStructure != nil {
		env.Attachments = walkAttachmentParts(buf.BodyStructure)
		env.TextPath, env.IsTextPlain = findTextPartPath(buf.BodyStructure)
	}
	return env
}

// findTextPartPath walks the BODYSTRUCTURE looking for the best text
// part to index for search. Preference: text/plain > text/html. Returns
// the part path (e.g. [1] or [1, 1]) plus whether the hit is plain.
// Empty path means the message has no usable text part — search will
// degrade but ingest still succeeds.
func findTextPartPath(bs imap.BodyStructure) ([]int, bool) {
	var plain, html []int
	bs.Walk(func(path []int, part imap.BodyStructure) bool {
		sp, ok := part.(*imap.BodyStructureSinglePart)
		if !ok {
			return true
		}
		switch sp.MediaType() {
		case "text/plain":
			if plain == nil {
				plain = append([]int(nil), path...)
			}
		case "text/html":
			if html == nil {
				html = append([]int(nil), path...)
			}
		}
		return true
	})
	if plain != nil {
		return plain, true
	}
	if html != nil {
		return html, false
	}
	return nil, false
}
