package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode"
)

type Resend struct {
	key, from string
	client    *http.Client
	Origin    string
}

func NewResend(key, from string) (*Resend, error) {
	if key == "" {
		return nil, errors.New("Resend key required")
	}
	if _, e := mail.ParseAddress(from); e != nil {
		return nil, errors.New("invalid mail sender")
	}
	return &Resend{key: key, from: from, client: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, Origin: "http://127.0.0.1:8093"}, nil
}
func (r *Resend) SendCode(ctx context.Context, role, to, code, id string) error {
	return sendCode(ctx, r, r.Origin, role, to, code, id)
}
func (r *Resend) SendInvitation(ctx context.Context, to, origin, id string) error {
	return sendInvitation(ctx, r, r.Origin, to, origin, id)
}
func (r *Resend) SendMessage(ctx context.Context, to string, message Message, idempotencyKey string) (string, error) {
	body, e := r.Freeze(to, message)
	if e != nil {
		return "", e
	}
	return r.SendFrozen(ctx, body, idempotencyKey)
}

type Sender interface {
	Freeze(string, Message) (string, error)
	SendFrozen(context.Context, string, string) (string, error)
}
type Frozen struct {
	From string   `json:"from"`
	To   []string `json:"to"`
	Message
}

func (r *Resend) Freeze(to string, message Message) (string, error) {
	return freezeBody(r.from, to, message)
}

func (r *Resend) SendFrozen(ctx context.Context, body, idempotencyKey string) (string, error) {
	var saved Frozen
	if json.Unmarshal([]byte(body), &saved) != nil || !SameSender(saved.From, r.from) || len(saved.To) != 1 {
		return "", errors.New("restore original mail sender before retry")
	}
	if _, e := mail.ParseAddress(saved.To[0]); e != nil {
		return "", errors.New("invalid saved recipient")
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.resend.com/emails", bytes.NewReader([]byte(body)))
	if e != nil {
		return "", e
	}
	req.Header.Set("Authorization", "Bearer "+r.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	res, e := r.client.Do(req)
	if e != nil {
		return "", errors.New("mail transport failed")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", errors.New("mail provider rejected request")
	}
	data, e := io.ReadAll(io.LimitReader(res.Body, 16385))
	if e != nil || len(data) > 16384 {
		return "", errors.New("invalid mail response")
	}
	var sent struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &sent) != nil || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`).MatchString(sent.ID) {
		return "", errors.New("mail response missing message id")
	}
	return sent.ID, nil
}

// Display names may vary per store, but the verified sending mailbox cannot change.
func SameSender(saved, configured string) bool {
	a, e := mail.ParseAddress(saved)
	if e != nil || strings.ContainsFunc(saved, unicode.IsControl) {
		return false
	}
	b, e := mail.ParseAddress(configured)
	return e == nil && a.Address == b.Address
}
