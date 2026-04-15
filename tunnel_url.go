package main

import (
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// TunnelURLWatcher monitors cloudflared journal for the quick tunnel URL
// and publishes it to MQTT state informations when it changes.
type TunnelURLWatcher struct {
	mu  sync.RWMutex
	url string
}

var tunnelWatcher = &TunnelURLWatcher{}

// GetTunnelURL returns the current tunnel URL (empty if not available).
func GetTunnelURL() string {
	tunnelWatcher.mu.RLock()
	defer tunnelWatcher.mu.RUnlock()
	return tunnelWatcher.url
}

// StartTunnelURLWatcher polls journalctl for the cloudflared tunnel URL.
// Checks every 30 seconds. When URL changes, immediately publishes MQTT state.
func StartTunnelURLWatcher(mb *MQTTBridge) {
	go func() {
		for {
			url := fetchTunnelURL()
			if url != "" {
				tunnelWatcher.mu.Lock()
				old := tunnelWatcher.url
				tunnelWatcher.url = url
				tunnelWatcher.mu.Unlock()

				if url != old {
					log.Printf("[Tunnel] URL changed: %s -> %s", old, url)
					if mb != nil {
						mb.TriggerStatePublish()
						log.Printf("[Tunnel] Published MQTT state with new URL")
					}
				}
			}
			time.Sleep(30 * time.Second)
		}
	}()
}

func fetchTunnelURL() string {
	out, err := exec.Command("journalctl", "-u", "cloudflared-tunnel", "--no-pager", "-n", "50").Output()
	if err != nil {
		return ""
	}
	// Find the last trycloudflare URL in logs
	lines := strings.Split(string(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if idx := strings.Index(lines[i], "https://"); idx >= 0 {
			rest := lines[i][idx:]
			if end := strings.IndexAny(rest, " \t\n\r\"'"); end > 0 {
				rest = rest[:end]
			}
			if strings.Contains(rest, ".trycloudflare.com") {
				return rest
			}
		}
	}
	return ""
}
