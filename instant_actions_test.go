package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func waitTick() { time.Sleep(10 * time.Millisecond) }

func newTestActionHandler(t *testing.T, cfg *Config) (*InstantActionHandler, *RobotState, *MQTTBridge) {
	t.Helper()
	state := NewRobotState()
	ms := NewMapService(cfg)
	ws := NewRobotWSClient(cfg, state)
	mb := NewMQTTBridge(cfg, state, ms, ws, nil)
	h := NewInstantActionHandler(cfg, state, ms, mb, ws)
	return h, state, mb
}

func TestInstantAction_HandleNavigate(t *testing.T) {
	t.Parallel()
	var posted int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/service/control/commands" && r.Method == http.MethodPost {
			atomic.AddInt32(&posted, 1)
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	h, _, _ := newTestActionHandler(t, testCfg(srv))

	if err := h.handleNavigate("a1", map[string]string{"target": "C01"}); err != nil {
		t.Fatalf("handleNavigate: %v", err)
	}
	if atomic.LoadInt32(&posted) == 0 {
		t.Errorf("handleNavigate should POST a delivery command")
	}
	// missing target → error
	if err := h.handleNavigate("a2", map[string]string{}); err == nil {
		t.Errorf("handleNavigate without target should error")
	}
}

func TestInstantAction_HandleNavigate_HTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()
	h, _, _ := newTestActionHandler(t, testCfg(srv))
	if err := h.handleNavigate("a1", map[string]string{"target": "C01"}); err == nil {
		t.Errorf("expected error on HTTP 500")
	}
}

func TestInstantAction_HandleCancelOrder(t *testing.T) {
	t.Parallel()
	var posted int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posted, 1)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	h, _, _ := newTestActionHandler(t, testCfg(srv))
	if err := h.handleCancelOrder("a1", nil); err != nil {
		t.Fatalf("handleCancelOrder: %v", err)
	}
	if atomic.LoadInt32(&posted) == 0 {
		t.Errorf("handleCancelOrder should POST a stop command")
	}
}

func TestInstantAction_HandleGetWaypoints(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/poi_data" {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"point":{"0":{"name":"C01","x":1,"y":2,"type":"station","angle":0}}}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	cfg := testCfg(srv)
	h, state, _ := newTestActionHandler(t, cfg)
	state.SetMapID("mapA") // current map → uses FastAPI POI path

	if err := h.handleGetWaypoints("a1", map[string]string{}); err != nil {
		t.Fatalf("handleGetWaypoints: %v", err)
	}

	// No map at all → error.
	state2 := NewRobotState()
	h2 := NewInstantActionHandler(cfg, state2, NewMapService(cfg), NewMQTTBridge(cfg, state2, NewMapService(cfg), NewRobotWSClient(cfg, state2), nil), NewRobotWSClient(cfg, state2))
	if err := h2.handleGetWaypoints("a1", map[string]string{}); err == nil {
		t.Errorf("handleGetWaypoints with no map should error")
	}
}

func TestInstantAction_HandleInitPosition(t *testing.T) {
	t.Parallel()
	h, state, _ := newTestActionHandler(t, testCfg(nil))
	// WS is not connected → SetInitialPose errors on every attempt → handler
	// returns an error after 3 attempts (~2s). But mapId/state side-effects
	// are NOT applied because it returns before reaching them. We still cover
	// the param-parsing + retry loop.
	err := h.handleInitPosition("a1", map[string]string{"x": "1.5", "y": "2.5", "theta": "0.3", "mapId": "mapZ"})
	if err == nil {
		t.Errorf("handleInitPosition should error when WS is not connected")
	}
	// mapId is set before the WS loop, so it should have been applied.
	if state.Snapshot().MapID != "mapZ" {
		t.Errorf("handleInitPosition should set mapId before sending pose")
	}

	// invalid x → immediate error
	if err := h.handleInitPosition("a2", map[string]string{"x": "abc", "y": "1"}); err == nil {
		t.Errorf("handleInitPosition with invalid x should error")
	}
}

func TestInstantAction_HandleUploadMap_MissingURL(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestActionHandler(t, testCfg(nil))
	if err := h.handleUploadMap("a1", map[string]string{}); err == nil {
		t.Errorf("handleUploadMap without presigned URL should error")
	}
	if err := h.handleUploadMap("a1", map[string]string{"url": "http://x/up"}); err == nil {
		t.Errorf("handleUploadMap with no map name should error")
	}
}

func TestInstantAction_Handle_Dispatch(t *testing.T) {
	t.Parallel()
	// Exercise the public Handle entry point with a no-op-ish action and
	// verify the action state is registered. (stopPause is synchronous-ish.)
	h, state, _ := newTestActionHandler(t, testCfg(nil))
	h.Handle("d1", "stopPause", nil)
	if !waitForActionState(state, "d1") {
		t.Errorf("Handle should register action state for d1")
	}
	// Unknown action type → FAILED state.
	h.Handle("d2", "totallyUnknown", nil)
	ok := false
	for i := 0; i < 100 && !ok; i++ {
		for _, as := range state.Snapshot().ActionStates {
			if as.ActionID == "d2" && as.ActionStatus == "FAILED" {
				ok = true
			}
		}
		if !ok {
			waitTick()
		}
	}
	if !ok {
		t.Errorf("unknown action should be marked FAILED")
	}
}
