package main

import (
	"context"
	"log"
	"os/exec"
	"strings"
	"time"
)

// StartNetworkSnapshotLogger periodically dumps Pi's network state.
// Every snapshot is one big multi-line log entry tagged [NET-SNAPSHOT].
// Survives reboot via persistent journald; after a power cycle, read with
// `journalctl -b -1 -u pi2w-bridge | grep NET-SNAPSHOT` to see the prior
// session's state — essential when debugging USB-side failures where
// remote access is only available in the "working" configuration.
func StartNetworkSnapshotLogger() {
	const interval = 30 * time.Second

	go func() {
		snapshotNetwork("startup")

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			snapshotNetwork("periodic")
		}
	}()
}

func snapshotNetwork(reason string) {
	var b strings.Builder
	b.WriteString("\n====== NET-SNAPSHOT (" + reason + ") ======\n")

	probes := []struct{ label, cmd string }{
		{"addr", "ip -br addr"},
		{"route", "ip route"},
		{"resolv", "cat /etc/resolv.conf"},
		{"nm-dev", "nmcli -t device status"},
		{"usb-udc", "sh -c 'for d in /sys/class/udc/*; do echo -n \"$d state=\"; cat $d/state 2>/dev/null; done'"},
		{"usb0-stats", "sh -c 'cat /sys/class/net/usb0/operstate /sys/class/net/usb0/statistics/rx_packets /sys/class/net/usb0/statistics/tx_packets 2>/dev/null | paste -sd\" \" -'"},
		{"cloudflared", "systemctl is-active cloudflared-tunnel"},
		{"tunnel-url", "sh -c 'journalctl -u cloudflared-tunnel --no-pager -n 200 2>/dev/null | grep -oE \"https://[a-z0-9-]+\\.trycloudflare\\.com\" | tail -1'"},
		{"reach-1.1.1.1", "sh -c 'ping -c 1 -W 2 1.1.1.1 >/dev/null 2>&1 && echo OK || echo FAIL'"},
		{"reach-robot", "sh -c 'ping -c 1 -W 2 192.168.2.100 >/dev/null 2>&1 && echo OK || echo FAIL'"},
	}

	for _, p := range probes {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		out, _ := exec.CommandContext(ctx, "bash", "-c", p.cmd).CombinedOutput()
		cancel()
		b.WriteString(p.label)
		b.WriteString(": ")
		s := strings.TrimSpace(string(out))
		if strings.Contains(s, "\n") {
			b.WriteString("\n")
			b.WriteString(s)
			b.WriteString("\n")
		} else {
			b.WriteString(s)
			b.WriteString("\n")
		}
	}
	b.WriteString("====== /NET-SNAPSHOT ======")
	log.Printf("[NET-SNAPSHOT] %s", b.String())
}
