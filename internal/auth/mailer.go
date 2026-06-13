package auth

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/smtp"
	"strings"
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
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.String()
}

type LogMailer struct{ Log *slog.Logger }

func (l *LogMailer) Send(_ context.Context, to, subject, body string) error {
	l.Log.Info("mail (log-only)", "to", to, "subject", subject, "body", body)
	return nil
}
