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

func TestLoadConfigForRobot_PortDefaults(t *testing.T) {
	os.Clearenv()
	os.Setenv("ROBOT_PORT", "8802")
	srv := LoadServerConfig()
	if srv.RobotPort != "8802" {
		t.Fatalf("RobotPort = %q, want 8802", srv.RobotPort)
	}

	// atomBaseURL without a port → ROBOT_PORT is used; FastAPI defaults to the
	// same host:port as the ATOM API.
	cfg := LoadConfigForRobot(RobotRecord{ID: "r1", AtomBaseURL: "http://10.0.0.5"}, srv)
	if cfg.RobotIP != "10.0.0.5" || cfg.RobotPort != "8802" {
		t.Errorf("IP/Port = %q/%q, want 10.0.0.5/8802", cfg.RobotIP, cfg.RobotPort)
	}
	if cfg.RobotFastAPI != "http://10.0.0.5:8802" {
		t.Errorf("RobotFastAPI = %q, want http://10.0.0.5:8802", cfg.RobotFastAPI)
	}

	// An explicit port in atomBaseURL wins over ROBOT_PORT.
	cfg = LoadConfigForRobot(RobotRecord{ID: "r2", AtomBaseURL: "http://10.0.0.6:9000"}, srv)
	if cfg.RobotPort != "9000" || cfg.RobotFastAPI != "http://10.0.0.6:9000" {
		t.Errorf("explicit port: Port=%q FastAPI=%q", cfg.RobotPort, cfg.RobotFastAPI)
	}
}
