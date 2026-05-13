package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestAPI(t *testing.T) (*APIServer, *SessionManager, *Store) {
	t.Helper()
	srv := &ServerConfig{ListenAddr: ":0", AdminToken: "tok", DefaultRobotSecret: "wsec",
		MQTTBroker: "tcp://127.0.0.1:1", MQTTPrefix: "/uagv/v2", Manufacturer: "atom", PublicBaseURL: "http://localhost"}
	st := newTestStore(t)
	mgr := NewSessionManager(srv, st)
	t.Cleanup(mgr.StopAll)
	api := NewAPIServer(srv, mgr, st)
	return api, mgr, st
}

func TestAPI_Healthz(t *testing.T) {
	t.Parallel()
	api, _, _ := newTestAPI(t)
	rec := httptest.NewRecorder()
	api.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "robots") {
		t.Fatalf("healthz: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAPI_AdminAuth(t *testing.T) {
	t.Parallel()
	api, _, _ := newTestAPI(t)
	rec := httptest.NewRecorder()
	api.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/robots", nil))
	if rec.Code != 401 {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/robots", nil)
	req.Header.Set("Authorization", "Bearer tok")
	api.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200 with token, got %d", rec.Code)
	}
}

func TestAPI_AdminRegisterThenWebhook(t *testing.T) {
	t.Parallel()
	api, mgr, _ := newTestAPI(t)
	body := `{"id":"r1","serial":"r1","atomBaseURL":"http://127.0.0.1:18080","fastapiHTTPURL":"http://127.0.0.1:18000","fastapiWSURL":"ws://127.0.0.1:18000/ws","webhookSecret":"abc"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/robots", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	api.mux.ServeHTTP(rec, req)
	if rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	if mgr.Get("r1") == nil {
		t.Fatalf("session r1 not created")
	}
	rec = httptest.NewRecorder()
	wreq := httptest.NewRequest("POST", "/webhook/r1", strings.NewReader(`[{"foo":1}]`))
	wreq.Header.Set("X-Webhook-Secret", "WRONG")
	api.mux.ServeHTTP(rec, wreq)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for bad secret, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	wreq = httptest.NewRequest("POST", "/webhook/r1", strings.NewReader(`[{"foo":1}]`))
	wreq.Header.Set("X-Webhook-Secret", "abc")
	api.mux.ServeHTTP(rec, wreq)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d %s", rec.Code, rec.Body.String())
	}

	// Missing X-Webhook-Secret header → accepted (fail open for robots that don't send it).
	rec = httptest.NewRecorder()
	wreq = httptest.NewRequest("POST", "/webhook/r1", strings.NewReader(`[{"foo":1}]`))
	api.mux.ServeHTTP(rec, wreq)
	if rec.Code != 200 {
		t.Fatalf("expected 200 with no secret header, got %d %s", rec.Code, rec.Body.String())
	}

	// Trailing-slash webhook URL → robot appends a data-source segment
	// (".../webhook/r1/imu"); only the first segment is the robot key, so these
	// route to r1 and don't spawn new sessions.
	for _, p := range []string{"/webhook/r1/imu", "/webhook/r1/encoder", "/webhook/r1/"} {
		rec = httptest.NewRecorder()
		wreq = httptest.NewRequest("POST", p, strings.NewReader(`[{"foo":1}]`))
		wreq.Header.Set("X-Webhook-Secret", "abc")
		api.mux.ServeHTTP(rec, wreq)
		if rec.Code != 200 {
			t.Fatalf("POST %s: expected 200, got %d %s", p, rec.Code, rec.Body.String())
		}
	}
}

func TestAPI_WebhookUnmanagedRobotDropped(t *testing.T) {
	t.Parallel()
	api, mgr, st := newTestAPI(t)
	for _, p := range []string{"/webhook/newbot", "/webhook/r1imu", "/webhook/newbot/encoder"} {
		rec := httptest.NewRecorder()
		wreq := httptest.NewRequest("POST", p, strings.NewReader(`[{"foo":1}]`))
		wreq.RemoteAddr = "5.6.7.8:55555"
		api.mux.ServeHTTP(rec, wreq)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("POST %s: expected 404, got %d %s", p, rec.Code, rec.Body.String())
		}
	}
	if mgr.Get("newbot") != nil || mgr.Get("r1imu") != nil {
		t.Fatal("unmanaged webhook must not create a session")
	}
	if got, _ := st.GetRobot("newbot"); got.ID != "" {
		t.Fatalf("unmanaged webhook must not persist a record, got %+v", got)
	}
}

func TestAPI_AdminRobot_GetAndDelete(t *testing.T) {
	t.Parallel()
	api, mgr, st := newTestAPI(t)
	_ = st.UpsertRobot(RobotRecord{ID: "r9", Serial: "r9", Status: "online", Source: "test",
		AtomBaseURL: "http://1.2.3.4:8080"})

	// GET /admin/robots/r9
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/robots/r9", nil)
	req.Header.Set("Authorization", "Bearer tok")
	api.mux.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "1.2.3.4") {
		t.Fatalf("GET single robot: %d %s", rec.Code, rec.Body.String())
	}

	// GET unknown → 404
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/admin/robots/nope", nil)
	req.Header.Set("Authorization", "Bearer tok")
	api.mux.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Errorf("GET unknown robot: %d, want 404", rec.Code)
	}

	// GET without auth → 401
	rec = httptest.NewRecorder()
	api.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/robots/r9", nil))
	if rec.Code != 401 {
		t.Errorf("GET without auth: %d, want 401", rec.Code)
	}

	// DELETE /admin/robots/r9
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("DELETE", "/admin/robots/r9", nil)
	req.Header.Set("Authorization", "Bearer tok")
	api.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("DELETE robot: %d %s", rec.Code, rec.Body.String())
	}
	_ = mgr

	// Unsupported method → 405
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("PUT", "/admin/robots/r9", nil)
	req.Header.Set("Authorization", "Bearer tok")
	api.mux.ServeHTTP(rec, req)
	if rec.Code != 405 {
		t.Errorf("PUT robot: %d, want 405", rec.Code)
	}
}

func TestAPI_Webhook_GetAndMethodNotAllowed(t *testing.T) {
	t.Parallel()
	api, _, _ := newTestAPI(t)
	// GET /webhook/x → "ok" text
	rec := httptest.NewRecorder()
	api.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/webhook/x", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("GET webhook: %d %s", rec.Code, rec.Body.String())
	}
	// PUT /webhook/x → 405
	rec = httptest.NewRecorder()
	api.mux.ServeHTTP(rec, httptest.NewRequest("PUT", "/webhook/x", nil))
	if rec.Code != 405 {
		t.Errorf("PUT webhook: %d, want 405", rec.Code)
	}
}

func TestAPI_Healthz_WithRobot(t *testing.T) {
	t.Parallel()
	api, mgr, _ := newTestAPI(t)
	_ = mgr.Register(RobotRecord{ID: "h1", Serial: "h1",
		AtomBaseURL: "http://127.0.0.1:18080", FastAPIHTTPURL: "http://127.0.0.1:18000",
		FastAPIWSURL: "ws://127.0.0.1:18000/ws", Source: "test"})
	sess := mgr.Get("h1")
	if sess != nil {
		sess.State().mu.Lock()
		sess.State().LastUpdate = time.Now()
		sess.State().mu.Unlock()
	}
	rec := httptest.NewRecorder()
	api.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"h1"`) {
		t.Errorf("healthz with robot: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"dataFresh":true`) {
		t.Errorf("healthz should report dataFresh=true for fresh robot: %s", rec.Body.String())
	}
}

func TestApplyWebhookPayload(t *testing.T) {
	t.Parallel()
	lg := log.New(io.Discard, "", 0)

	t.Run("array of items", func(t *testing.T) {
		state := NewRobotState()
		applyWebhookPayload(state, nil, []byte(`[{"route_status":{"status":"delivering"}},{"battery_level":55}]`), lg)
		snap := state.Snapshot()
		if snap.Status != "delivering" || snap.BatteryPercent != 55 {
			t.Errorf("array payload not applied: %+v", snap)
		}
	})

	t.Run("single object", func(t *testing.T) {
		state := NewRobotState()
		applyWebhookPayload(state, nil, []byte(`{"battery":42}`), lg)
		if state.Snapshot().BatteryPercent != 42 {
			t.Errorf("single object payload not applied")
		}
	})

	t.Run("empty body is a no-op", func(t *testing.T) {
		state := NewRobotState()
		applyWebhookPayload(state, nil, nil, lg)
		if !state.Snapshot().LastUpdate.IsZero() {
			t.Errorf("empty payload should be a no-op")
		}
	})

	t.Run("invalid JSON is a no-op", func(t *testing.T) {
		state := NewRobotState()
		applyWebhookPayload(state, nil, []byte(`{not json`), lg)
		applyWebhookPayload(state, nil, []byte(`[not json`), lg)
		if !state.Snapshot().LastUpdate.IsZero() {
			t.Errorf("invalid JSON should be a no-op")
		}
	})

	t.Run("map_loaded triggers async map query", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"current_map_name":"mapZ"}`))
		}))
		defer srv.Close()
		state := NewRobotState()
		applyWebhookPayload(state, testCfg(srv), []byte(`[{"route_status":{"status":"map_loaded"}}]`), lg)
		// async — poll briefly.
		for i := 0; i < 50; i++ {
			if state.Snapshot().MapID == "mapZ" {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if state.Snapshot().MapID != "mapZ" {
			t.Errorf("map_loaded should trigger map query; MapID=%q", state.Snapshot().MapID)
		}
	})
}
