package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestDoRegister(t *testing.T) {
	t.Parallel()
	var gotURL string
	var gotReportMode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/service/control/commands" {
			w.WriteHeader(404)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		_ = json.Unmarshal(body, &m)
		gotURL, _ = m["webhook_url"].(string)
		gotReportMode, _ = m["report mode"].(string)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if err := doRegister(srv.URL, "http://1.2.3.4:5201/"); err != nil {
		t.Fatalf("doRegister: %v", err)
	}
	if gotURL != "http://1.2.3.4:5201/" {
		t.Errorf("robot received webhook_url=%q, want http://1.2.3.4:5201/", gotURL)
	}
	if gotReportMode != "repeat" {
		t.Errorf("report mode = %q, want repeat", gotReportMode)
	}
}

func TestDoRegister_HTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	if err := doRegister(srv.URL, "http://x/"); err == nil {
		t.Errorf("expected error on HTTP 503")
	}
}

func TestGetLocalIP_EnvOverride(t *testing.T) {
	t.Setenv("LOCAL_IP", "192.168.99.99")
	if got := getLocalIP("10.0.0.1"); got != "192.168.99.99" {
		t.Errorf("getLocalIP with LOCAL_IP env = %q, want 192.168.99.99", got)
	}
	os.Unsetenv("LOCAL_IP")
	// Without env: dials a UDP "connection" to the robot IP. Even for an
	// unroutable address this returns a local interface IP (or 127.0.0.1 fallback).
	got := getLocalIP("10.255.255.1")
	if got == "" {
		t.Errorf("getLocalIP returned empty string")
	}
}

func TestReadLastmap(t *testing.T) {
	t.Run("env override", func(t *testing.T) {
		t.Setenv("LASTMAP_NAME", "envmap")
		if got := readLastmap(&Config{}); got != "envmap" {
			t.Errorf("readLastmap with LASTMAP_NAME = %q", got)
		}
	})
	t.Run("from ATOM API", func(t *testing.T) {
		os.Unsetenv("LASTMAP_NAME")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"current map name":"apimap"}`))
		}))
		defer srv.Close()
		if got := readLastmap(testCfg(srv)); got != "apimap" {
			t.Errorf("readLastmap from API = %q, want apimap", got)
		}
	})
}

func TestGetRobotMode(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"robot_mode":"delivery"}`))
	}))
	defer srv.Close()
	if got := getRobotMode(srv.URL, &http.Client{}); got != "delivery" {
		t.Errorf("getRobotMode = %q, want delivery", got)
	}
}

func TestSleepOrStop(t *testing.T) {
	t.Parallel()
	stop := make(chan struct{})
	close(stop)
	if sleepOrStop(time.Hour, stop) {
		t.Errorf("sleepOrStop should return false when stopCh is closed")
	}
	if !sleepOrStop(time.Millisecond, make(chan struct{})) {
		t.Errorf("sleepOrStop should return true when the timer fires")
	}
}
