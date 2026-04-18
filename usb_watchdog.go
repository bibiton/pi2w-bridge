package main

import (
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Shared reload state so multiple watchdogs don't stomp on each other.
var (
	reloadMu     sync.Mutex
	lastReloadAt time.Time
)

// safeReloadGEther performs a reload but returns immediately (without
// reloading) if one already ran within minInterval. Thread-safe.
func safeReloadGEther(reason string, minInterval time.Duration) bool {
	reloadMu.Lock()
	defer reloadMu.Unlock()
	if !lastReloadAt.IsZero() && time.Since(lastReloadAt) < minInterval {
		return false
	}
	log.Printf("[USB] Hot-plugging g_ether — %s", reason)
	reloadGEther()
	lastReloadAt = time.Now()
	return true
}

// StartUSBWatchdog monitors TCP reachability to the robot.
// Triggers reload after 3 minutes of unreachable (long-term safety net).
// Fast packet-level recovery is handled by StartUSBLinkWatchdog.
func StartUSBWatchdog(robotIP string) {
	go func() {
		time.Sleep(10 * time.Second)

		var firstUnreachableAt time.Time
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			if isRobotReachable(robotIP) {
				if !firstUnreachableAt.IsZero() {
					log.Printf("[USB] Robot %s reachable again", robotIP)
				}
				firstUnreachableAt = time.Time{}
				continue
			}

			now := time.Now()
			if firstUnreachableAt.IsZero() {
				firstUnreachableAt = now
				log.Printf("[USB] Robot %s unreachable, monitoring...", robotIP)
				continue
			}

			if now.Sub(firstUnreachableAt) < 3*time.Minute {
				continue
			}

			if safeReloadGEther(
				"TCP unreachable "+time.Since(firstUnreachableAt).Truncate(time.Second).String(),
				5*time.Minute,
			) {
				firstUnreachableAt = time.Time{}
			}
		}
	}()
}

// StartUSBLinkWatchdog watches usb0 RX packet counter. If no new RX packets
// arrive for rxStaleThreshold, hot-plug g_ether to force host re-enumerate.
// This catches the "link up but host-side dead" scenario that TCP probe
// is too slow to detect.
func StartUSBLinkWatchdog() {
	const (
		pollInterval     = 3 * time.Second
		rxStaleThreshold = 15 * time.Second
		reloadCooldown   = 45 * time.Second
		warmup           = 20 * time.Second
	)

	go func() {
		time.Sleep(warmup)

		var lastRX uint64
		haveBaseline := false
		lastActivityAt := time.Now()

		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()

		for range ticker.C {
			rx, ok := readUSB0RXPackets()
			if !ok {
				// usb0 gone (mid-reload or unplugged) — reset baseline.
				haveBaseline = false
				lastActivityAt = time.Now()
				continue
			}

			if !haveBaseline {
				lastRX = rx
				haveBaseline = true
				lastActivityAt = time.Now()
				continue
			}

			if rx != lastRX {
				lastRX = rx
				lastActivityAt = time.Now()
				continue
			}

			stale := time.Since(lastActivityAt)
			if stale < rxStaleThreshold {
				continue
			}

			if safeReloadGEther(
				"usb0 RX frozen "+stale.Truncate(time.Second).String(),
				reloadCooldown,
			) {
				// Re-baseline after reload — usb0 interface was recreated.
				haveBaseline = false
				lastActivityAt = time.Now()
			}
		}
	}()
}

func readUSB0RXPackets() (uint64, bool) {
	data, err := os.ReadFile("/sys/class/net/usb0/statistics/rx_packets")
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// isRobotReachable checks if the robot is reachable via TCP connect to port 8080.
func isRobotReachable(robotIP string) bool {
	conn, err := net.DialTimeout("tcp", robotIP+":8080", 3*time.Second)
	if err == nil {
		conn.Close()
		return true
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + robotIP + ":8080/")
	if err == nil {
		resp.Body.Close()
		return true
	}

	return false
}

// reloadGEther reloads the g_ether USB gadget kernel module on Pi,
// then lets NetworkManager reapply the usb0 profile (robot USB host expects
// Pi = 192.168.2.1/24). Static fallback only if NM fails to set an IPv4.
// Do NOT hardcode MAC: kernel derives deterministic MAC from board serial.
// Do NOT hardcode a wrong subnet: watchdog must not fight NM's profile.
func reloadGEther() {
	script := `
set +e
modprobe -r g_ether 2>/dev/null
sleep 1
modprobe g_ether
sleep 2
if command -v nmcli >/dev/null 2>&1; then
  nmcli device reapply usb0 2>/dev/null || nmcli connection up ifname usb0 2>/dev/null || true
fi
sleep 2
if ! ip -4 addr show dev usb0 2>/dev/null | grep -q "inet "; then
  ip link set usb0 up
  ip addr add 192.168.2.1/24 dev usb0
  echo "[usb-fallback] applied static 192.168.2.1/24"
fi
`
	cmd := exec.Command("sudo", "bash", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[USB] g_ether reload failed: %v output=%s", err, string(out))
	} else {
		log.Printf("[USB] g_ether reloaded: %s", string(out))
	}
}
