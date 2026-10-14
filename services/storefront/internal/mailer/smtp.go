package mailer

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// SMTP delivers frozen messages through a standard SMTP server (STARTTLS or
// implicit TLS on port 465). Plaintext without STARTTLS is only accepted for
// loopback relays.
type SMTP struct {
	host, user, pass, from string
	port                   int
	Origin                 string
}

func NewSMTP(host string, port int, user, pass, from string) (*SMTP, error) {
	if host == "" || len(host) > 253 || strings.ContainsAny(host, "\r\n /@") {
		return nil, errors.New("invalid SMTP host")
	}
	if port < 1 || port > 65535 {
		return nil, errors.New("invalid SMTP port")
	}
	if _, e := mail.ParseAddress(from); e != nil {
		return nil, errors.New("invalid mail sender")
	}
	if strings.ContainsAny(user, "\r\n") || strings.ContainsAny(pass, "\r\n") {
		return nil, errors.New("invalid SMTP credentials")
	}
	return &SMTP{host: host, port: port, user: user, pass: pass, from: from, Origin: "http://127.0.0.1:8093"}, nil
}

func (s *SMTP) SendCode(ctx context.Context, role, to, code, id string) error {
	return sendCode(ctx, s, s.Origin, role, to, code, id)
}

func (s *SMTP) SendInvitation(ctx context.Context, to, origin, id string) error {
	return sendInvitation(ctx, s, s.Origin, to, origin, id)
}

func (s *SMTP) SendMessage(ctx context.Context, to string, message Message, idempotencyKey string) (string, error) {
	body, e := s.Freeze(to, message)
	if e != nil {
		return "", e
	}
	return s.SendFrozen(ctx, body, idempotencyKey)
}

func (s *SMTP) Freeze(to string, message Message) (string, error) {
	return freezeBody(s.from, to, message)
}

func (s *SMTP) SendFrozen(ctx context.Context, body, idempotencyKey string) (string, error) {
	var saved Frozen
	if json.Unmarshal([]byte(body), &saved) != nil || !SameSender(saved.From, s.from) || len(saved.To) != 1 {
		return "", errors.New("restore original mail sender before retry")
	}
	if _, e := mail.ParseAddress(saved.To[0]); e != nil {
		return "", errors.New("invalid saved recipient")
	}
	raw, e := renderRFC822(saved)
	if e != nil {
		return "", e
	}
	if e = s.deliver(ctx, saved.To[0], raw); e != nil {
		return "", e
	}
	sum := sha256Hex(idempotencyKey + ":" + body)
	return "smtp:" + sum[:24], nil
}

func (s *SMTP) deliver(ctx context.Context, to string, raw []byte) error {
	address, _ := mail.ParseAddress(s.from)
	recipient, _ := mail.ParseAddress(to)
	dialer := &net.Dialer{Timeout: 8 * time.Second}
	var conn net.Conn
	var e error
	target := net.JoinHostPort(s.host, strconv.Itoa(s.port))
	if s.port == 465 {
		conn, e = dialer.DialContext(ctx, "tcp", target)
		if e != nil {
			return errors.New("mail transport failed")
		}
		tlsConn := tls.Client(conn, &tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12})
		if e = tlsConn.HandshakeContext(ctx); e != nil {
			conn.Close()
			return errors.New("mail transport failed")
		}
		conn = tlsConn
	} else {
		conn, e = dialer.DialContext(ctx, "tcp", target)
		if e != nil {
			return errors.New("mail transport failed")
		}
	}
	client, e := smtp.NewClient(conn, s.host)
	if e != nil {
		conn.Close()
		return errors.New("mail transport failed")
	}
	defer client.Close()
	if s.port != 465 && !loopbackHost(s.host) {
		ok, _ := client.Extension("STARTTLS")
		if !ok {
			return errors.New("mail server refuses STARTTLS")
		}
		if e = client.StartTLS(&tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}); e != nil {
			return errors.New("mail transport failed")
		}
	}
	if s.user != "" {
		if e = client.Auth(smtp.PlainAuth("", s.user, s.pass, s.host)); e != nil {
			return errors.New("mail authentication failed")
		}
	}
	if e = client.Mail(address.Address); e != nil {
		return errors.New("mail sender rejected")
	}
	if e = client.Rcpt(recipient.Address); e != nil {
		return errors.New("mail recipient rejected")
	}
	w, e := client.Data()
	if e != nil {
		return errors.New("mail transport failed")
	}
	if _, e = w.Write(raw); e != nil {
		w.Close()
		return errors.New("mail transport failed")
	}
	if e = w.Close(); e != nil {
		return errors.New("mail provider rejected request")
	}
	_ = client.Quit()
	return nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func headerValue(kind, value string) (string, error) {
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("invalid %s", kind)
	}
	return mime.QEncoding.Encode("utf-8", value), nil
}

func renderRFC822(saved Frozen) ([]byte, error) {
	subject, e := headerValue("mail subject", saved.Subject)
	if e != nil {
		return nil, e
	}
	if strings.ContainsAny(saved.From, "\r\n") || strings.ContainsAny(saved.To[0], "\r\n") {
		return nil, errors.New("invalid mail address")
	}
	var b strings.Builder
	b.WriteString("From: " + saved.From + "\r\n")
	b.WriteString("To: " + saved.To[0] + "\r\n")
	b.WriteString("Subject: " + subject + "\r\n")
	b.WriteString("Date: " + time.Now().Format("Mon, 02 Jan 2006 15:04:05 -0700") + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	part := func(contentType, value string) string {
		return "Content-Type: " + contentType + "; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
			base64.StdEncoding.EncodeToString([]byte(value)) + "\r\n"
	}
	switch {
	case saved.Text != "" && saved.HTML != "":
		boundary := "=_storefront_" + sha256Hex(saved.Subject + saved.From)[:24]
		b.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n\r\n")
		b.WriteString("--" + boundary + "\r\n" + part("text/plain", saved.Text))
		b.WriteString("--" + boundary + "\r\n" + part("text/html", saved.HTML))
		b.WriteString("--" + boundary + "--\r\n")
	case saved.HTML != "":
		b.WriteString(part("text/html", saved.HTML))
	default:
		b.WriteString(part("text/plain", saved.Text))
	}
	return []byte(b.String()), nil
}
