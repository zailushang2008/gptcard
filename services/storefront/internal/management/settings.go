package management

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"gptibo/storefront/internal/auth"
	"gptibo/storefront/internal/fault"
)

type Contacts struct {
	Wechat string `json:"wechat"`
	QQ     string `json:"qq"`
	Email  string `json:"email"`
}
type ContactSettings struct {
	Contacts Contacts `json:"contacts"`
	Version  int64    `json:"version"`
}

func (s Service) Contacts(ctx context.Context, owner string) (ContactSettings, error) {
	var out ContactSettings
	e := s.Store.DB.QueryRow(ctx, "SELECT wechat,qq,email,version FROM support_contacts WHERE owner=$1", owner).Scan(&out.Contacts.Wechat, &out.Contacts.QQ, &out.Contacts.Email, &out.Version)
	if errors.Is(e, pgx.ErrNoRows) {
		return out, nil
	}
	return out, e
}

type ContactInput struct {
	Contacts
	Version *int64 `json:"version"`
}

func (s Service) SaveContacts(ctx context.Context, role, token string, in ContactInput) error {
	if in.Version == nil || *in.Version < 0 || !validText(in.Wechat, 64) || !validText(in.QQ, 20) || !validText(in.Email, 254) || strings.ContainsAny(in.Wechat+in.QQ+in.Email, "\r\n\t") {
		return fault.New(400, "联系方式或版本无效。")
	}
	if in.QQ != "" {
		if len(in.QQ) < 5 {
			return fault.New(400, "QQ 号码无效。")
		}
		for _, ch := range in.QQ {
			if ch < '0' || ch > '9' {
				return fault.New(400, "QQ 号码无效。")
			}
		}
	}
	if in.Email != "" {
		email, e := auth.Email(in.Email)
		if e != nil {
			return e
		}
		in.Email = email
	}
	return s.write(ctx, role, token, func(tx pgx.Tx, p auth.Principal) error {
		owner := "platform"
		if role == "partner" {
			owner = p.Subject
		}
		tag, e := tx.Exec(ctx, `INSERT INTO support_contacts(owner,wechat,qq,email,version) SELECT $1,$2,$3,$4,1 WHERE $5::bigint=0
 ON CONFLICT(owner) DO UPDATE SET wechat=excluded.wechat,qq=excluded.qq,email=excluded.email,version=support_contacts.version+1 WHERE support_contacts.version=$5`, owner, strings.TrimSpace(in.Wechat), strings.TrimSpace(in.QQ), in.Email, *in.Version)
		// Existing rows with versions above zero use UPDATE, keeping inserts version-guarded.
		if *in.Version > 0 {
			tag, e = tx.Exec(ctx, "UPDATE support_contacts SET wechat=$1,qq=$2,email=$3,version=version+1 WHERE owner=$4 AND version=$5", strings.TrimSpace(in.Wechat), strings.TrimSpace(in.QQ), in.Email, owner, *in.Version)
		}
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return conflict()
		}
		if _, e = tx.Exec(ctx, `UPDATE partner_stores SET settings=settings-ARRAY['wechat','qq','email'],version=version+1 WHERE id=$1`, p.Subject); e != nil {
			return e
		}
		return audit(ctx, tx, p, "support-contacts", owner, map[string]any{"version": *in.Version + 1})
	})
}

type Announcement struct {
	Enabled   *bool  `json:"enabled"`
	Placement string `json:"placement"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Version   *int64 `json:"version"`
}

func (s Service) Announcement(ctx context.Context, id string) (map[string]any, error) {
	out, e := rows(ctx, s.Store.DB, "SELECT enabled,placement,title,content,version FROM partner_announcements WHERE partner_id=$1", id)
	if e != nil {
		return nil, e
	}
	if len(out) == 0 {
		return map[string]any{"enabled": false, "placement": "home", "title": "站点公告", "content": "", "version": int64(0)}, nil
	}
	return out[0], nil
}
func (s Service) SaveAnnouncement(ctx context.Context, token string, in Announcement) error {
	if in.Enabled == nil || in.Version == nil || *in.Version < 0 || (in.Placement != "home" && in.Placement != "product" && in.Placement != "both") || !validText(in.Title, 80) || !validText(in.Content, 5000) || (*in.Enabled && (strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Content) == "")) {
		return fault.New(400, "公告内容或版本无效。")
	}
	return s.write(ctx, "partner", token, func(tx pgx.Tx, p auth.Principal) error {
		var current int64
		e := tx.QueryRow(ctx, "SELECT version FROM partner_announcements WHERE partner_id=$1 FOR UPDATE", p.Subject).Scan(&current)
		if errors.Is(e, pgx.ErrNoRows) {
			if *in.Version != 0 {
				return conflict()
			}
			_, e = tx.Exec(ctx, "INSERT INTO partner_announcements(partner_id,enabled,placement,title,content,version) VALUES($1,$2,$3,$4,$5,1)", p.Subject, *in.Enabled, in.Placement, strings.TrimSpace(in.Title), strings.TrimSpace(in.Content))
		} else {
			if e != nil {
				return e
			}
			if current != *in.Version {
				return conflict()
			}
			_, e = tx.Exec(ctx, "UPDATE partner_announcements SET enabled=$1,placement=$2,title=$3,content=$4,version=version+1 WHERE partner_id=$5", *in.Enabled, in.Placement, strings.TrimSpace(in.Title), strings.TrimSpace(in.Content), p.Subject)
		}
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `UPDATE partner_stores SET settings=settings-ARRAY['announcement','announcementTitle','announcementPlacement'],version=version+1 WHERE id=$1`, p.Subject); e != nil {
			return e
		}
		return audit(ctx, tx, p, "partner-announcement", p.Subject, map[string]any{"enabled": *in.Enabled, "version": *in.Version + 1})
	})
}
// PlatformAnnouncement reads the platform storefront announcement (owner "platform").
func (s Service) PlatformAnnouncement(ctx context.Context) (map[string]any, error) {
	out, e := rows(ctx, s.Store.DB, "SELECT enabled,placement,title,content,version FROM platform_announcements WHERE owner='platform'")
	if e != nil {
		return nil, e
	}
	if len(out) == 0 {
		return map[string]any{"enabled": false, "placement": "home", "title": "站点公告", "content": "", "version": int64(0)}, nil
	}
	return out[0], nil
}

// SavePlatformAnnouncement is the admin counterpart of SaveAnnouncement for the platform shop.
func (s Service) SavePlatformAnnouncement(ctx context.Context, token string, in Announcement) error {
	if in.Enabled == nil || in.Version == nil || *in.Version < 0 || (in.Placement != "home" && in.Placement != "product" && in.Placement != "both") || !validText(in.Title, 80) || !validText(in.Content, 5000) || (*in.Enabled && (strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Content) == "")) {
		return fault.New(400, "公告内容或版本无效。")
	}
	return s.write(ctx, "admin", token, func(tx pgx.Tx, p auth.Principal) error {
		var current int64
		e := tx.QueryRow(ctx, "SELECT version FROM platform_announcements WHERE owner='platform' FOR UPDATE").Scan(&current)
		if errors.Is(e, pgx.ErrNoRows) {
			if *in.Version != 0 {
				return conflict()
			}
			_, e = tx.Exec(ctx, "INSERT INTO platform_announcements(owner,enabled,placement,title,content,version) VALUES('platform',$1,$2,$3,$4,1)", *in.Enabled, in.Placement, strings.TrimSpace(in.Title), strings.TrimSpace(in.Content))
		} else {
			if e != nil {
				return e
			}
			if current != *in.Version {
				return conflict()
			}
			_, e = tx.Exec(ctx, "UPDATE platform_announcements SET enabled=$1,placement=$2,title=$3,content=$4,version=version+1,updated_at=now() WHERE owner='platform'", *in.Enabled, in.Placement, strings.TrimSpace(in.Title), strings.TrimSpace(in.Content))
		}
		if e != nil {
			return e
		}
		return audit(ctx, tx, p, "platform-announcement", "platform", map[string]any{"enabled": *in.Enabled, "version": *in.Version + 1})
	})
}
func (s Service) Partner(ctx context.Context, id string) (map[string]any, error) {
	out, e := rows(ctx, s.Store.DB, "SELECT id,name,email,slug,active::int,version,(extract(epoch FROM created_at)*1000)::bigint AS created_at FROM partners WHERE id=$1", id)
	if e != nil {
		return nil, e
	}
	if len(out) == 0 {
		return nil, fault.New(404, "代理不存在。")
	}
	return out[0], nil
}
func (s Service) Audit(ctx context.Context, page int) ([]map[string]any, error) {
	if page < 1 || page > 10000 {
		return nil, fault.New(400, "页码无效。")
	}
	return rows(ctx, s.Store.DB, "SELECT id::text,actor_role,actor_id,action,target,target AS target_id,detail,COALESCE(detail->>'note',action) note,(extract(epoch FROM created_at)*1000)::bigint AS created_at FROM audit_events ORDER BY created_at DESC,id LIMIT 50 OFFSET $1", (page-1)*50)
}
