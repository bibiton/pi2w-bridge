package main

import (
	"testing"
)

func TestSessionManager_RegisterDeregister(t *testing.T) {
	srv := &ServerConfig{MQTTBroker: "tcp://127.0.0.1:1", MQTTPrefix: "/uagv/v2",
		Manufacturer: "atom", DefaultRobotSecret: "s", PublicBaseURL: "http://localhost"}
	st := newTestStore(t)
	m := NewSessionManager(srv, st)
	defer m.StopAll()

	rec := RobotRecord{ID: "r1", Serial: "r1", AtomBaseURL: "http://127.0.0.1:18080",
		FastAPIHTTPURL: "http://127.0.0.1:18000", FastAPIWSURL: "ws://127.0.0.1:18000/ws", Source: "yaml"}
	if err := m.Register(rec); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if m.Get("r1") == nil {
		t.Fatalf("Get r1 nil after Register")
	}
	if len(m.List()) != 1 {
		t.Fatalf("List len != 1")
	}
	if err := m.Register(rec); err != nil {
		t.Fatalf("re-Register: %v", err)
	}
	if len(m.List()) != 1 {
		t.Fatalf("List len != 1 after re-register")
	}
	m.Deregister("r1")
	if m.Get("r1") != nil {
		t.Fatalf("Get r1 not nil after Deregister")
	}
	got, _ := st.GetRobot("r1")
	if got.Status != "deleted" {
		t.Errorf("DB status after Deregister = %q, want deleted", got.Status)
	}
}
