package integration_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gptibo/storefront/internal/domains"
	"gptibo/storefront/internal/httpapi"
	"gptibo/storefront/migrations"
)

type unavailableDomainProvider struct {
	domainProvider
	started chan struct{}
	release chan struct{}
}

func (p *unavailableDomainProvider) Get(ctx context.Context, _ string) (domains.Hostname, error) {
	close(p.started)
	select {
	case <-ctx.Done():
	case <-p.release:
	}
	return domains.Hostname{}, errors.New("provider temporarily unavailable")
}

func TestDomainProviderOutageDoesNotBlockPrimaryStore(t *testing.T) {
	owner, app, identity, _, _ := identityFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	execSQL(t, owner, `INSERT INTO partners(id,slug,name,active) VALUES('a','ABC123','A',true),('b','DEF456','B',true);
INSERT INTO partner_domains(partner_id,hostname,verification,provider_id,state,checked_at) VALUES('a','shop.example.com','verified','cf-domain','active',now())`)
	p := &unavailableDomainProvider{started: make(chan struct{}), release: make(chan struct{})}
	svc := &domains.Service{Store: app, Provider: p, DNS: &domainDNS{txt: "verified", cname: "shops.shop.example.invalid."}, Target: "shops.shop.example.invalid", Zone: "shop.example.invalid", Limit: 100}
	h := httpapi.Router(app, "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), httpapi.Options{Origin: testOrigin, Auth: identity, Domains: svc, TrustedProxies: []string{"127.0.0.1/32"}})
	done := make(chan error, 1)
	go func() { done <- svc.Check(ctx, "a") }()
	select {
	case <-p.started:
	case <-ctx.Done():
		t.Fatal("domain reconciliation did not start")
	}
	// A slow provider must not queue bind requests against the shared DB pool.
	quick, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	err := svc.Bind(quick, "b", "other.example.com")
	stop()
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("binding did not fail promptly while domain reconciliation holds the lock", err)
	}
	if err = app.Heartbeat(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}
	object(t, request(h, "GET", "/healthz", nil, nil), 200)
	object(t, request(h, "GET", "/api/catalog", nil, nil), 200)
	object(t, request(h, "POST", "/api/orders/client", map[string]any{}, nil), 200)
	if _, err = svc.Resolve(ctx, "shop.example.com"); err != nil {
		t.Fatal("healthy binding waited on provider", err)
	}
	close(p.release)
	if err = <-done; err == nil {
		t.Fatal("provider outage went unnoticed")
	}
	if _, err = svc.Resolve(ctx, "shop.example.com"); err != nil {
		t.Fatal("transient outage disabled healthy binding", err)
	}
	// Stale domains fail closed without turning the primary site unhealthy.
	execSQL(t, owner, `UPDATE partner_domains SET checked_at=now()-interval '25 hours'`)
	r := httptest.NewRequest(http.MethodGet, "/api/catalog", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Storefront-Host", "shop.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("stale custom domain returned %d", w.Code)
	}
	object(t, request(h, "GET", "/healthz", nil, nil), 200)
	object(t, request(h, "GET", "/api/catalog", nil, nil), 200)
}

func TestDomainMigrationBusyDatabaseRollsBackAndPreservesSessions(t *testing.T) {
	owner, app, identity, h, mail := identityFixture(t)
	ctx := context.Background()
	cookie := login(t, h, mail, "customer", "buyer@example.test")
	execSQL(t, owner, `INSERT INTO partners(id,slug,name,email,active) VALUES('a','ABC123','A','a@example.test',true);
INSERT INTO partner_payout_accounts(partner_id,phone,name,version) VALUES('a','13800000000','Fixture',7);
INSERT INTO partner_notification_settings(partner_id,order_completed_email,version) VALUES('a',true,3)`)
	// Reconstruct release 020 in this disposable schema, including old sessions,
	// notification preferences and payout accounts that must survive the upgrade.
	execSQL(t, owner, `DROP TABLE partner_ticket_events; DROP TABLE partner_tickets; DROP TABLE partner_ticket_notifications;
DROP FUNCTION coupon_reserved(orders);
DROP TRIGGER partner_store_prices ON partner_prices;
DROP FUNCTION provision_store_prices();
DROP TRIGGER partner_default_store ON partners;
DROP FUNCTION provision_default_store();
DROP TRIGGER partner_supply_quote ON partner_prices;
DROP FUNCTION initialize_supply_quote();
DROP TRIGGER domain_binding_defaults ON partner_domains;
DROP FUNCTION default_domain_binding();
ALTER TABLE partner_debits DROP CONSTRAINT debit_source,DROP COLUMN domain_charge_id;
ALTER TABLE partner_debits ALTER COLUMN refund_id SET NOT NULL;
DROP TABLE domain_charges;
ALTER TABLE partner_prices DROP COLUMN child_cost_cents;
ALTER TABLE orders DROP COLUMN store_id,DROP COLUMN store_name,DROP COLUMN coupon_id,DROP COLUMN coupon_code,DROP COLUMN list_price_cents,DROP COLUMN discount_cents;
DROP TABLE coupon_targets; DROP TABLE partner_coupons;DROP TABLE store_prices;
ALTER TABLE partner_domains DROP COLUMN store_id;
DROP TABLE partner_stores;
DROP VIEW partner_earning_orders;
DROP FUNCTION partner_order_profit(text,text);
ALTER TABLE orders DROP COLUMN upstream_partner_id,DROP COLUMN upstream_cost_cents,DROP COLUMN upstream_payout_id;
ALTER TABLE partners DROP COLUMN parent_id,DROP COLUMN can_recruit;
ALTER TABLE partner_prices DROP COLUMN enabled;
ALTER TABLE partner_debits DROP CONSTRAINT partner_debits_beneficiary_refund;
ALTER TABLE partner_debits ADD CONSTRAINT partner_debits_refund_id_key UNIQUE(refund_id);
DROP TABLE partner_branding; DROP TABLE partner_domains; DROP TABLE telegram_group_moderation; DROP TABLE recharge_credentials; DROP TABLE platform_announcements;
ALTER TABLE auth_challenges DROP COLUMN domain_scope;
ALTER TABLE auth_sessions DROP COLUMN domain_scope;
ALTER TABLE catalog_plans DROP COLUMN supports_warranty;
ALTER TABLE orders DROP COLUMN supports_warranty;
ALTER TABLE partner_order_versions DROP COLUMN hidden_product_ids;
ALTER TABLE recharge_redemptions DROP COLUMN public_message, DROP COLUMN recovery_action;
DELETE FROM storefront_migrations WHERE name > '020_partner_payout_account.sql'`)
	if err := app.VerifyRuntime(ctx); err == nil {
		t.Fatal("new binary accepted unmigrated database")
	}
	lock, err := owner.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	if _, err = lock.Exec(ctx, `LOCK TABLE auth_challenges IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
	started := time.Now()
	err = owner.Migrate(bounded)
	cancel()
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "55P03" || time.Since(started) > 6*time.Second {
		t.Fatal("busy migration must abort on lock timeout", err)
	}
	var count int
	var tableExists bool
	if err = owner.DB.QueryRow(ctx, `SELECT count(*) FROM storefront_migrations`).Scan(&count); err != nil || count != 20 {
		t.Fatal("migration receipt partially applied", count, err)
	}
	if err = owner.DB.QueryRow(ctx, `SELECT to_regclass('partner_domains') IS NOT NULL`).Scan(&tableExists); err != nil || tableExists {
		t.Fatal("failed migration left a partial domain table", err)
	}
	if err = lock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = owner.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Apply the real runtime grants using this test role; no production role is used.
	role := app.DB.Config().ConnConfig.User
	password := app.DB.Config().ConnConfig.Password
	if err = owner.GrantRuntime(ctx, role, password); err != nil {
		t.Fatal(err)
	}
	if err = app.VerifyRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	if err = owner.Migrate(ctx); err != nil {
		t.Fatal("repeat upgrade failed", err)
	}
	files, err := migrations.Files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	if err = app.DB.QueryRow(ctx, `SELECT count(*) FROM storefront_migrations`).Scan(&count); err != nil || count != len(files) {
		t.Fatal("expected all embedded migrations", count, err)
	}
	var preserved bool
	if err = app.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM partner_payout_accounts WHERE partner_id='a' AND phone='13800000000' AND name='Fixture' AND version=7)
AND EXISTS(SELECT 1 FROM partner_notification_settings WHERE partner_id='a' AND order_completed_email AND version=3)`).Scan(&preserved); err != nil || !preserved {
		t.Fatal("upgrade changed payout account or notification settings", err)
	}
	if _, err = identity.Require(ctx, app.DB, "customer", cookie.Value); err != nil {
		t.Fatal("migration invalidated primary-domain session", err)
	}
	if err = app.Heartbeat(ctx, "fixture-worker"); err != nil {
		t.Fatal(err)
	}
	object(t, request(h, "GET", "/healthz", nil, nil), 200)
	object(t, request(h, "GET", "/api/catalog", nil, nil), 200)
	// Domain management is visible but safely disabled without SaaS configuration.
	if err = (&domains.Service{Store: app}).Bind(ctx, "unused", "shop.example.com"); err == nil {
		t.Fatal("disabled feature accepted a binding")
	}
}
