package mailer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/mail"
	"strings"
	"unicode"
)

// messenger is implemented by every transport that can deliver a rendered message.
type messenger interface {
	SendMessage(context.Context, string, Message, string) (string, error)
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func sendCode(ctx context.Context, m messenger, origin, role, to, code, id string) error {
	labels := map[string]string{"admin": "admin-login", "partner": "partner-login", "customer": "order-recovery"}
	label, ok := labels[role]
	if !ok {
		return errors.New("unsupported email kind")
	}
	message, e := BrandedTemplate(label, ContextBrand(ctx, origin), map[string]string{"VERIFICATION_CODE": code})
	if e != nil {
		return e
	}
	_, e = m.SendMessage(ctx, to, message, "storefront-auth/"+id)
	return e
}

func sendInvitation(ctx context.Context, m messenger, origin, to, joinOrigin, id string) error {
	brand := ContextBrand(ctx, origin)
	brand.WorkspaceURL = strings.TrimRight(joinOrigin, "/") + "/join"
	message, e := BrandedTemplate("partner-welcome", brand, nil)
	if e != nil {
		return e
	}
	_, e = m.SendMessage(ctx, to, message, "storefront-invitation/"+id)
	return e
}

func freezeBody(from, to string, message Message) (string, error) {
	if _, e := mail.ParseAddress(to); e != nil {
		return "", errors.New("invalid recipient")
	}
	if message.SenderName != "" {
		if len(message.SenderName) > 512 || strings.ContainsFunc(message.SenderName, unicode.IsControl) {
			return "", errors.New("invalid mail display name")
		}
		address, _ := mail.ParseAddress(from)
		address.Name = message.SenderName
		from = address.String()
	}
	body, e := json.Marshal(Frozen{From: from, To: []string{to}, Message: message})
	return string(body), e
}
