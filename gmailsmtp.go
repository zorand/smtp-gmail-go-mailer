package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"strings"
	"time"
)

const (
	smtpHost = "smtp.gmail.com"
	smtpPort = "587" // STARTTLS
)

// errStop aborts the whole run (quota exhausted or auth failed): continuing
// would only extend a block or waste attempts.
var errStop = errors.New("stop run")

// errRecipientRejected marks a per-recipient failure (a rejected RCPT TO, e.g. a
// bad address) that should skip only that recipient rather than stop the run.
var errRecipientRejected = errors.New("recipient rejected")

// ---- SMTP mailer -----------------------------------------------------------

type mailer struct {
	username string
	auth     smtp.Auth
	heloName string
	client   *smtp.Client
}

func newMailer(username, password string) *mailer {
	name, err := os.Hostname()
	if err != nil || name == "" {
		name = "localhost"
	}
	return &mailer{
		username: username,
		auth:     smtp.PlainAuth("", username, password, smtpHost),
		heloName: name,
	}
}

func (m *mailer) connect() error {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(smtpHost, smtpPort), 30*time.Second)
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, smtpHost)
	if err != nil {
		_ = conn.Close()
		return err
	}
	if err := c.Hello(m.heloName); err != nil {
		_ = c.Close()
		return err
	}
	if ok, _ := c.Extension("STARTTLS"); !ok {
		_ = c.Close()
		return errors.New("server does not advertise STARTTLS")
	}
	if err := c.StartTLS(&tls.Config{ServerName: smtpHost}); err != nil {
		_ = c.Close()
		return err
	}
	if err := c.Auth(m.auth); err != nil {
		_ = c.Close()
		return err // typically 535 on a bad app password
	}
	m.client = c
	return nil
}

func (m *mailer) close() {
	if m.client != nil {
		_ = m.client.Quit()
		m.client = nil
	}
}

// send performs one SMTP transaction, connecting lazily. On a protocol-level
// rejection (*textproto.Error) the connection is reset and kept; on an I/O
// error it is dropped so the next attempt reconnects.
func (m *mailer) send(from, to string, raw []byte) error {
	if m.client == nil {
		if err := m.connect(); err != nil {
			m.client = nil
			return err
		}
	}
	if err := m.transact(from, to, raw); err != nil {
		var te *textproto.Error
		if errors.As(err, &te) {
			_ = m.client.Reset() // command rejected; connection still usable
		} else {
			m.close() // I/O failure; force a reconnect next time
		}
		return err
	}
	_ = m.client.Reset()
	return nil
}

func (m *mailer) transact(from, to string, raw []byte) error {
	if err := m.client.Mail(from); err != nil {
		return err
	}
	if err := m.client.Rcpt(to); err != nil {
		// Wrap with both the sentinel and the underlying error so the loop can
		// skip just this recipient while classify() still sees the SMTP code.
		return fmt.Errorf("%w: %w", errRecipientRejected, err)
	}
	w, err := m.client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close() // writes the "." terminator; server's final reply lands here
}

func sendWithRetry(ctx context.Context, m *mailer, from, to string, raw []byte, retries int) error {
	const baseDelay = 2 * time.Second
	const maxDelay = 60 * time.Second

	for attempt := 0; ; attempt++ {
		err := m.send(from, to, raw)
		if err == nil {
			return nil
		}

		switch classify(err) {
		case errClassStop:
			return fmt.Errorf("%w: %v", errStop, err)
		case errClassPermanent:
			return err // bad address / rejected message; skip this recipient
		}

		if attempt >= retries {
			// Persistent transient failure often means the account, not just a
			// momentary hiccup, is throttled. Stop instead of thrashing.
			return fmt.Errorf("%w: giving up after %d attempts: %v", errStop, retries+1, err)
		}
		delay := baseDelay << attempt
		if delay > maxDelay {
			delay = maxDelay
		}
		delay += time.Duration(rand.Int63n(int64(time.Second)))
		log.Printf("transient error (attempt %d/%d), backing off %s: %v", attempt+1, retries, delay.Round(time.Second), err)

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type errClass int

const (
	errClassTransient errClass = iota
	errClassPermanent
	errClassStop
)

func classify(err error) errClass {
	var te *textproto.Error
	if !errors.As(err, &te) {
		// I/O error, EOF, dial failure -- retry on a fresh connection.
		return errClassTransient
	}

	msg := strings.ToLower(te.Msg)
	switch {
	case te.Code == 535: // authentication failed -- app password/2FA problem
		return errClassStop
	case te.Code == 550 && (strings.Contains(msg, "5.4.5") || strings.Contains(msg, "quota") || strings.Contains(msg, "limit")):
		return errClassStop // daily sending quota exceeded
	case te.Code == 421, te.Code == 454: // service unavailable / temp auth -- back off
		return errClassTransient
	case te.Code >= 400 && te.Code < 500:
		return errClassTransient
	default: // other 5xx: bad recipient, message rejected, etc.
		return errClassPermanent
	}
}
