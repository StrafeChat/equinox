// Package mail sends the few transactional emails the API has: the verification link a
// new account gets and the password-reset link anyone can ask for. It is deliberately a
// thin SMTP client - the queueing, retries, DKIM signing and MX delivery that make mail
// actually arrive are the relay's job (the compose deployment bundles one, deploy/mail),
// and any SMTP server an operator already runs does the same. Nothing here depends on a
// third-party API.
package mail

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	gomail "github.com/wneessen/go-mail"

	"github.com/StrafeChat/equinox/internal/config"
)

// Message is one email to one recipient. Text is required; HTML, when set, is offered as
// an alternative part so clients that render it do and the rest see the text.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Mailer is what the auth service depends on. Nil means "this instance cannot send
// email" - every caller treats that as a feature being off, never as an error path.
type Mailer interface {
	Send(ctx context.Context, msg Message) error
}

// sendTimeout bounds one delivery to the relay. The relay is on the same network (or a
// provider's submission endpoint), so anything this slow is down, and a registration or
// a login must not hang on it for longer.
const sendTimeout = 20 * time.Second

// SMTP delivers through one SMTP server. A fresh connection per message: the volume here
// is a few mails an hour at most, and a client per send is simpler to reason about than a
// shared one with reconnection rules.
type SMTP struct {
	cfg        config.MailConfig
	fromDomain string
}

// NewSMTP builds a mailer from a validated MailConfig (config.Load already refused the
// shapes that cannot work). It does not connect: a relay that is down at boot must not
// keep the API from starting.
func NewSMTP(cfg config.MailConfig) (*SMTP, error) {
	if !cfg.Enabled {
		return nil, errors.New("mail: SMTP_HOST is not set")
	}
	at := strings.LastIndexByte(cfg.From, '@')
	if at < 0 || at == len(cfg.From)-1 {
		return nil, errors.New("mail: MAIL_FROM has no domain")
	}
	return &SMTP{cfg: cfg, fromDomain: cfg.From[at+1:]}, nil
}

func (s *SMTP) client() (*gomail.Client, error) {
	opts := []gomail.Option{
		gomail.WithPort(s.cfg.Port),
		gomail.WithTimeout(sendTimeout),
		// EHLO with the sender's domain rather than the container's random hostname: some
		// relays refuse a HELO that is not a fully-qualified name.
		gomail.WithHELO(s.fromDomain),
	}
	switch s.cfg.TLS {
	case "tls":
		opts = append(opts, gomail.WithSSL())
	case "none":
		opts = append(opts, gomail.WithTLSPolicy(gomail.NoTLS))
	default:
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSMandatory))
	}
	if s.cfg.Username != "" {
		// Let the server say which mechanisms it offers rather than hard-coding PLAIN; a
		// provider that only advertises LOGIN or CRAM-MD5 then works unchanged.
		opts = append(opts,
			gomail.WithSMTPAuth(gomail.SMTPAuthAutoDiscover),
			gomail.WithUsername(s.cfg.Username),
			gomail.WithPassword(s.cfg.Password),
		)
	}
	return gomail.NewClient(s.cfg.Host, opts...)
}

// Send delivers one message, or returns why the relay would not take it.
func (s *SMTP) Send(ctx context.Context, msg Message) error {
	if msg.To == "" || msg.Subject == "" || msg.Text == "" {
		return errors.New("mail: message needs To, Subject and Text")
	}
	m := gomail.NewMsg()
	if err := m.FromFormat(s.cfg.FromName, s.cfg.From); err != nil {
		return fmt.Errorf("mail: from: %w", err)
	}
	if err := m.To(msg.To); err != nil {
		return fmt.Errorf("mail: to: %w", err)
	}
	m.Subject(msg.Subject)
	// A Message-ID under the sender's domain, not the container hostname go-mail would
	// otherwise use - a bare non-FQDN there is a spam signal at the big providers.
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return err
	}
	m.SetMessageIDWithValue(fmt.Sprintf("<%s@%s>", hex.EncodeToString(idBytes[:]), s.fromDomain))
	m.SetUserAgent("StrafeChat")
	m.SetBodyString(gomail.TypeTextPlain, msg.Text)
	if msg.HTML != "" {
		m.AddAlternativeString(gomail.TypeTextHTML, msg.HTML)
	}

	c, err := s.client()
	if err != nil {
		return fmt.Errorf("mail: client: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := c.DialAndSendWithContext(ctx, m); err != nil {
		return fmt.Errorf("mail: send via %s:%d: %w", s.cfg.Host, s.cfg.Port, err)
	}
	return nil
}
