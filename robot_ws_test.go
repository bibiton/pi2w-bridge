package main

import (
	"strings"
	"testing"
)

func TestRobotWSClient_Disabled(t *testing.T) {
	for _, sentinel := range []string{"disabled", "DISABLED", " none ", "off", "-"} {
		cfg := &Config{RobotIP: "1.2.3.4", RobotPort: "8080", RobotFastAPI: "http://1.2.3.4:8080", RobotFastAPIWS: sentinel}
		c := NewRobotWSClient(cfg, NewRobotState())
		if !c.disabled {
			t.Fatalf("RobotFastAPIWS=%q: expected disabled", sentinel)
		}
		c.Start() // must be a no-op, not spawn a reconnect loop
		if c.IsConnected() {
			t.Fatalf("RobotFastAPIWS=%q: must not be connected", sentinel)
		}
		if err := c.SetInitialPose(1, 2, 3); err == nil || !strings.Contains(err.Error(), "disabled") {
			t.Fatalf("RobotFastAPIWS=%q: SetInitialPose err = %v, want a 'disabled' error", sentinel, err)
		}
		c.Stop()
	}
}

func TestRobotWSClient_DerivesURL(t *testing.T) {
	// Empty WS URL → derived from RobotFastAPI host:port.
	c := NewRobotWSClient(&Config{RobotIP: "1.2.3.4", RobotPort: "8802", RobotFastAPI: "http://1.2.3.4:8802"}, NewRobotState())
	if c.disabled {
		t.Fatal("should not be disabled")
	}
	if c.url != "ws://1.2.3.4:8802/ws" {
		t.Fatalf("derived url = %q, want ws://1.2.3.4:8802/ws", c.url)
	}
	// Explicit WS URL is used verbatim.
	c2 := NewRobotWSClient(&Config{RobotFastAPIWS: "ws://5.6.7.8:9000/socket"}, NewRobotState())
	if c2.url != "ws://5.6.7.8:9000/socket" {
		t.Fatalf("explicit url = %q", c2.url)
	}
}
