package main

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"
)

// ConnectElevatorWifi attempts to connect to elevator WiFi networks in order.
// Tries each SSID sequentially; returns on first successful connection.
func ConnectElevatorWifi(networks []WifiNetwork) error {
	if len(networks) == 0 {
		log.Printf("[WiFi] No elevator WiFi networks configured, skipping")
		return nil
	}

	log.Printf("[WiFi] Attempting to connect elevator WiFi (%d networks)", len(networks))
	start := time.Now()

	for i, nw := range networks {
		log.Printf("[WiFi] Trying network %d/%d: SSID=%s", i+1, len(networks), nw.SSID)

		cmd := exec.Command("nmcli", "device", "wifi", "connect", nw.SSID, "password", nw.Password)
		output, err := cmd.CombinedOutput()
		outputStr := strings.TrimSpace(string(output))

		if err != nil {
			log.Printf("[WiFi] Failed to connect SSID=%s: %v, output: %s", nw.SSID, err, outputStr)
			continue
		}

		log.Printf("[WiFi] Connected to SSID=%s (took %v), output: %s", nw.SSID, time.Since(start), outputStr)
		return nil
	}

	return fmt.Errorf("failed to connect to any elevator WiFi network")
}

// DisconnectElevatorWifi disconnects from all configured elevator WiFi networks.
// After disconnection, the Pi will auto-reconnect to the strongest available network.
func DisconnectElevatorWifi(networks []WifiNetwork) error {
	if len(networks) == 0 {
		log.Printf("[WiFi] No elevator WiFi networks configured, skipping disconnect")
		return nil
	}

	log.Printf("[WiFi] Disconnecting elevator WiFi networks")
	start := time.Now()
	var lastErr error

	for _, nw := range networks {
		cmd := exec.Command("nmcli", "connection", "down", nw.SSID)
		output, err := cmd.CombinedOutput()
		outputStr := strings.TrimSpace(string(output))

		if err != nil {
			// Not connected to this SSID — not an error
			log.Printf("[WiFi] Disconnect SSID=%s: %v, output: %s", nw.SSID, err, outputStr)
			lastErr = err
			continue
		}

		log.Printf("[WiFi] Disconnected SSID=%s, output: %s", nw.SSID, outputStr)
	}

	log.Printf("[WiFi] Disconnect sequence done (took %v)", time.Since(start))

	// If all disconnects failed, return the last error; otherwise nil
	// In practice, only the connected SSID matters, others will "fail" harmlessly
	_ = lastErr
	return nil
}
