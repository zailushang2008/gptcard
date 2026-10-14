package catalog

import (
	"context"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
	"gptibo/storefront/internal/store"
)

var ErrShopUnavailable = errors.New("shop unavailable")
var slugPattern = regexp.MustCompile(`^(?:[A-Z0-9]{6}|[A-Z]{8}|[a-z0-9][a-z0-9-]{2,31})$`)
var productOrder = []string{"chatgpt", "gemini", "duolingo", "x-premium", "gmail", "claude", "grok", "codex-sms"}

func NormalizeProductOrder(saved []string) []string {
	known := map[string]bool{}
	for _, id := range productOrder {
		known[id] = true
	}
	out := []string{}
	for _, id := range append(append([]string{}, saved...), productOrder...) {
		if known[id] {
			out = append(out, id)
			delete(known, id)
		}
	}
	return out
}

type Plan struct {
	HasCoupons           bool   `json:"has_coupons"`
	SupportsWarranty     bool   `json:"supports_warranty"`
	Cost                 *int64 `json:"-"`
	ID                   string `json:"id"`
	ProductID            string `json:"product_id"`
	Name                 string `json:"name"`
	PriceCents           int64  `json:"price_cents"`
	Enabled              int    `json:"enabled"`
	PartnerEnabled       int    `json:"partner_enabled"`
	RenewalOnly          int    `json:"renewal_only"`
	PurchaseNote         string `json:"purchase_note"`
	DeliveryInstructions string `json:"delivery_instructions"`
	Version              int64  `json:"version"`
	UpdatedAt            int64  `json:"updated_at"`
}
type Contacts struct {
	Wechat string `json:"wechat"`
	QQ     string `json:"qq"`
	Email  string `json:"email"`
}
type Partner struct {
	PageTitle   string `json:"pageTitle"`
	Headline    string `json:"headline"`
	Description string `json:"description"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	BrandName   string `json:"brandName"`
	Logo        string `json:"logo"`
}
type Announcement struct {
	Enabled   bool   `json:"enabled"`
	Placement string `json:"placement"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Version   int64  `json:"version"`
}
type Response struct {
	CompletedOrdersByProduct map[string]int64 `json:"completedOrdersByProduct"`
	HiddenProductIDs         []string         `json:"hiddenProductIds"`
	PaymentMethods           []PaymentMethod  `json:"paymentMethods"`
	Plans                    []Plan           `json:"plans"`
	SupportContacts          Contacts         `json:"supportContacts"`
	Announcement             *Announcement    `json:"announcement"`
	Partner                  *Partner         `json:"partner,omitempty"`
	ProductOrder             []string         `json:"productOrder,omitempty"`
	Mode                     string           `json:"mode"`
	CheckoutEnabled          bool             `json:"checkoutEnabled"`
	TestingEnabled           bool             `json:"testingEnabled"`
}

type PaymentMethod struct {
	FeeBPS int64  `json:"feeBps"`
	Method string `json:"method"`
	Name   string `json:"name"`
}

type Service struct {
	Store           *store.Store
	CompletedOrders *CompletedOrderCache
	Testing         bool
	Availability    func(context.Context, []Plan) []Plan
}

func (s Service) Read(ctx context.Context, slug string) (Response, error) {
	out := Response{Plans: []Plan{}, HiddenProductIDs: []string{}, Mode: "live"}
	if s.Testing {
		out.Mode = "demo"
		out.TestingEnabled = true
		out.CheckoutEnabled = true
	}
	if slug != "" && !slugPattern.MatchString(slug) {
		return out, ErrShopUnavailable
	}
	// Current shop settings and prices use one consistent snapshot; only counts are cached.
	tx, err := s.Store.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	owner := "platform"
	var shopID string
	var slot int
	var shopSettings map[string]string
	if slug != "" {
		p := &Partner{}
		err = tx.QueryRow(ctx, "SELECT p.id,s.name,s.slug,COALESCE(b.name,''),COALESCE(b.logo,''),COALESCE(b.page_title,''),COALESCE(b.headline,''),COALESCE(b.description,''),s.id,s.settings,s.slot FROM partner_stores s JOIN partners p ON p.id=s.partner_id LEFT JOIN partner_branding b ON b.partner_id=p.id AND s.slot=1 WHERE s.slug=$1 AND s.active AND p.active AND (p.parent_id IS NULL OR EXISTS(SELECT 1 FROM partners parent WHERE parent.id=p.parent_id AND parent.active))", slug).Scan(&owner, &p.Name, &p.Slug, &p.BrandName, &p.Logo, &p.PageTitle, &p.Headline, &p.Description, &shopID, &shopSettings, &slot)
		if errors.Is(err, pgx.ErrNoRows) {
			return out, ErrShopUnavailable
		}
		if err != nil {
			return out, err
		}
		err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT hidden_product_ids FROM partner_order_versions WHERE partner_id=$1),'[]'::jsonb)`, owner).Scan(&out.HiddenProductIDs)
		if err != nil {
			return out, err
		}
		if v, ok := shopSettings["brandName"]; ok {
			p.BrandName = v
		}
		if v, ok := shopSettings["logo"]; ok {
			p.Logo = v
		}
		if v, ok := shopSettings["pageTitle"]; ok {
			p.PageTitle = v
		}
		if v, ok := shopSettings["headline"]; ok {
			p.Headline = v
		}
		if v, ok := shopSettings["description"]; ok {
			p.Description = v
		}
		if slot != 1 {
			out.HiddenProductIDs = []string{}
		}
		out.Partner = p
	}
	sql := `SELECT c.id,c.product_id,c.name,c.price_cents,c.partner_enabled::int,c.renewal_only::int,
 c.purchase_note,c.delivery_instructions,c.version,(extract(epoch FROM c.updated_at)*1000)::bigint,c.enabled,NULL::bigint,c.supports_warranty,false
 FROM catalog_plans c ORDER BY c.sort_order,c.id`
	var args []any
	if out.Partner != nil {
		sql = `SELECT c.id,c.product_id,c.name,COALESCE(sp.price_cents,p.price_cents),c.partner_enabled::int,c.renewal_only::int,
 c.purchase_note,''::text,p.version,(extract(epoch FROM c.updated_at)*1000)::bigint,c.enabled AND c.partner_enabled AND p.enabled AND COALESCE(sp.enabled,true) AND COALESCE(sp.price_cents,p.price_cents)>=p.cost_cents AND NOT EXISTS(SELECT 1 FROM partners child JOIN partner_prices parent ON parent.partner_id=child.parent_id AND parent.plan=p.plan WHERE child.id=p.partner_id AND (NOT parent.enabled OR parent.cost_cents>p.cost_cents)),p.cost_cents,c.supports_warranty,
 EXISTS(SELECT 1 FROM partner_coupons coupon JOIN coupon_targets target ON target.coupon_id=coupon.id
 WHERE coupon.partner_id=$1 AND target.store_id=$2 AND target.plan=c.id
 AND coupon.active AND coupon.starts_at<=now() AND coupon.ends_at>now()
 AND COALESCE(sp.price_cents,p.price_cents)-coupon.discount_cents>=p.cost_cents
 AND COALESCE(sp.price_cents,p.price_cents)-coupon.discount_cents>0
 AND (SELECT count(*) FROM orders o WHERE o.coupon_id=coupon.id AND coupon_reserved(o))<coupon.max_uses)
 FROM catalog_plans c JOIN partner_prices p ON p.plan=c.id LEFT JOIN store_prices sp ON sp.store_id=$2 AND sp.plan=c.id WHERE p.partner_id=$1 ORDER BY c.sort_order,c.id`
		args = []any{owner, shopID}
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return out, err
	}
	hidden := map[string]bool{}
	for _, id := range out.HiddenProductIDs {
		hidden[id] = true
	}
	for rows.Next() {
		var p Plan
		var enabled bool
		if err = rows.Scan(&p.ID, &p.ProductID, &p.Name, &p.PriceCents, &p.PartnerEnabled, &p.RenewalOnly, &p.PurchaseNote, &p.DeliveryInstructions, &p.Version, &p.UpdatedAt, &enabled, &p.Cost, &p.SupportsWarranty, &p.HasCoupons); err != nil {
			rows.Close()
			return out, err
		}
		p.HasCoupons = p.HasCoupons && enabled
		if hidden[p.ProductID] {
			continue
		}
		if s.Testing && enabled && (p.ID == "plus" || p.ID == "pro-5x" || p.ID == "pro-20x" || p.ID == "pro-25x" || p.ID == "codex-sms-once") {
			p.Enabled = 1
		}
		if !s.Testing && s.Availability != nil && enabled {
			p.Enabled = 1
		}
		out.Plans = append(out.Plans, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, "SELECT wechat,qq,email FROM support_contacts WHERE owner=$1", owner).Scan(&out.SupportContacts.Wechat, &out.SupportContacts.QQ, &out.SupportContacts.Email)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if out.Partner == nil {
		a := &Announcement{Enabled: true}
		err = tx.QueryRow(ctx, "SELECT placement,title,content,version FROM platform_announcements WHERE owner='platform' AND enabled").Scan(&a.Placement, &a.Title, &a.Content, &a.Version)
		if err == nil {
			out.Announcement = a
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return out, err
		}
	}
	if out.Partner != nil {
		a := &Announcement{Enabled: true}
		err = tx.QueryRow(ctx, "SELECT placement,title,content,version FROM partner_announcements WHERE partner_id=$1 AND enabled", owner).Scan(&a.Placement, &a.Title, &a.Content, &a.Version)
		if err == nil {
			out.Announcement = a
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return out, err
		}
		rows, err = tx.Query(ctx, "SELECT product_id FROM partner_product_order WHERE partner_id=$1 ORDER BY position", owner)
		if err != nil {
			return out, err
		}
		known := map[string]bool{}
		seen := map[string]bool{}
		for _, id := range productOrder {
			known[id] = true
		}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return out, err
			}
			if known[id] && !seen[id] {
				out.ProductOrder = append(out.ProductOrder, id)
				seen[id] = true
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
		for _, id := range productOrder {
			if !seen[id] {
				out.ProductOrder = append(out.ProductOrder, id)
			}
		}
	}
	if out.Partner != nil && slot != 1 {
		out.SupportContacts = Contacts{}
		out.Announcement = nil
	}
	if out.Partner != nil && len(shopSettings) > 0 {
		if v, ok := shopSettings["wechat"]; ok {
			out.SupportContacts.Wechat = v
		}
		if v, ok := shopSettings["qq"]; ok {
			out.SupportContacts.QQ = v
		}
		if v, ok := shopSettings["email"]; ok {
			out.SupportContacts.Email = v
		}
		if v, ok := shopSettings["announcement"]; ok {
			out.Announcement = nil
			if v != "" {
				title := shopSettings["announcementTitle"]
				if title == "" {
					title = "商店公告"
				}
				placement := shopSettings["announcementPlacement"]
				if placement == "" {
					placement = "both"
				}
				out.Announcement = &Announcement{Enabled: true, Placement: placement, Title: title, Content: v}
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return out, err
	}
	// Release the catalog transaction before a cache miss acquires a connection.
	// A statistics failure must not disable prices or checkout, or fabricate zeros.
	var counts map[string]int64
	if s.CompletedOrders != nil {
		counts, err = s.CompletedOrders.Read(ctx)
	} else {
		counts, err = readCompletedOrders(ctx, s.Store)
	}
	if err == nil {
		out.CompletedOrdersByProduct = map[string]int64{}
		for _, p := range out.Plans {
			out.CompletedOrdersByProduct[p.ProductID] = counts[p.ProductID]
		}
	}
	if !s.Testing && s.Availability != nil {
		out.Plans = s.Availability(ctx, out.Plans)
		for _, p := range out.Plans {
			out.CheckoutEnabled = out.CheckoutEnabled || p.Enabled == 1
		}
	}
	return out, nil
}
