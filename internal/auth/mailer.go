package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net/smtp"
	"strings"
)

type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

type SMTPMailer struct {
	Host string
	Port int
	User string
	Pass string
	From string
	Log  *slog.Logger
}

func (m *SMTPMailer) Send(_ context.Context, to, subject, body string) error {
	addr := fmt.Sprintf("%s:%d", m.Host, m.Port)
	var auth smtp.Auth
	if m.User != "" {
		auth = smtp.PlainAuth("", m.User, m.Pass, m.Host)
	}
	msg := strings.Builder{}
	fmt.Fprintf(&msg, "From: %s\r\n", m.From)
	fmt.Fprintf(&msg, "To: %s\r\n", to)
	fmt.Fprintf(&msg, "Subject: %s\r\n", subject)
	msg.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	msg.WriteString("\r\n")
	msg.WriteString(body)
	if err := smtp.SendMail(addr, auth, m.From, []string{to}, []byte(msg.String())); err != nil {
		m.Log.Warn("smtp send failed", "to", to, "err", err)
		return fmt.Errorf("smtp send: %w", err)
	}
	m.Log.Info("smtp sent", "to", to, "subject", subject)
	return nil
}

type LogMailer struct{ Log *slog.Logger }

func (l *LogMailer) Send(_ context.Context, to, subject, body string) error {
	l.Log.Info("mail (log-only)", "to", to, "subject", subject, "body", body)
	return nil
}
