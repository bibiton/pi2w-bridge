package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	api, _, _ := newTestAPI(t)
	rec := httptest.NewRecorder()
	api.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "robots") {
		t.Fatalf("healthz: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAPI_AdminAuth(t *testing.T) {
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
}

func TestAPI_WebhookUnknownRobotProvisional(t *testing.T) {
	api, mgr, st := newTestAPI(t)
	rec := httptest.NewRecorder()
	wreq := httptest.NewRequest("POST", "/webhook/newbot", strings.NewReader(`[{"foo":1}]`))
	wreq.RemoteAddr = "5.6.7.8:55555"
	api.mux.ServeHTTP(rec, wreq)
	if rec.Code != http.StatusAccepted && rec.Code != 200 {
		t.Fatalf("expected 202/200 for provisional, got %d %s", rec.Code, rec.Body.String())
	}
	if mgr.Get("newbot") == nil {
		t.Fatalf("provisional session not created")
	}
	got, _ := st.GetRobot("newbot")
	if got.Status != "provisional" {
		t.Errorf("status = %q, want provisional", got.Status)
	}
}
