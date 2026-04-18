package main

import (
	"log"
	"net"
	"net/http"
	"os/exec"
	"time"
)

// StartUSBWatchdog monitors connectivity to the robot.
// Only reloads g_ether after 3 minutes of unreachable + 5-minute debounce —
// short flakes self-heal; aggressive reload destroys a working link.
func StartUSBWatchdog(robotIP string) {
	go func() {
		time.Sleep(10 * time.Second)

		var firstUnreachableAt time.Time
		var lastReloadAt time.Time
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

			unreachableFor := now.Sub(firstUnreachableAt)
			if unreachableFor < 3*time.Minute {
				continue
			}

			if time.Since(lastReloadAt) < 5*time.Minute {
				continue
			}

			log.Printf("[USB] Robot %s unreachable for %.0fs, reloading g_ether...",
				robotIP, unreachableFor.Seconds())
			reloadGEther()
			lastReloadAt = now
			firstUnreachableAt = time.Time{}
		}
	}()
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
