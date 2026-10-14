package integration_test

import (
	"context"
	"testing"
	"time"
)

func TestPlanDeleteLifecycle(t *testing.T) {
	owner, _, _, h, mail := identityFixture(t)
	admin := login(t, h, mail, "admin", "")
	plan := func(id string) map[string]any {
		return map[string]any{"id": id, "productId": "chatgpt", "name": "Plus", "priceCents": 16000, "defaultCostCents": 10000, "enabled": true, "partnerEnabled": true, "note": "fixture"}
	}
	object(t, request(h, "POST", "/api/admin/plans", plan("plus"), admin), 200)
	object(t, request(h, "POST", "/api/admin/plans", plan("pro-5x"), admin), 200)
	created := object(t, request(h, "POST", "/api/admin/partners", map[string]any{"email": "plan-delete@example.test"}, admin), 200)
	partnerID := created["id"].(string)

	if r := request(h, "POST", "/api/admin/plans/plus/delete", map[string]any{"note": ""}, admin); r.Code != 400 {
		t.Fatal("delete without note accepted", r.Code)
	}
	if r := request(h, "POST", "/api/admin/plans/ghost/delete", map[string]any{"note": "x"}, admin); r.Code != 404 {
		t.Fatal("delete of unknown plan did not 404", r.Code)
	}

	now := time.Now().UnixMilli()
	execSQL(t, owner, `INSERT INTO orders(id,owner_hash,idempotency_key,plan,plan_name,price_cents,cost_cents,partner_id,mode,payment_status,status,created_at,updated_at,last_event_id) VALUES('delete-order','fixture','fixture','plus','Plus',16000,10000,$1,'live','paid','success',$2,$2,'fixture')`, partnerID, now)
	if r := request(h, "POST", "/api/admin/plans/plus/delete", map[string]any{"note": "has orders"}, admin); r.Code != 409 {
		t.Fatal("plan with orders must not be deletable", r.Code)
	}

	object(t, request(h, "POST", "/api/admin/plans/pro-5x/delete", map[string]any{"note": "unused"}, admin), 200)
	var remaining bool
	if e := owner.DB.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM catalog_plans WHERE id='pro-5x') OR EXISTS(SELECT 1 FROM partner_prices WHERE plan='pro-5x')`).Scan(&remaining); e != nil || remaining {
		t.Fatal("deleted plan left catalog or partner price rows", e)
	}
	plans := object(t, request(h, "GET", "/api/admin/plans", nil, admin), 200)["plans"].([]any)
	if len(plans) != 1 || plans[0].(map[string]any)["id"] != "plus" {
		t.Fatal("plan list wrong after delete", plans)
	}
}

func TestPlatformAnnouncementContract(t *testing.T) {
	_, _, _, h, mail := identityFixture(t)
	admin := login(t, h, mail, "admin", "")
	empty := object(t, request(h, "GET", "/api/catalog", nil, nil), 200)
	if empty["announcement"] != nil {
		t.Fatal("platform catalog announced before any announcement was saved")
	}
	current := object(t, request(h, "GET", "/api/admin/announcement", nil, admin), 200)
	if current["version"] != float64(0) {
		t.Fatal("default announcement version wrong", current)
	}
	in := map[string]any{"enabled": true, "placement": "both", "title": "维护通知", "content": "今晚升级", "version": 0}
	object(t, request(h, "POST", "/api/admin/announcement", in, admin), 200)
	if r := request(h, "POST", "/api/admin/announcement", in, admin); r.Code != 409 {
		t.Fatal("stale announcement version accepted", r.Code)
	}
	live := object(t, request(h, "GET", "/api/catalog", nil, nil), 200)["announcement"].(map[string]any)
	if live["title"] != "维护通知" || live["content"] != "今晚升级" {
		t.Fatal("platform catalog missing saved announcement", live)
	}
	in["enabled"] = false
	in["version"] = 1
	object(t, request(h, "POST", "/api/admin/announcement", in, admin), 200)
	if off := object(t, request(h, "GET", "/api/catalog", nil, nil), 200); off["announcement"] != nil {
		t.Fatal("disabled announcement still visible", off["announcement"])
	}
}
