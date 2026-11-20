package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// smtpDial connects to the submission server, with TLS from the first byte
// (security "tls") or upgraded with STARTTLS ("starttls"), and logs in.
// It never talks to a server without TLS: a server that doesn't offer
// STARTTLS is refused before the password is sent.
func smtpDial(ctx context.Context, cfg AccountConfig, tlsFor func(string) *tls.Config) (*smtp.Client, error) {
	h := cfg.SMTP
	port := h.Port
	if port == 0 {
		port = map[string]int{"tls": 465, "starttls": 587}[h.Security]
	}
	addr := net.JoinHostPort(h.Host, strconv.Itoa(port))
	d := &net.Dialer{Timeout: 20 * time.Second}
	var conn net.Conn
	var err error
	switch h.Security {
	case "tls":
		conn, err = (&tls.Dialer{NetDialer: d, Config: tlsFor(h.Host)}).DialContext(ctx, "tcp", addr)
	case "starttls":
		conn, err = d.DialContext(ctx, "tcp", addr)
	default:
		return nil, fmt.Errorf("smtp security %q refused: use tls or starttls (never plaintext)", h.Security)
	}
	if err != nil {
		return nil, fmt.Errorf("SMTP %s: %w", addr, err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, h.Host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP %s: %w", addr, err)
	}
	domain := cfg.Address[strings.LastIndexByte(cfg.Address, '@')+1:]
	if err := c.Hello(domain); err != nil {
		c.Close()
		return nil, fmt.Errorf("SMTP %s: %w", addr, err)
	}
	if h.Security == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			c.Close()
			return nil, fmt.Errorf("SMTP server %s doesn't offer STARTTLS: refusing to send mail or a password without TLS", addr)
		}
		if err := c.StartTLS(tlsFor(h.Host)); err != nil {
			c.Close()
			return nil, fmt.Errorf("SMTP %s STARTTLS: %w", addr, err)
		}
	}
	if ok, _ := c.Extension("AUTH"); !ok {
		c.Close()
		return nil, fmt.Errorf("SMTP server %s doesn't offer login", addr)
	}
	if err := c.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, h.Host)); err != nil {
		c.Close()
		return nil, errors.New("SMTP login failed: the server refused the username or password")
	}
	return c, nil
}

// smtpSend submits one message. raw must not carry a Bcc header.
func smtpSend(ctx context.Context, cfg AccountConfig, tlsFor func(string) *tls.Config, from string, rcpts []string, raw []byte) error {
	c, err := smtpDial(ctx, cfg, tlsFor)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("SMTP MAIL FROM: %w", err)
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("SMTP RCPT TO %s: %w", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	return c.Quit()
}
