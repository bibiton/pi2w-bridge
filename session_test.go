package main

import "testing"

func TestNewRobotSession_Wires(t *testing.T) {
	srv := &ServerConfig{MQTTBroker: "tcp://127.0.0.1:1", MQTTPrefix: "/uagv/v2",
		Manufacturer: "atom", DefaultRobotSecret: "s", PublicBaseURL: "http://localhost:5201"}
	rec := RobotRecord{ID: "adai01", Serial: "adai01",
		AtomBaseURL: "http://127.0.0.1:18080", FastAPIHTTPURL: "http://127.0.0.1:18000",
		FastAPIWSURL: "ws://127.0.0.1:18000/ws"}
	sess := NewRobotSession(rec, srv, nil) // nil store ok for construction
	if sess == nil || sess.ID() != "adai01" {
		t.Fatalf("session not wired: %+v", sess)
	}
	if sess.cfg.RobotPort != "18080" {
		t.Errorf("cfg not derived: %+v", sess.cfg)
	}
	sess.Start()
	sess.Stop()
}
