package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestSession_StartsAgainstFakeRobot registers a session pointed at a fake robot
// HTTP server and verifies that:
//  1. The session contacts the robot at startup (FetchInitialMapID + StartMapListLoop
//     hit the robot's ATOM/FastAPI endpoint immediately on Start).
//  2. Calling HandleWebhook updates state.LastUpdate (ApplyWebhookData always sets it).
func TestSession_StartsAgainstFakeRobot(t *testing.T) {
	var hits int32
	fr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"status":"ok","maps":[],"name":""}`))
	}))
	defer fr.Close()

	srv := &ServerConfig{
		ListenAddr:         ":0",
		AdminToken:         "tok",
		DefaultRobotSecret: "wsec",
		MQTTBroker:         "tcp://127.0.0.1:1",
		MQTTPrefix:         "/uagv/v2",
		Manufacturer:       "atom",
		PublicBaseURL:      "http://localhost:5201",
	}
	st := newTestStore(t)
	mgr := NewSessionManager(srv, st)
	defer mgr.StopAll()

	rec := RobotRecord{
		ID:             "fakebot",
		Serial:         "fakebot",
		AtomBaseURL:    fr.URL,
		FastAPIHTTPURL: fr.URL,
		FastAPIWSURL:   "ws://127.0.0.1:1/ws",
		Source:         "test",
	}
	if err := mgr.Register(rec); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// FetchInitialMapID and StartMapListLoop fire immediately in goroutines;
	// 300ms is more than enough for them to reach the fake server.
	time.Sleep(300 * time.Millisecond)
	if atomic.LoadInt32(&hits) == 0 {
		t.Errorf("expected the session to call the fake robot at startup")
	}

	// ApplyWebhookData sets LastUpdate unconditionally before any field parsing,
	// so any valid JSON object/array will cause LastUpdate to become non-zero.
	// Use a simple status payload that is semantically meaningful.
	sess := mgr.Get("fakebot")
	if sess == nil {
		t.Fatal("session not found after Register")
	}
	sess.HandleWebhook([]byte(`[{"status":"standby"}]`))
	snap := sess.State().Snapshot()
	if snap.LastUpdate.IsZero() {
		t.Errorf("state not updated by webhook: LastUpdate is still zero")
	}
}
