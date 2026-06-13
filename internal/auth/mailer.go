package auth

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log/slog"
	"mime"
	"net/smtp"
	"strings"
	"time"
)

type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// TLSMode controls how the connection is secured.
//   - "auto"     plain connect, STARTTLS upgrade if server advertises it
//   - "starttls" plain connect, require STARTTLS or fail
//   - "tls"      implicit TLS from the first byte (SMTPS, port 465)
//   - "none"     plaintext only
type SMTPMailer struct {
	Host     string
	Port     int
	User     string
	Pass     string
	From     string
	TLSMode  string
	TLSSkip  bool
	Log      *slog.Logger
}

func (m *SMTPMailer) Send(_ context.Context, to, subject, body string) error {
	addr := fmt.Sprintf("%s:%d", m.Host, m.Port)
	msg := buildMessage(m.From, to, subject, body)

	tlsCfg := &tls.Config{ServerName: m.Host, InsecureSkipVerify: m.TLSSkip}
	mode := strings.ToLower(strings.TrimSpace(m.TLSMode))
	if mode == "" {
		mode = "auto"
	}

	var (
		c   *smtp.Client
		err error
	)
	if mode == "tls" {
		conn, dErr := tls.Dial("tcp", addr, tlsCfg)
		if dErr != nil {
			return m.fail(to, "tls dial", dErr)
		}
		c, err = smtp.NewClient(conn, m.Host)
	} else {
		c, err = smtp.Dial(addr)
	}
	if err != nil {
		return m.fail(to, "smtp dial", err)
	}
	defer c.Close()

	if err := c.Hello("localhost"); err != nil {
		return m.fail(to, "hello", err)
	}

	if mode == "auto" || mode == "starttls" {
		ok, _ := c.Extension("STARTTLS")
		if ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return m.fail(to, "starttls", err)
			}
		} else if mode == "starttls" {
			return m.fail(to, "starttls", fmt.Errorf("server did not advertise STARTTLS"))
		}
	}

	if m.User != "" {
		if err := c.Auth(smtp.PlainAuth("", m.User, m.Pass, m.Host)); err != nil {
			return m.fail(to, "auth", err)
		}
	}
	if err := c.Mail(m.From); err != nil {
		return m.fail(to, "mail from", err)
	}
	if err := c.Rcpt(to); err != nil {
		return m.fail(to, "rcpt", err)
	}
	w, err := c.Data()
	if err != nil {
		return m.fail(to, "data", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return m.fail(to, "write", err)
	}
	if err := w.Close(); err != nil {
		return m.fail(to, "data close", err)
	}
	if err := c.Quit(); err != nil {
		m.Log.Warn("smtp quit", "to", to, "err", err)
	}
	m.Log.Info("smtp sent", "to", to, "subject", subject, "tls", mode)
	return nil
}

func (m *SMTPMailer) fail(to, stage string, err error) error {
	m.Log.Warn("smtp send failed", "to", to, "stage", stage, "err", err)
	return fmt.Errorf("smtp %s: %w", stage, err)
}

func buildMessage(from, to, subject, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", randomID(), hostFromAddr(from))
	fmt.Fprintf(&b, "From: %s\r\n", encodeAddr(from))
	fmt.Fprintf(&b, "To: %s\r\n", encodeAddr(to))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("UTF-8", subject))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(normalizeCRLF(body))
	return b.String()
}

func randomID() string {
	var buf [12]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

func hostFromAddr(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		h := addr[i+1:]
		h = strings.TrimRight(h, ">")
		if h != "" {
			return h
		}
	}
	return "localhost"
}

// encodeAddr Q-encodes a display name when it contains non-ASCII, leaving the
// "<email>" portion untouched. Plain addresses pass through.
func encodeAddr(a string) string {
	lt := strings.LastIndex(a, "<")
	if lt <= 0 {
		return a
	}
	name := strings.TrimSpace(a[:lt])
	addr := a[lt:]
	if isASCII(name) {
		return a
	}
	return mime.QEncoding.Encode("UTF-8", name) + " " + addr
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7f {
			return false
		}
	}
	return true
}

func normalizeCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

type LogMailer struct{ Log *slog.Logger }

func (l *LogMailer) Send(_ context.Context, to, subject, body string) error {
	l.Log.Info("mail (log-only)", "to", to, "subject", subject, "body", body)
	return nil
}
