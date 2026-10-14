package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gptibo/storefront/internal/catalog"
	"gptibo/storefront/internal/httpapi"
	"gptibo/storefront/internal/store"
	"gptibo/storefront/internal/worker"
)

func fixture(t *testing.T, migrate bool) (*store.Store, *store.Store) {
	t.Helper()
	dsn := os.Getenv("STOREFRONT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("STOREFRONT_TEST_DATABASE_URL required for real PostgreSQL integration tests")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	if (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") || !strings.HasSuffix(u.Path, "_test") {
		t.Fatal("tests require a loopback database named *_test")
	}
	ctx := context.Background()
	root, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("test database unavailable")
	}
	schema := "sf_test_" + strings.ToLower(rand.Text())
	role := schema + "_app"
	if _, err = root.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		root.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer root.Close()
		if _, err := root.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
		var exists bool
		if err := root.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)", role).Scan(&exists); err != nil {
			t.Error(err)
		}
		if exists {
			if _, err := root.Exec(ctx, "DROP OWNED BY "+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Error(err)
			}
			if _, err := root.Exec(ctx, "DROP ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Error(err)
			}
		}
	})
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	owner, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.DB.Close)
	if !migrate {
		return owner, nil
	}
	if err = owner.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	password := rand.Text() + rand.Text()
	if err = owner.GrantRuntime(ctx, role, password); err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	app, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.DB.Close)
	if err = app.VerifyRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	return owner, app
}

func execSQL(t *testing.T, s *store.Store, sql string, args ...any) {
	t.Helper()
	if _, err := s.DB.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogIsolationAndImmediateUpdates(t *testing.T) {
	owner, app := fixture(t, true)
	execSQL(t, owner, `INSERT INTO catalog_plans(id,product_id,name,price_cents,enabled,partner_enabled) VALUES('plus','chatgpt','Plus',16900,true,true);
 INSERT INTO partners(id,slug,name,active) VALUES('a','ABC123','A',true),('b','DEF456','B',true);
 INSERT INTO partner_prices(partner_id,plan,price_cents,cost_cents) VALUES('a','plus',18000,10000),('b','plus',19000,11000);
 INSERT INTO support_contacts(owner,wechat) VALUES('platform','main-contact'),('a','contact-a'),('b','contact-b');
 INSERT INTO partner_announcements(partner_id,enabled,placement,title,content) VALUES('a',true,'home','A notice','Only A');
 INSERT INTO partner_product_order(partner_id,product_id,position) VALUES('a','x-premium',0),('a','retired',1);`)
	gin.SetMode(gin.TestMode)
	var logs bytes.Buffer
	handler := httpapi.Router(app, "test-worker", slog.New(slog.NewJSONHandler(&logs, nil)))
	for _, tc := range []struct {
		slug    string
		price   int64
		contact string
	}{{"", 16900, "main-contact"}, {"ABC123", 18000, "contact-a"}, {"DEF456", 19000, "contact-b"}} {
		t.Run(tc.slug, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/catalog?partner="+tc.slug+"&secret=never-log-this", nil))
			if w.Code != 200 {
				t.Fatalf("status %d", w.Code)
			}
			var got catalog.Response
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Plans) != 1 || got.Plans[0].PriceCents != tc.price || got.SupportContacts.Wechat != tc.contact {
				t.Fatal("cross-shop or incorrect catalogue")
			}
			if got.CheckoutEnabled || got.TestingEnabled || got.Plans[0].Enabled != 0 {
				t.Fatal("unfinished checkout advertised as available")
			}
			if strings.Contains(w.Body.String(), "cost_cents") || strings.Contains(w.Body.String(), "partner_id") {
				t.Fatal("private fields leaked")
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Server-Timing") == "" {
				t.Fatal("missing catalogue headers")
			}
			if tc.slug == "ABC123" {
				if got.Announcement == nil || got.Announcement.Content != "Only A" || len(got.ProductOrder) != 8 || got.ProductOrder[0] != "x-premium" {
					t.Fatal("partner presentation settings lost")
				}
			} else if got.Announcement != nil {
				t.Fatal("cross-shop announcement")
			}
		})
	}
	if strings.Contains(logs.String(), "never-log-this") {
		t.Fatal("query string logged")
	}
	execSQL(t, owner, "UPDATE partner_prices SET price_cents=18500,version=version+1 WHERE partner_id='a'")
	got, err := (catalog.Service{Store: app}).Read(context.Background(), "ABC123")
	if err != nil || got.Plans[0].PriceCents != 18500 || got.Plans[0].Version != 1 {
		t.Fatal("stale price")
	}
	execSQL(t, owner, "UPDATE partners SET active=false WHERE id='a'")
	for _, slug := range []string{"ABC123", "MISSING", "' OR 1=1 --"} {
		_, err = (catalog.Service{Store: app}).Read(context.Background(), slug)
		if err != catalog.ErrShopUnavailable {
			t.Fatal("inactive/unknown/invalid shop did not fail closed")
		}
	}
	app.DB.Close()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/catalog", nil))
	if w.Code != 503 || strings.Contains(w.Body.String(), "pool") {
		t.Fatal("database failure not safely reported")
	}
}

func TestMigrationsAndRestrictedRuntime(t *testing.T) {
	owner, app := fixture(t, true)
	ctx := context.Background()
	if err := owner.Migrate(ctx); err != nil {
		t.Fatal("repeat migration failed", err)
	}
	if err := owner.VerifyRuntime(ctx); err == nil {
		t.Fatal("privileged runtime accepted")
	}
	// catalog_plans, partner_prices and store_prices legitimately allow DELETE for
	// admin-initiated plan removal; audit and migration history stay immutable.
	for _, sql := range []string{"DELETE FROM storefront_migrations", "CREATE TABLE forbidden(id int)", "DELETE FROM audit_events", "UPDATE audit_events SET action='tamper'", "DELETE FROM worker_heartbeats", "DELETE FROM orders"} {
		if _, err := app.DB.Exec(ctx, sql); err == nil {
			t.Fatalf("runtime unexpectedly allowed: %s", sql)
		}
	}
	if err := app.Heartbeat(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	execSQL(t, owner, "UPDATE storefront_migrations SET checksum='changed'")
	if err := app.Verify(ctx); err == nil {
		t.Fatal("changed migration accepted")
	}
	if err := owner.Migrate(ctx); err == nil {
		t.Fatal("migration history silently rewritten")
	}
}

func TestRejectUnrelatedDatabase(t *testing.T) {
	owner, _ := fixture(t, false)
	execSQL(t, owner, "CREATE TABLE supply_wallets(id int)")
	if err := owner.Migrate(context.Background()); err == nil {
		t.Fatal("foreign schema accepted")
	}
	var exists bool
	if err := owner.DB.QueryRow(context.Background(), "SELECT to_regclass('storefront_migrations') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatal("failed migration left partial state")
	}
}

func TestNewerSchemaBlocksOldBinary(t *testing.T) {
	owner, app := fixture(t, true)
	execSQL(t, owner, "INSERT INTO storefront_migrations(name,checksum) VALUES('999_future.sql','future')")
	if err := app.Verify(context.Background()); err == nil {
		t.Fatal("newer schema accepted")
	}
	if err := owner.Migrate(context.Background()); err == nil {
		t.Fatal("downgrade accepted")
	}
}

func TestHealthAndWorkerLifecycle(t *testing.T) {
	owner, app := fixture(t, true)
	gin.SetMode(gin.TestMode)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := httpapi.Router(app, "test-worker", logger)
	get := func(path, method string) int {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w.Code
	}
	if get("/livez", "GET") != 200 || get("/healthz", "GET") != 503 {
		t.Fatal("liveness/readiness confused")
	}
	if get("/api/orders", "POST") != http.StatusForbidden || get("/api/catalog", "POST") != http.StatusMethodNotAllowed {
		t.Fatal("unexpected write route")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx, app, "test-worker", logger) }()
	deadline := time.Now().Add(3 * time.Second)
	for !app.WorkerReady(context.Background(), "test-worker") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if get("/healthz", "GET") != 200 {
		t.Fatal("worker failed to become ready")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker ignored cancellation")
	}
	execSQL(t, owner, "UPDATE worker_heartbeats SET updated_at=now()-interval '46 seconds'")
	if get("/healthz", "GET") != 503 {
		t.Fatal("stale worker reported healthy")
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/catalog", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"plans":[]`) {
		t.Fatal("empty catalogue not compatible")
	}
}
