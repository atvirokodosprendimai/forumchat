package mailbox

import (
	"crypto/tls"
	"errors"
	"fmt"
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

// FetchedEnvelope is one Phase 2 result row — just enough to log "I
// saw this message". Body bytes and BODYSTRUCTURE walking come in
// Phase 3/4.
type FetchedEnvelope struct {
	UID          uint32
	FromAddr     string
	FromName     string
	Subject      string
	MessageID    string
	InternalDate time.Time
}

// fetchEnvelopesSince fetches messages with UID strictly greater than
// since across the currently-examined mailbox. The UIDRange uses
// 0 as the upper bound, which the protocol encodes as "*" — every
// UID from since+1 to the end of the mailbox.
func (i *imapClient) fetchEnvelopesSince(since uint32) ([]FetchedEnvelope, error) {
	if since == ^uint32(0) {
		return nil, errors.New("imap: refusing to fetch with overflow since value")
	}
	set := imap.UIDSet{imap.UIDRange{Start: imap.UID(since + 1), Stop: 0}}
	cmd := i.c.Fetch(set, &imap.FetchOptions{
		UID:          true,
		Envelope:     true,
		InternalDate: true,
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
	return env
}
