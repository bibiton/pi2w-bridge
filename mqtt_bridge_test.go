package main

import (
	"strings"
	"testing"
	"time"
)

func TestIsTwAction(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"tw_elevator_call": true,
		"tw_":              true,
		"twAction":         false,
		"playVoice":        false,
		"":                 false,
		"TW_FOO":           false, // case-sensitive prefix
	}
	for in, want := range cases {
		if got := isTwAction(in); got != want {
			t.Errorf("isTwAction(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestConfig_TopicPrefixAndBaseURL(t *testing.T) {
	t.Parallel()
	c := &Config{MQTTPrefix: "/uagv/v2", Manufacturer: "atom", SerialNumber: "adai01",
		RobotIP: "10.0.0.5", RobotPort: "8080"}
	if got := c.TopicPrefix(); got != "/uagv/v2/atom/adai01" {
		t.Errorf("TopicPrefix = %q", got)
	}
	if got := c.RobotBaseURL(); got != "http://10.0.0.5:8080" {
		t.Errorf("RobotBaseURL = %q", got)
	}
}

func TestMQTTBridge_PublishNoOpWhenDisconnected(t *testing.T) {
	t.Parallel()
	cfg := testCfg(nil)
	state := NewRobotState()
	ms := NewMapService(cfg)
	ws := NewRobotWSClient(cfg, state)
	mb := NewMQTTBridge(cfg, state, ms, ws, nil)

	// None of these should panic with a nil client.
	mb.publish("topic", []byte("x"), 0, false)
	mb.publishState()
	mb.publishVisualization()
	mb.publishConnection("ONLINE")
	mb.publishFactsheet()
	mb.PublishWaypoints([]byte("[]"))
	mb.TriggerStatePublish()
	mb.TriggerStatePublish() // second call hits the default branch (channel full)
}

func TestMQTTBridge_HandleInstantActions(t *testing.T) {
	t.Parallel()
	cfg := testCfg(nil)
	state := NewRobotState()
	ms := NewMapService(cfg)
	ws := NewRobotWSClient(cfg, state)
	mb := NewMQTTBridge(cfg, state, ms, ws, nil)

	t.Run("nil actionHandler is a no-op", func(t *testing.T) {
		// actionHandler is nil until Connect(); must not panic.
		mb.handleInstantActions([]byte(`{"instantActions":[{"actionId":"a1","actionType":"stateRequest"}]}`))
	})

	t.Run("malformed json is a no-op", func(t *testing.T) {
		mb.handleInstantActions([]byte(`{not-json`))
	})

	// Wire a real action handler with non-connected deps.
	mb.actionHandler = NewInstantActionHandler(cfg, state, ms, mb, ws)
	mb.orderHandler = NewOrderHandler(cfg, state, mb, ws, nil)

	t.Run("instantActions array: stateRequest is forwarded", func(t *testing.T) {
		mb.handleInstantActions([]byte(`{"instantActions":[{"actionId":"sr1","actionType":"stateRequest"}]}`))
		if !waitForActionState(state, "sr1") {
			t.Errorf("expected action sr1 to be registered by the handler")
		}
	})

	t.Run("fallback to 'actions' key", func(t *testing.T) {
		mb.handleInstantActions([]byte(`{"actions":[{"actionId":"sr2","actionType":"stateRequest"}]}`))
		if !waitForActionState(state, "sr2") {
			t.Errorf("expected action sr2 to be registered via 'actions' key")
		}
	})

	t.Run("tw_ actions are skipped (not forwarded)", func(t *testing.T) {
		mb.handleInstantActions([]byte(`{"instantActions":[{"actionId":"tw1","actionType":"tw_elevator_call"}]}`))
		time.Sleep(100 * time.Millisecond) // give a (rejected) forward a chance
		for _, as := range state.Snapshot().ActionStates {
			if as.ActionID == "tw1" {
				t.Errorf("tw_ action should NOT be forwarded to the action handler")
			}
		}
	})

	t.Run("actionParameters key/value extracted", func(t *testing.T) {
		// startPause has no params but stopPause/startPause toggle state.Paused —
		// use that as an observable side-effect of the parsing path.
		mb.handleInstantActions([]byte(`{"instantActions":[{"actionId":"p1","actionType":"startPause","actionParameters":[{"key":"x","value":"y"}]}]}`))
		// startPause runs in a goroutine; poll.
		if !waitForPaused(state, true) {
			t.Errorf("startPause should set state.Paused=true")
		}
	})
}

func waitForActionState(state *RobotState, actionID string) bool {
	for i := 0; i < 100; i++ {
		for _, as := range state.Snapshot().ActionStates {
			if as.ActionID == actionID {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func waitForPaused(state *RobotState, want bool) bool {
	for i := 0; i < 100; i++ {
		if state.Snapshot().Paused == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestMQTTBridge_StopNoOp(t *testing.T) {
	t.Parallel()
	cfg := testCfg(nil)
	state := NewRobotState()
	mb := NewMQTTBridge(cfg, state, NewMapService(cfg), NewRobotWSClient(cfg, state), nil)
	mb.Stop()
	mb.Stop() // idempotent (sync.Once)
}

func TestComposeConnection_RoundTrip(t *testing.T) {
	t.Parallel()
	cfg := testCfg(nil)
	data, err := ComposeConnection("CONNECTIONBROKEN", cfg)
	if err != nil {
		t.Fatalf("ComposeConnection: %v", err)
	}
	if !strings.Contains(string(data), `"connectionState":"CONNECTIONBROKEN"`) {
		t.Errorf("unexpected connection payload: %s", data)
	}
}
