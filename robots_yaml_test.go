package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRobotsYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "robots.yaml")
	os.WriteFile(p, []byte(`
robots:
  - id: adai01
    serial: adai01
    atomBaseURL: http://1.2.3.4:8080
    fastapiHTTPURL: http://1.2.3.4:8000
    fastapiWSURL: ws://1.2.3.4:8000/ws
  - id: adai02
    atomBaseURL: http://1.2.3.5:8080
`), 0644)
	recs, err := LoadRobotsYAML(p)
	if err != nil {
		t.Fatalf("LoadRobotsYAML: %v", err)
	}
	if len(recs) != 2 || recs[0].ID != "adai01" || recs[1].ID != "adai02" {
		t.Fatalf("parsed wrong: %+v", recs)
	}
	if recs[0].Source != "yaml" {
		t.Errorf("expected Source=yaml, got %q", recs[0].Source)
	}
}

func TestRobotRecordChanged(t *testing.T) {
	base := RobotRecord{
		ID: "r1", Manufacturer: "m", Serial: "s",
		AtomBaseURL: "http://1.1.1.1:8080", FastAPIHTTPURL: "http://1.1.1.1:8000",
		FastAPIWSURL: "ws://1.1.1.1:8000/ws", WebhookSecret: "secret",
	}
	// Identical except ignored fields → no change.
	other := base
	other.Status = "errored"
	other.Source = "yaml"
	other.LastSeenAt = 12345
	if robotRecordChanged(base, other) {
		t.Errorf("expected no change when only Status/Source/LastSeenAt differ")
	}
	// Connection-relevant field changed → change.
	other = base
	other.AtomBaseURL = "http://2.2.2.2:8080"
	if !robotRecordChanged(base, other) {
		t.Errorf("expected change when AtomBaseURL differs")
	}
	other = base
	other.WebhookSecret = "new"
	if !robotRecordChanged(base, other) {
		t.Errorf("expected change when WebhookSecret differs")
	}
}

func TestLoadRobotsYAML_Missing(t *testing.T) {
	recs, err := LoadRobotsYAML("/nonexistent/robots.yaml")
	if err != nil || len(recs) != 0 {
		t.Fatalf("missing file should be (nil,nil): %v %v", recs, err)
	}
}
