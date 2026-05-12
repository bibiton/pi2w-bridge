package main

import (
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStore_RobotRoundTrip(t *testing.T) {
	s := newTestStore(t)
	rec := RobotRecord{ID: "adai01", Manufacturer: "atom", Serial: "adai01",
		AtomBaseURL: "http://1.2.3.4:8080", FastAPIHTTPURL: "http://1.2.3.4:8000",
		FastAPIWSURL: "ws://1.2.3.4:8000/ws", WebhookSecret: "s", Status: "online", Source: "yaml"}
	if err := s.UpsertRobot(rec); err != nil {
		t.Fatalf("UpsertRobot: %v", err)
	}
	got, err := s.GetRobot("adai01")
	if err != nil {
		t.Fatalf("GetRobot: %v", err)
	}
	if got.AtomBaseURL != rec.AtomBaseURL || got.Status != "online" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	if err := s.TouchRobot("adai01", "online", time.Now()); err != nil {
		t.Fatalf("TouchRobot: %v", err)
	}
	list, err := s.ListRobots()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListRobots: %v len=%d", err, len(list))
	}
	if err := s.SetRobotStatus("adai01", "deleted"); err != nil {
		t.Fatalf("SetRobotStatus: %v", err)
	}
	list, _ = s.ListActiveRobots()
	if len(list) != 0 {
		t.Errorf("deleted robot still active: %+v", list)
	}
}

func TestStore_OrderLifecycle(t *testing.T) {
	s := newTestStore(t)
	_ = s.UpsertRobot(RobotRecord{ID: "r1", Status: "online", Source: "yaml"})
	if err := s.InsertOrder("ord1", "r1", 0, []byte(`{"orderId":"ord1"}`)); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	if err := s.UpdateOrderNode("ord1", "n2"); err != nil {
		t.Fatalf("UpdateOrderNode: %v", err)
	}
	if err := s.FinishOrder("ord1", "finished", ""); err != nil {
		t.Fatalf("FinishOrder: %v", err)
	}
	if err := s.UpsertActionState("ord1", "a1", "playVoice", "FINISHED", "ok"); err != nil {
		t.Fatalf("UpsertActionState: %v", err)
	}
	_ = s.InsertOrder("ord2", "r1", 0, []byte(`{}`))
	n, err := s.FailRunningOrders("bridge_restarted")
	if err != nil || n != 1 {
		t.Fatalf("FailRunningOrders n=%d err=%v", n, err)
	}
}
