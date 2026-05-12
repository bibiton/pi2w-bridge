package main

import (
	"os"
	"testing"
)

func TestLoadServerConfig_Defaults(t *testing.T) {
	os.Clearenv()
	c := LoadServerConfig()
	if c.ListenAddr != ":5201" {
		t.Errorf("ListenAddr = %q, want :5201", c.ListenAddr)
	}
	if c.MQTTBroker == "" || c.MQTTUser == "" || c.MQTTPass == "" {
		t.Errorf("MQTT defaults must be non-empty: %+v", c)
	}
	if c.AdminToken == "" {
		t.Errorf("AdminToken default must be non-empty")
	}
	if c.DBPath == "" {
		t.Errorf("DBPath default must be non-empty")
	}
}

func TestLoadServerConfig_EnvOverride(t *testing.T) {
	os.Clearenv()
	os.Setenv("MQTT_BROKER", "wss://example/mqtt")
	os.Setenv("ADMIN_TOKEN", "tok123")
	c := LoadServerConfig()
	if c.MQTTBroker != "wss://example/mqtt" {
		t.Errorf("MQTTBroker not overridden: %q", c.MQTTBroker)
	}
	if c.AdminToken != "tok123" {
		t.Errorf("AdminToken not overridden: %q", c.AdminToken)
	}
}

func TestLoadConfigForRobot(t *testing.T) {
	os.Clearenv()
	srv := LoadServerConfig()
	rec := RobotRecord{
		ID: "adai01", Manufacturer: "atom", Serial: "adai01",
		AtomBaseURL: "http://1.2.3.4:8080", FastAPIHTTPURL: "http://1.2.3.4:8000",
		FastAPIWSURL: "ws://1.2.3.4:8000/ws", WebhookSecret: "s3cr3t",
	}
	cfg := LoadConfigForRobot(rec, srv)
	if cfg.RobotIP != "1.2.3.4" || cfg.RobotPort != "8080" {
		t.Errorf("RobotIP/Port wrong: %q %q", cfg.RobotIP, cfg.RobotPort)
	}
	if cfg.RobotFastAPI != "http://1.2.3.4:8000" {
		t.Errorf("RobotFastAPI wrong: %q", cfg.RobotFastAPI)
	}
	if cfg.Manufacturer != "atom" || cfg.SerialNumber != "adai01" {
		t.Errorf("identity wrong: %+v", cfg)
	}
	if cfg.MQTTBroker != srv.MQTTBroker {
		t.Errorf("MQTT broker should come from server config")
	}
}
