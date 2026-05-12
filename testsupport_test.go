package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
)

// testCfg returns a *Config wired to point at the given fake-robot HTTP server
// (used as both the ATOM API and the FastAPI endpoint). If server is nil the
// robot endpoints point at an unroutable address.
func testCfg(server *httptest.Server) *Config {
	c := &Config{
		RobotIP:      "127.0.0.1",
		RobotPort:    "1",
		MQTTPrefix:   "/uagv/v2",
		Manufacturer: "atom",
		SerialNumber: "testbot",
	}
	if server != nil {
		u, _ := url.Parse(server.URL)
		c.RobotIP = u.Hostname()
		c.RobotPort = u.Port()
		c.RobotFastAPI = server.URL
	}
	return c
}

// newTestOrderHandler builds an OrderHandler with non-connected MQTT/WS deps.
func newTestOrderHandler(t *testing.T, cfg *Config, store *Store) (*OrderHandler, *RobotState) {
	t.Helper()
	state := NewRobotState()
	ms := NewMapService(cfg)
	ws := NewRobotWSClient(cfg, state)
	bridge := NewMQTTBridge(cfg, state, ms, ws, store)
	oh := NewOrderHandler(cfg, state, bridge, ws, store)
	return oh, state
}

// readOrderStatus returns (status, errorRef) for an order row.
func readOrderStatus(t *testing.T, s *Store, orderID string) (string, string) {
	t.Helper()
	var status, errRef string
	err := s.db.QueryRow(`SELECT status, error_ref FROM orders WHERE order_id=?`, orderID).Scan(&status, &errRef)
	if err != nil {
		t.Fatalf("readOrderStatus(%s): %v", orderID, err)
	}
	return status, errRef
}

// drainBody reads and closes an http.Response body (for handlers that ignore it).
func drainBody(r *http.Response) {
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
}

func TestMain(m *testing.M) {
	// Tests log a lot via the standard logger; keep output quiet but valid.
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}
