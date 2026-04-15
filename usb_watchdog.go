package main

import (
	"log"
	"net"
	"net/http"
	"os/exec"
	"time"
)

// StartUSBWatchdog monitors connectivity to the robot.
// If robot is unreachable for over 1 minute, reload g_ether USB gadget
// to re-establish Ethernet-over-USB link (simulates USB replug).
// Debounces reloads to at most once every 2 minutes.
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

			// Robot unreachable
			now := time.Now()
			if firstUnreachableAt.IsZero() {
				firstUnreachableAt = now
				log.Printf("[USB] Robot %s unreachable, monitoring...", robotIP)
				continue
			}

			unreachableFor := now.Sub(firstUnreachableAt)
			if unreachableFor < 1*time.Minute {
				continue // not yet 1 minute
			}

			if time.Since(lastReloadAt) < 2*time.Minute {
				continue // debounce
			}

			log.Printf("[USB] Robot %s unreachable for %.0fs, reloading g_ether...",
				robotIP, unreachableFor.Seconds())
			reloadGEther()
			lastReloadAt = now
			firstUnreachableAt = time.Time{} // reset counter
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

// reloadGEther reloads the g_ether USB gadget kernel module on Pi.
func reloadGEther() {
	script := `modprobe -r g_ether 2>/dev/null; sleep 1; modprobe g_ether host_addr=48:6F:73:74:50:43 dev_addr=42:61:64:55:53:42; sleep 2; ifconfig usb0 192.168.168.169 netmask 255.255.255.0 up`
	cmd := exec.Command("sudo", "bash", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[USB] g_ether reload failed: %v output=%s", err, string(out))
	} else {
		log.Printf("[USB] g_ether reloaded successfully")
	}
}
