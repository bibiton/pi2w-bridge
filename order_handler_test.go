package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetActionParam(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		action *VDA5050Action
		key    string
		want   string
	}{
		{"present", &VDA5050Action{ActionParameters: []VDA5050ActionParam{{Key: "text", Value: "hi"}}}, "text", "hi"},
		{"absent", &VDA5050Action{ActionParameters: []VDA5050ActionParam{{Key: "text", Value: "hi"}}}, "duration", ""},
		{"empty params", &VDA5050Action{}, "text", ""},
		{"multiple, pick right", &VDA5050Action{ActionParameters: []VDA5050ActionParam{
			{Key: "a", Value: "1"}, {Key: "b", Value: "2"}, {Key: "c", Value: "3"},
		}}, "b", "2"},
		{"duplicate key returns first", &VDA5050Action{ActionParameters: []VDA5050ActionParam{
			{Key: "k", Value: "first"}, {Key: "k", Value: "second"},
		}}, "k", "first"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getActionParam(tt.action, tt.key); got != tt.want {
				t.Errorf("getActionParam = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOrderHandler_StateHelpers(t *testing.T) {
	t.Parallel()
	oh, state := newTestOrderHandler(t, testCfg(nil), nil)

	order := &VDA5050Order{
		OrderID: "ord-helpers",
		Nodes: []VDA5050Node{
			{NodeID: "n0", SequenceID: 0, Released: true, Actions: []VDA5050Action{{ActionID: "a1", ActionType: "playVoice"}}},
			{NodeID: "n1", SequenceID: 2, Released: true},
		},
		Edges: []VDA5050Edge{
			{EdgeID: "e1", SequenceID: 1, Released: true, Actions: []VDA5050Action{{ActionID: "a2", ActionType: "wait"}}},
		},
	}

	oh.initOrderStates(order)
	snap := state.Snapshot()
	if len(snap.NodeStates) != 2 || snap.NodeStates[0].NodeID != "n0" || snap.NodeStates[1].SequenceID != 2 {
		t.Fatalf("initOrderStates nodes wrong: %+v", snap.NodeStates)
	}
	if len(snap.EdgeStates) != 1 || snap.EdgeStates[0].EdgeID != "e1" {
		t.Fatalf("initOrderStates edges wrong: %+v", snap.EdgeStates)
	}

	oh.initActionStates(order)
	snap = state.Snapshot()
	if len(snap.ActionStates) != 2 {
		t.Fatalf("initActionStates: got %d action states, want 2", len(snap.ActionStates))
	}
	for _, as := range snap.ActionStates {
		if as.ActionStatus != "WAITING" {
			t.Errorf("action %s status = %q, want WAITING", as.ActionID, as.ActionStatus)
		}
	}

	oh.removeNodeState("n0")
	snap = state.Snapshot()
	if len(snap.NodeStates) != 1 || snap.NodeStates[0].NodeID != "n1" {
		t.Fatalf("removeNodeState: %+v", snap.NodeStates)
	}
	oh.removeNodeState("does-not-exist") // no-op
	if len(state.Snapshot().NodeStates) != 1 {
		t.Fatalf("removeNodeState non-existent should be no-op")
	}

	oh.removeEdgeState("e1")
	if len(state.Snapshot().EdgeStates) != 0 {
		t.Fatalf("removeEdgeState: %+v", state.Snapshot().EdgeStates)
	}
}

func TestOrderHandler_FinishOrder(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	_ = st.UpsertRobot(RobotRecord{ID: "testbot", Status: "online", Source: "test"})
	cfg := testCfg(nil)
	oh, state := newTestOrderHandler(t, cfg, st)

	_ = st.InsertOrder("ord-fin", "testbot", 0, []byte(`{"orderId":"ord-fin"}`))
	state.SetOrder("ord-fin", 0)

	oh.finishOrder("ord-fin")

	status, errRef := readOrderStatus(t, st, "ord-fin")
	if status != "finished" || errRef != "" {
		t.Errorf("after finishOrder: status=%q errRef=%q, want finished/empty", status, errRef)
	}
	if state.Snapshot().OrderID != "" {
		t.Errorf("finishOrder should clear state OrderID")
	}
}

func TestOrderHandler_FailOrder(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	_ = st.UpsertRobot(RobotRecord{ID: "testbot", Status: "online", Source: "test"})
	oh, state := newTestOrderHandler(t, testCfg(nil), st)

	_ = st.InsertOrder("ord-fail", "testbot", 0, []byte(`{}`))
	state.SetOrder("ord-fail", 0)

	oh.failOrder("ord-fail", "boom")
	status, errRef := readOrderStatus(t, st, "ord-fail")
	if status != "failed" || errRef != "boom" {
		t.Errorf("after failOrder: status=%q errRef=%q, want failed/boom", status, errRef)
	}
	if state.Snapshot().OrderID != "" {
		t.Errorf("failOrder should clear state OrderID")
	}
}

func TestOrderHandler_CancelOrder(t *testing.T) {
	t.Parallel()
	var posted int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			atomic.AddInt32(&posted, 1)
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	st := newTestStore(t)
	_ = st.UpsertRobot(RobotRecord{ID: "testbot", Status: "online", Source: "test"})
	oh, state := newTestOrderHandler(t, testCfg(srv), st)
	_ = st.InsertOrder("ord-cancel", "testbot", 0, []byte(`{}`))
	state.SetOrder("ord-cancel", 0)

	oh.cancelOrder("ord-cancel")
	status, _ := readOrderStatus(t, st, "ord-cancel")
	if status != "cancelled" {
		t.Errorf("after cancelOrder: status=%q, want cancelled", status)
	}
	if atomic.LoadInt32(&posted) == 0 {
		t.Errorf("cancelOrder should POST a cancel command to the robot")
	}
	if state.Snapshot().OrderID != "" {
		t.Errorf("cancelOrder should clear state OrderID")
	}
}

func TestOrderHandler_HandleOrder_MalformedJSON(t *testing.T) {
	t.Parallel()
	oh, state := newTestOrderHandler(t, testCfg(nil), nil)
	// Should not panic; should leave state untouched.
	oh.HandleOrder([]byte(`{not json`))
	oh.HandleOrder([]byte(`{"orderId":"","nodes":[]}`)) // missing id+nodes
	if state.Snapshot().OrderID != "" {
		t.Errorf("malformed/invalid order should not set state")
	}
}

func TestOrderHandler_ExecuteOrder_CrossMapFails(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	_ = st.UpsertRobot(RobotRecord{ID: "testbot", Status: "online", Source: "test"})
	oh, _ := newTestOrderHandler(t, testCfg(nil), st)

	order := &VDA5050Order{
		OrderID: "ord-crossmap",
		Nodes: []VDA5050Node{
			{NodeID: "n0", SequenceID: 0, Released: true, NodePosition: &VDA5050Position{MapID: "mapA"}},
			{NodeID: "n1", SequenceID: 2, Released: true, NodePosition: &VDA5050Position{MapID: "mapB"}},
		},
	}
	cancelCh := make(chan struct{})
	oh.executeOrder(order, cancelCh)

	status, errRef := readOrderStatus(t, st, "ord-crossmap")
	if status != "failed" || errRef != "cross_map_not_supported" {
		t.Errorf("cross-map order: status=%q errRef=%q, want failed/cross_map_not_supported", status, errRef)
	}
}

func TestOrderHandler_ExecuteOrder_ChargingTaskFinishes(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	_ = st.UpsertRobot(RobotRecord{ID: "testbot", Status: "online", Source: "test"})
	oh, state := newTestOrderHandler(t, testCfg(nil), st)

	order := &VDA5050Order{
		OrderID:  "ord-charge",
		TaskType: "charging",
		Nodes: []VDA5050Node{
			{NodeID: "origin", SequenceID: 0, Released: true},
		},
	}
	oh.executeOrder(order, make(chan struct{}))

	status, _ := readOrderStatus(t, st, "ord-charge")
	if status != "finished" {
		t.Errorf("charging task: status=%q, want finished", status)
	}
	if state.Snapshot().OrderID != "" {
		t.Errorf("expected order cleared from state")
	}
}

func TestOrderHandler_ExecuteOrder_CancelledMidway(t *testing.T) {
	t.Parallel()
	oh, state := newTestOrderHandler(t, testCfg(nil), nil)

	order := &VDA5050Order{
		OrderID: "ord-cancelmid",
		Nodes: []VDA5050Node{
			{NodeID: "n0", SequenceID: 0, Released: true},
			{NodeID: "n1", SequenceID: 2, Released: true},
		},
	}
	cancelCh := make(chan struct{})
	close(cancelCh) // already cancelled
	oh.executeOrder(order, cancelCh)

	if state.Snapshot().OrderID != "" {
		t.Errorf("cancelled order should clear state OrderID")
	}
}

func TestOrderHandler_ActionWait(t *testing.T) {
	t.Parallel()
	oh, _ := newTestOrderHandler(t, testCfg(nil), nil)

	// zero/negative duration → clamped to 1s minimum; this is the slowest path
	// so test the cancel path for speed instead.
	cancelCh := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(cancelCh)
	}()
	err := oh.actionWait(&VDA5050Action{ActionParameters: []VDA5050ActionParam{{Key: "duration", Value: "10"}}}, cancelCh)
	if err == nil {
		t.Errorf("actionWait should return error when cancelled")
	}

	// short positive duration completes normally
	start := time.Now()
	if err := oh.actionWait(&VDA5050Action{ActionParameters: []VDA5050ActionParam{{Key: "duration", Value: "0.05"}}}, make(chan struct{})); err != nil {
		t.Errorf("actionWait short duration: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("actionWait 0.05s took too long")
	}
}

func TestOrderHandler_WaitForNavigation(t *testing.T) {
	t.Parallel()
	// Fake ATOM status endpoint reports "arrived".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"route_status":{"status":"arrived"}}`))
	}))
	defer srv.Close()

	oh, state := newTestOrderHandler(t, testCfg(srv), nil)
	// Pre-seed: robot is already delivering (so waitForNavigation treats it as
	// departed) and an arrival signal is queued.
	state.mu.Lock()
	state.Status = "delivering"
	state.mu.Unlock()
	state.NotifyNavArrived()

	start := time.Now()
	if err := oh.waitForNavigation(make(chan struct{})); err != nil {
		t.Fatalf("waitForNavigation: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("waitForNavigation took too long: %v", time.Since(start))
	}

	// Cancel path.
	cancelCh := make(chan struct{})
	close(cancelCh)
	if err := oh.waitForNavigation(cancelCh); err == nil {
		t.Errorf("waitForNavigation should error on cancel")
	}
}

func TestOrderHandler_SendDeliveryWithRetry(t *testing.T) {
	t.Parallel()
	t.Run("HTTP error returns immediately", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`fail`))
		}))
		defer srv.Close()
		oh, _ := newTestOrderHandler(t, testCfg(srv), nil)
		start := time.Now()
		if err := oh.sendDeliveryWithRetry("C01"); err == nil {
			t.Errorf("expected error on HTTP 500")
		}
		if time.Since(start) > 2*time.Second {
			t.Errorf("HTTP-error path should be fast")
		}
	})

	t.Run("success when status becomes delivering", func(t *testing.T) {
		var commands int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/service/system/routing/status/get" {
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"route_status":{"status":"delivering"}}`))
				return
			}
			atomic.AddInt32(&commands, 1)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer srv.Close()
		oh, _ := newTestOrderHandler(t, testCfg(srv), nil)
		if err := oh.sendDeliveryWithRetry("C01"); err != nil {
			t.Errorf("sendDeliveryWithRetry: %v", err)
		}
		if atomic.LoadInt32(&commands) != 1 {
			t.Errorf("expected exactly 1 deliver command (no retry), got %d", commands)
		}
	})
}

func TestOrderHandler_ActionStartCharging(t *testing.T) {
	t.Parallel()
	t.Run("fast path when already charging", func(t *testing.T) {
		oh, state := newTestOrderHandler(t, testCfg(nil), nil)
		state.mu.Lock()
		state.BatteryCharging = true
		state.mu.Unlock()
		start := time.Now()
		if err := oh.actionStartCharging(&VDA5050Action{ActionID: "c1"}, make(chan struct{})); err != nil {
			t.Errorf("actionStartCharging fast path: %v", err)
		}
		if time.Since(start) > time.Second {
			t.Errorf("fast path should be near-instant")
		}
	})

	t.Run("HTTP error returns error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`nope`))
		}))
		defer srv.Close()
		oh, _ := newTestOrderHandler(t, testCfg(srv), nil)
		if err := oh.actionStartCharging(&VDA5050Action{ActionID: "c2"}, make(chan struct{})); err == nil {
			t.Errorf("expected error on HTTP 500 from goto_charging")
		}
	})

	t.Run("succeeds once webhook reports charging", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer srv.Close()
		oh, state := newTestOrderHandler(t, testCfg(srv), nil)
		go func() {
			time.Sleep(100 * time.Millisecond)
			state.mu.Lock()
			state.BatteryCharging = true
			state.mu.Unlock()
		}()
		start := time.Now()
		if err := oh.actionStartCharging(&VDA5050Action{ActionID: "c3"}, make(chan struct{})); err != nil {
			t.Errorf("actionStartCharging: %v", err)
		}
		if time.Since(start) > 4*time.Second {
			t.Errorf("should detect charging within a couple poll ticks")
		}
	})
}

func TestOrderHandler_NavigateByStation_MissingTarget(t *testing.T) {
	t.Parallel()
	oh, _ := newTestOrderHandler(t, testCfg(nil), nil)
	err := oh.navigateByStation("ord", &VDA5050Action{ActionID: "g1", ActionType: "GoToLocation"}, make(chan struct{}))
	if err == nil {
		t.Errorf("navigateByStation with no stationId/stationName should error")
	}
}

func TestOrderHandler_NavigateByStation_Success(t *testing.T) {
	t.Parallel()
	// First /service/system/routing/status/get returns "delivering" (so the
	// deliver command is considered active), subsequent calls return "arrived".
	var statusCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/service/system/routing/status/get" {
			n := atomic.AddInt32(&statusCalls, 1)
			w.WriteHeader(200)
			if n <= 1 {
				_, _ = w.Write([]byte(`{"route_status":{"status":"delivering"}}`))
			} else {
				_, _ = w.Write([]byte(`{"route_status":{"status":"arrived"}}`))
			}
			return
		}
		// commands endpoint
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	oh, state := newTestOrderHandler(t, testCfg(srv), nil)
	state.mu.Lock()
	state.Status = "delivering" // already departed
	state.mu.Unlock()
	// Register the action so UpdateActionState has something to update.
	state.AddActionState(ActionState{ActionID: "g2", ActionType: "GoToLocation", ActionStatus: "WAITING"})

	action := &VDA5050Action{
		ActionID:   "g2",
		ActionType: "GoToLocation",
		ActionParameters: []VDA5050ActionParam{
			{Key: "stationName", Value: "C01"},
		},
	}
	start := time.Now()
	if err := oh.navigateByStation("ord-nav", action, make(chan struct{})); err != nil {
		t.Fatalf("navigateByStation: %v", err)
	}
	if time.Since(start) > 8*time.Second {
		t.Errorf("navigateByStation too slow: %v", time.Since(start))
	}
	// action should be marked FINISHED.
	for _, as := range state.Snapshot().ActionStates {
		if as.ActionID == "g2" && as.ActionStatus != "FINISHED" {
			t.Errorf("action g2 status = %q, want FINISHED", as.ActionStatus)
		}
	}
}

func TestOrderHandler_HandleOrder_FullSmoke(t *testing.T) {
	t.Parallel()
	// End-to-end-ish: HandleOrder accepts a charging order, executeOrder runs
	// in a goroutine, eventually the order reaches "finished" in the store.
	st := newTestStore(t)
	_ = st.UpsertRobot(RobotRecord{ID: "testbot", Status: "online", Source: "test"})
	oh, state := newTestOrderHandler(t, testCfg(nil), st)

	order := VDA5050Order{
		OrderID:  "ord-smoke",
		TaskType: "charging",
		Nodes:    []VDA5050Node{{NodeID: "origin", SequenceID: 0, Released: true}},
	}
	payload, _ := json.Marshal(order)
	oh.HandleOrder(payload)

	pollStatus := func() string {
		var s string
		_ = st.db.QueryRow(`SELECT status FROM orders WHERE order_id=?`, "ord-smoke").Scan(&s)
		return s
	}
	// Wait up to 6s for executeOrder (finishOrder sleeps 3s).
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if pollStatus() == "finished" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := pollStatus(); got != "finished" {
		t.Errorf("order status = %q, want finished", got)
	}
	_ = state
	oh.CancelCurrentOrder() // exercise + cleanup
}

func TestOrderHandler_ExecuteAction(t *testing.T) {
	t.Parallel()
	oh, state := newTestOrderHandler(t, testCfg(nil), nil)

	t.Run("wait action", func(t *testing.T) {
		state.AddActionState(ActionState{ActionID: "w1", ActionType: "wait", ActionStatus: "WAITING"})
		err := oh.executeAction("ord", &VDA5050Action{ActionID: "w1", ActionType: "wait",
			ActionParameters: []VDA5050ActionParam{{Key: "duration", Value: "0.01"}}}, make(chan struct{}))
		if err != nil {
			t.Fatalf("wait action: %v", err)
		}
		assertActionStatus(t, state, "w1", "FINISHED")
	})

	t.Run("drop action (no mechanism)", func(t *testing.T) {
		state.AddActionState(ActionState{ActionID: "d1", ActionType: "drop", ActionStatus: "WAITING"})
		if err := oh.executeAction("ord", &VDA5050Action{ActionID: "d1", ActionType: "drop"}, make(chan struct{})); err != nil {
			t.Fatalf("drop action: %v", err)
		}
		assertActionStatus(t, state, "d1", "FINISHED")
	})

	t.Run("unknown action type is skipped", func(t *testing.T) {
		state.AddActionState(ActionState{ActionID: "u1", ActionType: "frobnicate", ActionStatus: "WAITING"})
		if err := oh.executeAction("ord", &VDA5050Action{ActionID: "u1", ActionType: "frobnicate"}, make(chan struct{})); err != nil {
			t.Fatalf("unknown action: %v", err)
		}
		assertActionStatus(t, state, "u1", "FINISHED")
	})

	t.Run("playVoice with nil TTS continues", func(t *testing.T) {
		// cfg.TTSURL is empty → oh.tts is nil → playVoice logs and continues.
		state.AddActionState(ActionState{ActionID: "v1", ActionType: "playVoice", ActionStatus: "WAITING"})
		if err := oh.executeAction("ord", &VDA5050Action{ActionID: "v1", ActionType: "playVoice",
			ActionParameters: []VDA5050ActionParam{{Key: "text", Value: "hi"}}}, make(chan struct{})); err != nil {
			t.Fatalf("playVoice nil tts: %v", err)
		}
		assertActionStatus(t, state, "v1", "FINISHED")
	})

	t.Run("startCharging fast path (already charging)", func(t *testing.T) {
		state.mu.Lock()
		state.BatteryCharging = true
		state.mu.Unlock()
		state.AddActionState(ActionState{ActionID: "c1", ActionType: "startCharging", ActionStatus: "WAITING"})
		if err := oh.executeAction("ord", &VDA5050Action{ActionID: "c1", ActionType: "startCharging"}, make(chan struct{})); err != nil {
			t.Fatalf("startCharging: %v", err)
		}
		assertActionStatus(t, state, "c1", "FINISHED")
	})
}

func TestOrderHandler_ExecuteNodeAndEdgeActions(t *testing.T) {
	t.Parallel()
	oh, state := newTestOrderHandler(t, testCfg(nil), nil)
	node := &VDA5050Node{
		NodeID: "n1",
		Actions: []VDA5050Action{
			{ActionID: "g1", ActionType: "GoToLocation"}, // skipped in executeNodeActions
			{ActionID: "w1", ActionType: "wait", ActionParameters: []VDA5050ActionParam{{Key: "duration", Value: "0.01"}}},
		},
	}
	state.AddActionState(ActionState{ActionID: "w1", ActionType: "wait", ActionStatus: "WAITING"})
	if err := oh.executeNodeActions("ord", node, make(chan struct{})); err != nil {
		t.Fatalf("executeNodeActions: %v", err)
	}
	assertActionStatus(t, state, "w1", "FINISHED")

	edge := &VDA5050Edge{
		EdgeID:  "e1",
		Actions: []VDA5050Action{{ActionID: "ew1", ActionType: "drop"}},
	}
	state.AddActionState(ActionState{ActionID: "ew1", ActionType: "drop", ActionStatus: "WAITING"})
	if err := oh.executeEdgeActions("ord", edge, make(chan struct{})); err != nil {
		t.Fatalf("executeEdgeActions: %v", err)
	}
	assertActionStatus(t, state, "ew1", "FINISHED")

	// Cancelled context → error.
	cancelCh := make(chan struct{})
	close(cancelCh)
	if err := oh.executeNodeActions("ord", node, cancelCh); err == nil {
		t.Errorf("executeNodeActions should error when cancelled")
	}
}

func TestOrderHandler_NavigateToNode(t *testing.T) {
	t.Parallel()
	t.Run("XY navigation (no GoToLocation) is a no-op success", func(t *testing.T) {
		oh, _ := newTestOrderHandler(t, testCfg(nil), nil)
		node := &VDA5050Node{NodeID: "n1", NodePosition: &VDA5050Position{X: 1.0, Y: 2.0}}
		if err := oh.navigateToNode("ord", node, make(chan struct{})); err != nil {
			t.Errorf("navigateToNode XY: %v", err)
		}
	})

	t.Run("GoToLocation routes to station nav", func(t *testing.T) {
		var statusCalls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/service/system/routing/status/get" {
				n := atomic.AddInt32(&statusCalls, 1)
				w.WriteHeader(200)
				if n <= 1 {
					_, _ = w.Write([]byte(`{"route_status":{"status":"delivering"}}`))
				} else {
					_, _ = w.Write([]byte(`{"route_status":{"status":"arrived"}}`))
				}
				return
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer srv.Close()
		oh, state := newTestOrderHandler(t, testCfg(srv), nil)
		state.mu.Lock()
		state.Status = "delivering"
		state.mu.Unlock()
		state.AddActionState(ActionState{ActionID: "g1", ActionType: "GoToLocation", ActionStatus: "WAITING"})
		node := &VDA5050Node{NodeID: "n1", Actions: []VDA5050Action{
			{ActionID: "g1", ActionType: "GoToLocation", ActionParameters: []VDA5050ActionParam{{Key: "stationName", Value: "C01"}}},
		}}
		if err := oh.navigateToNode("ord", node, make(chan struct{})); err != nil {
			t.Fatalf("navigateToNode GoToLocation: %v", err)
		}
	})
}

func TestOrderHandler_NavigateToStation(t *testing.T) {
	t.Parallel()
	var statusCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/service/system/routing/status/get" {
			n := atomic.AddInt32(&statusCalls, 1)
			w.WriteHeader(200)
			if n <= 1 {
				_, _ = w.Write([]byte(`{"route_status":{"status":"delivering"}}`))
			} else {
				_, _ = w.Write([]byte(`{"route_status":{"status":"arrived"}}`))
			}
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	oh, state := newTestOrderHandler(t, testCfg(srv), nil)
	state.mu.Lock()
	state.Status = "delivering"
	state.mu.Unlock()
	if err := oh.navigateToStation("ord", "counter", make(chan struct{})); err != nil {
		t.Fatalf("navigateToStation: %v", err)
	}
	if state.Snapshot().Driving {
		t.Errorf("navigateToStation should clear Driving on success")
	}

	// sendDelivery error → propagated.
	srvErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srvErr.Close()
	oh2, _ := newTestOrderHandler(t, testCfg(srvErr), nil)
	if err := oh2.navigateToStation("ord", "counter", make(chan struct{})); err == nil {
		t.Errorf("navigateToStation should error when delivery POST fails")
	}
}

func TestOrderHandler_PlayVoiceAction(t *testing.T) {
	t.Parallel()
	t.Run("immediate success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/play" {
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"ok":true,"duration_ms":10}`))
				return
			}
			w.WriteHeader(200)
		}))
		defer srv.Close()
		cfg := testCfg(nil)
		cfg.TTSURL = srv.URL
		oh, _ := newTestOrderHandler(t, cfg, nil)
		if err := oh.playVoiceAction(&VDA5050Action{ActionID: "v1"}, "hello", make(chan struct{})); err != nil {
			t.Fatalf("playVoiceAction: %v", err)
		}
	})

	t.Run("404 then prepare then success", func(t *testing.T) {
		var playCalls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/play":
				if atomic.AddInt32(&playCalls, 1) == 1 {
					w.WriteHeader(404)
					return
				}
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"ok":true}`))
			case "/prepare":
				w.WriteHeader(200)
			default:
				w.WriteHeader(404)
			}
		}))
		defer srv.Close()
		cfg := testCfg(nil)
		cfg.TTSURL = srv.URL
		oh, _ := newTestOrderHandler(t, cfg, nil)
		if err := oh.playVoiceAction(&VDA5050Action{ActionID: "v2"}, "hello", make(chan struct{})); err != nil {
			t.Fatalf("playVoiceAction (404->prepare): %v", err)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		cfg := testCfg(nil)
		cfg.TTSURL = "http://127.0.0.1:1"
		oh, _ := newTestOrderHandler(t, cfg, nil)
		cancelCh := make(chan struct{})
		close(cancelCh)
		if err := oh.playVoiceAction(&VDA5050Action{ActionID: "v3"}, "hi", cancelCh); err == nil {
			t.Errorf("playVoiceAction should error when cancelled")
		}
	})
}

func TestOrderHandler_EnsureNotCharging(t *testing.T) {
	t.Parallel()
	t.Run("not charging is a no-op", func(t *testing.T) {
		oh, _ := newTestOrderHandler(t, testCfg(nil), nil)
		if err := oh.ensureNotCharging(make(chan struct{})); err != nil {
			t.Errorf("ensureNotCharging when not charging: %v", err)
		}
	})

	t.Run("leaves charger then stabilizes", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer srv.Close()
		oh, state := newTestOrderHandler(t, testCfg(srv), nil)
		state.mu.Lock()
		state.BatteryCharging = true
		state.mu.Unlock()
		go func() {
			time.Sleep(100 * time.Millisecond)
			state.mu.Lock()
			state.BatteryCharging = false
			state.mu.Unlock()
		}()
		start := time.Now()
		if err := oh.ensureNotCharging(make(chan struct{})); err != nil {
			t.Fatalf("ensureNotCharging: %v", err)
		}
		// 2s poll tick to notice + 5s stabilize = ~7s; allow margin.
		if time.Since(start) > 10*time.Second {
			t.Errorf("ensureNotCharging too slow: %v", time.Since(start))
		}
	})

	t.Run("HTTP error returns error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
		}))
		defer srv.Close()
		oh, state := newTestOrderHandler(t, testCfg(srv), nil)
		state.mu.Lock()
		state.BatteryCharging = true
		state.mu.Unlock()
		if err := oh.ensureNotCharging(make(chan struct{})); err == nil {
			t.Errorf("expected error on leave_charger HTTP 500")
		}
	})
}

func assertActionStatus(t *testing.T, state *RobotState, actionID, want string) {
	t.Helper()
	for _, as := range state.Snapshot().ActionStates {
		if as.ActionID == actionID {
			if as.ActionStatus != want {
				t.Errorf("action %s status = %q, want %q", actionID, as.ActionStatus, want)
			}
			return
		}
	}
	t.Errorf("action %s not found in state", actionID)
}
