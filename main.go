package main

import (
	"log"
	"time"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.Fatal("main not wired yet — see Phase 7 of the plan")
}

func statusLogger(state *RobotState) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		snap := state.Snapshot()
		if snap.LastUpdate.IsZero() {
			log.Println("[Status] No data from robot yet")
		} else {
			log.Printf("[Status] %s (last update: %v ago)",
				FormatStateLog(snap),
				time.Since(snap.LastUpdate).Round(time.Second))
		}
	}
}
