package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestQuatToYaw(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		x, y, z, w       float64
		wantRad, wantDeg float64
	}{
		{"identity", 0, 0, 0, 1, 0, 0},
		{"90deg", 0, 0, 0.7071068, 0.7071068, math.Pi / 2, 90},
		{"180deg", 0, 0, 1, 0, math.Pi, 180},
		{"-90deg", 0, 0, -0.7071068, 0.7071068, -math.Pi / 2, 270},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRad := QuatToYaw(tt.x, tt.y, tt.z, tt.w)
			if math.Abs(gotRad-tt.wantRad) > 1e-4 {
				t.Errorf("QuatToYaw rad = %v, want %v", gotRad, tt.wantRad)
			}
			gotDeg := QuatToYawDeg(tt.x, tt.y, tt.z, tt.w)
			if math.Abs(gotDeg-tt.wantDeg) > 1e-3 {
				t.Errorf("QuatToYawDeg = %v, want %v", gotDeg, tt.wantDeg)
			}
			if gotDeg < 0 || gotDeg >= 360 {
				t.Errorf("QuatToYawDeg out of [0,360): %v", gotDeg)
			}
		})
	}
}

func TestApplyWebhookData(t *testing.T) {
	t.Parallel()
	t.Run("route status + driving + arrival notification", func(t *testing.T) {
		rs := NewRobotState()
		rs.DrainNavArrived()
		ApplyWebhookData(rs, map[string]interface{}{
			"route_status": map[string]interface{}{"status": "arrived"},
		})
		snap := rs.Snapshot()
		if snap.Status != "arrived" {
			t.Errorf("Status = %q, want arrived", snap.Status)
		}
		if snap.Driving {
			t.Errorf("Driving should be false when arrived")
		}
		if snap.LastUpdate.IsZero() {
			t.Errorf("LastUpdate must be set")
		}
		// arrival should have queued a NavArrived signal
		select {
		case <-rs.NavArrivedCh:
		default:
			t.Errorf("expected NavArrived signal after 'arrived'")
		}
	})

	t.Run("delivering sets driving + AUTOMATIC", func(t *testing.T) {
		rs := NewRobotState()
		ApplyWebhookData(rs, map[string]interface{}{"routing status": "DELIVERING"})
		snap := rs.Snapshot()
		if !snap.Driving || snap.OperatingMode != "AUTOMATIC" || snap.Status != "delivering" {
			t.Errorf("delivering not applied: %+v", snap)
		}
	})

	t.Run("battery + charging events", func(t *testing.T) {
		rs := NewRobotState()
		ApplyWebhookData(rs, map[string]interface{}{
			"battery_level": 73.5,
			"event":         "show_charging",
		})
		snap := rs.Snapshot()
		if snap.BatteryPercent != 73.5 || !snap.BatteryCharging || snap.Event != "show_charging" {
			t.Errorf("battery/charging not applied: %+v", snap)
		}
		ApplyWebhookData(rs, map[string]interface{}{"event": "remove_charging"})
		if rs.Snapshot().BatteryCharging {
			t.Errorf("remove_charging should clear BatteryCharging")
		}
	})

	t.Run("pose with valid quaternion", func(t *testing.T) {
		rs := NewRobotState()
		ApplyWebhookData(rs, map[string]interface{}{
			"pose": map[string]interface{}{
				"position":    map[string]interface{}{"x": 1.5, "y": -2.0},
				"orientation": map[string]interface{}{"x": 0.0, "y": 0.0, "z": 1.0, "w": 0.0},
				"velocity":    map[string]interface{}{"x": 0.1, "y": 0.0, "z": 0.2},
			},
		})
		snap := rs.Snapshot()
		if snap.PoseX != 1.5 || snap.PoseY != -2.0 {
			t.Errorf("pose not applied: %+v", snap)
		}
		if math.Abs(snap.PoseYaw-math.Pi) > 1e-4 {
			t.Errorf("yaw = %v, want pi", snap.PoseYaw)
		}
		if snap.VelocityVX != 0.1 || snap.VelocityOmega != 0.2 {
			t.Errorf("velocity not applied: %+v", snap)
		}
	})

	t.Run("invalid (all-zero) quaternion is ignored", func(t *testing.T) {
		rs := NewRobotState()
		rs.PoseX = 9
		ApplyWebhookData(rs, map[string]interface{}{
			"pose": map[string]interface{}{
				"position":    map[string]interface{}{"x": 1.0, "y": 1.0},
				"orientation": map[string]interface{}{"x": 0.0, "y": 0.0, "z": 0.0, "w": 0.0},
			},
		})
		if rs.Snapshot().PoseX != 9 {
			t.Errorf("pose should not be updated with zero quaternion")
		}
	})

	t.Run("target as dict", func(t *testing.T) {
		rs := NewRobotState()
		ApplyWebhookData(rs, map[string]interface{}{
			"target": map[string]interface{}{
				"delivery_command": map[string]interface{}{
					"deliver_to_location": []interface{}{"C03"},
				},
			},
		})
		if rs.Snapshot().Target != "C03" {
			t.Errorf("target dict not parsed: %q", rs.Snapshot().Target)
		}
	})
}

func TestToFloat64(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   interface{}
		want float64
	}{
		{float64(1.5), 1.5},
		{float32(2), 2},
		{int(3), 3},
		{int64(4), 4},
		{"5.5", 5.5},
		{"42%", 42},
		{"notanumber", 0},
		{nil, 0},
		{[]int{1}, 0},
	}
	for _, c := range cases {
		if got := toFloat64(c.in); got != c.want {
			t.Errorf("toFloat64(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRobotState_FreshnessAndMutators(t *testing.T) {
	t.Parallel()
	rs := NewRobotState()
	if rs.IsDataFresh(time.Second) {
		t.Errorf("brand-new state should not be fresh (zero LastUpdate)")
	}
	rs.mu.Lock()
	rs.LastUpdate = time.Now()
	rs.mu.Unlock()
	if !rs.IsDataFresh(time.Second) {
		t.Errorf("just-updated state should be fresh")
	}
	rs.mu.Lock()
	rs.LastUpdate = time.Now().Add(-time.Hour)
	rs.mu.Unlock()
	if rs.IsDataFresh(time.Second) {
		t.Errorf("stale state should not be fresh")
	}

	rs.SetMapID("mapX")
	rs.SetMapList([]string{"mapX", "mapY"})
	rs.SetConnectionState("ONLINE")
	rs.SetOrder("o1", 7)
	rs.SetLastNode("n5", 5)
	rs.SetDriving(true)
	rs.SetPaused(true)
	rs.AddActionState(ActionState{ActionID: "a1", ActionStatus: "RUNNING"})
	rs.AddActionState(ActionState{ActionID: "a2", ActionStatus: "FINISHED"})
	rs.UpdateActionState("a1", "FINISHED", "done")
	snap := rs.Snapshot()
	if snap.MapID != "mapX" || len(snap.MapList) != 2 || snap.ConnectionState != "ONLINE" {
		t.Errorf("map/conn mutators: %+v", snap)
	}
	if snap.OrderID != "o1" || snap.OrderUpdateID != 7 || snap.LastNodeID != "n5" || !snap.Driving || !snap.Paused {
		t.Errorf("order/node mutators: %+v", snap)
	}
	for _, as := range snap.ActionStates {
		if as.ActionID == "a1" && (as.ActionStatus != "FINISHED" || as.ResultDescription != "done") {
			t.Errorf("UpdateActionState a1: %+v", as)
		}
	}
	rs.RemoveFinishedActions()
	if len(rs.Snapshot().ActionStates) != 0 {
		t.Errorf("RemoveFinishedActions should drop FINISHED/FAILED entries")
	}

	rs.ClearOrder()
	snap = rs.Snapshot()
	if snap.OrderID != "" || len(snap.NodeStates) != 0 || snap.Driving {
		t.Errorf("ClearOrder did not reset: %+v", snap)
	}

	// NotifyNavArrived / DrainNavArrived
	rs.NotifyNavArrived()
	rs.NotifyNavArrived() // second is dropped (cap 1)
	rs.DrainNavArrived()
	select {
	case <-rs.NavArrivedCh:
		t.Errorf("channel should be drained")
	default:
	}
	rs.DrainNavArrived() // draining an empty channel is fine
}

func TestComposeState(t *testing.T) {
	t.Parallel()
	cfg := &Config{Manufacturer: "atom", SerialNumber: "adai01", RobotIP: "10.1.2.3"}
	snap := StateSnapshot{
		PoseX: 1.23456, PoseY: 2.0, PoseYaw: 0.5,
		MapID: "mapA", Driving: true, OperatingMode: "AUTOMATIC",
		BatteryPercent: 88.88, BatteryVoltage: 24.6, BatteryCharging: true,
		OrderID: "o1", OrderUpdateID: 3, LastNodeID: "n1", LastNodeSequenceID: 2,
		Status: "delivering", Target: "C01", Event: "show_charging",
		MapList:      []string{"mapA", "mapB"},
		ActionStates: []ActionState{{ActionID: "a1", ActionStatus: "RUNNING"}},
		PositionInit: true,
	}
	data, err := ComposeState(snap, cfg)
	if err != nil {
		t.Fatalf("ComposeState: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["orderId"] != "o1" || m["manufacturer"] != "atom" || m["version"] != "2.0.0" {
		t.Errorf("top-level fields wrong: %v", m)
	}
	pos := m["agvPosition"].(map[string]interface{})
	if pos["x"].(float64) != 1.235 { // round3
		t.Errorf("x not rounded to 3 dp: %v", pos["x"])
	}
	if pos["mapId"] != "mapA" {
		t.Errorf("mapId: %v", pos["mapId"])
	}
	bat := m["batteryState"].(map[string]interface{})
	if bat["batteryCharge"].(float64) != 88.9 { // round1
		t.Errorf("batteryCharge not rounded to 1 dp: %v", bat["batteryCharge"])
	}
	if bat["charging"] != true {
		t.Errorf("charging: %v", bat["charging"])
	}
	infos := m["informations"].([]interface{})
	// agvIp + robotStatus + currentTarget + lastEvent + 2 mapList = 6
	if len(infos) != 6 {
		t.Errorf("informations count = %d, want 6: %v", len(infos), infos)
	}

	// Visualization
	vdata, err := ComposeVisualization(snap, cfg)
	if err != nil {
		t.Fatalf("ComposeVisualization: %v", err)
	}
	var vm map[string]interface{}
	_ = json.Unmarshal(vdata, &vm)
	if vm["driving"] != true || vm["serialNumber"] != "adai01" {
		t.Errorf("visualization fields wrong: %v", vm)
	}
}

func TestFormatStateLog(t *testing.T) {
	t.Parallel()
	snap := StateSnapshot{PoseX: 1.0, PoseY: 2.0, PoseYaw: math.Pi, MapID: "mapA",
		BatteryPercent: 50, Driving: true, Status: "delivering"}
	s := FormatStateLog(snap)
	for _, want := range []string{"map=mapA", "bat=50%", "drv=true", "status=delivering", "180.0"} {
		if !strings.Contains(s, want) {
			t.Errorf("FormatStateLog missing %q in %q", want, s)
		}
	}
}

func TestComposeFactsheet(t *testing.T) {
	t.Parallel()
	cfg := &Config{Manufacturer: "atom", SerialNumber: "adai01"}
	data, err := ComposeFactsheet(cfg)
	if err != nil {
		t.Fatalf("ComposeFactsheet: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["manufacturer"] != "atom" || m["serialNumber"] != "adai01" || m["version"] != "2.0.0" {
		t.Errorf("factsheet identity wrong: %v", m)
	}
	ts := m["typeSpecification"].(map[string]interface{})
	if ts["seriesName"] != "ATOM" || ts["agvKinematic"] != "DIFF" {
		t.Errorf("typeSpecification wrong: %v", ts)
	}
	pf := m["protocolFeatures"].(map[string]interface{})
	actions := pf["agvActions"].([]interface{})
	if len(actions) != 3 {
		t.Errorf("expected 3 agvActions, got %d", len(actions))
	}
}
