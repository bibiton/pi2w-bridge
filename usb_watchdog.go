package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
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

// StartUSBLinkWatchdog actively ARP-probes the robot on usb0 and reacts fast:
//   - ARP reply from robot → healthy
//   - ARP silent + usb0 RX also frozen ≥15s → gadget stuck, hot-plug
//   - ARP silent but usb0 RX is active → wrong peer on the cable
//     (e.g. laptop instead of robot). Log a warning; don't reload —
//     reloading won't change what's on the other end of the cable.
func StartUSBLinkWatchdog(robotIP string) {
	const (
		pollInterval      = 5 * time.Second
		rxSilentThreshold = 15 * time.Second
		arpTimeout        = 2 * time.Second
		reloadCooldown    = 45 * time.Second
		warmup            = 20 * time.Second
		wrongPeerCooldown = 60 * time.Second
	)

	go func() {
		time.Sleep(warmup)

		var lastRX uint64
		haveBaseline := false
		lastRXChangeAt := time.Now()
		var lastWrongPeerLogAt time.Time

		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()

		for range ticker.C {
			rx, rxOK := readUSB0RXPackets()
			if rxOK {
				if !haveBaseline {
					lastRX = rx
					haveBaseline = true
					lastRXChangeAt = time.Now()
				} else if rx != lastRX {
					lastRX = rx
					lastRXChangeAt = time.Now()
				}
			} else {
				haveBaseline = false
			}

			if ok, _ := arpProbe("usb0", robotIP, arpTimeout); ok {
				continue
			}

			rxSilent := !rxOK || time.Since(lastRXChangeAt) >= rxSilentThreshold

			if !rxSilent {
				if time.Since(lastWrongPeerLogAt) < wrongPeerCooldown {
					continue
				}
				peer := detectForeignPeer("usb0", 3*time.Second)
				if peer == "" {
					continue
				}
				log.Printf("[USB] WARNING: usb0 has traffic but robot %s did not reply. Foreign peer detected: %s. Check the USB cable — it may be plugged into the wrong host.",
					robotIP, peer)
				lastWrongPeerLogAt = time.Now()
				continue
			}

			reason := fmt.Sprintf("ARP to %s no reply; usb0 RX frozen %s",
				robotIP, time.Since(lastRXChangeAt).Truncate(time.Second))
			if safeReloadGEther(reason, reloadCooldown) {
				haveBaseline = false
				lastRXChangeAt = time.Now()
			}
		}
	}()
}

// arpProbe sends a single L2 ARP request on iface asking for ip, waits up to
// timeout for a reply, and returns (true, peerMAC) on success.
// Relies on iputils arping being in $PATH.
func arpProbe(iface, ip string, timeout time.Duration) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout+time.Second)
	defer cancel()

	secs := int(timeout.Seconds())
	if secs < 1 {
		secs = 1
	}
	cmd := exec.CommandContext(ctx, "arping",
		"-I", iface, "-c", "1", "-w", strconv.Itoa(secs), "-f", ip)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, ""
	}
	m := regexp.MustCompile(`\[([0-9A-Fa-f:]{11,17})\]`).FindStringSubmatch(string(out))
	if len(m) > 1 {
		return true, m[1]
	}
	return false, ""
}

// detectForeignPeer briefly sniffs iface and returns the first source IP
// seen whose address is NOT on 192.168.2.0/24 (robot subnet). Returns ""
// if nothing relevant is seen in `window`.
func detectForeignPeer(iface string, window time.Duration) string {
	secs := int(window.Seconds())
	if secs < 1 {
		secs = 1
	}
	cmd := exec.Command("timeout", strconv.Itoa(secs),
		"tcpdump", "-i", iface, "-n", "-c", "5", "-p",
		"ip", "and", "not", "net", "192.168.2.0/24")
	out, _ := cmd.CombinedOutput()

	re := regexp.MustCompile(`IP (\d+\.\d+\.\d+\.\d+)\.\d+ >`)
	for _, line := range strings.Split(string(out), "\n") {
		if m := re.FindStringSubmatch(line); len(m) > 1 {
			return m[1]
		}
	}
	return ""
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
ip link set usb0 up 2>/dev/null
if ! ip -4 addr show dev usb0 2>/dev/null | grep -q "inet "; then
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
