package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.Println("=== Pi 2W VDA5050 Bridge ===")

	// 1. Load config
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	log.Printf("[Config] Robot: %s:%s, MQTT: %s, Identity: %s/%s",
		cfg.RobotIP, cfg.RobotPort, cfg.MQTTBroker, cfg.Manufacturer, cfg.SerialNumber)
	log.Printf("[Config] Topic prefix: %s", cfg.TopicPrefix())

	// 2. Create robot state
	state := NewRobotState()

	// 3. Load elevator config (optional — for multi-floor navigation)
	elevatorCfg, err := LoadElevatorConfig("elevator_config.json")
	if err != nil {
		log.Fatalf("Failed to load elevator config: %v", err)
	}

	// 4. Create map service (uses robot FastAPI :8000 + ATOM API :8080)
	mapService := NewMapService(cfg)

	// 4. Connect WebSocket to robot FastAPI (port 8000)
	robotWS := NewRobotWSClient(cfg, state)
	robotWS.Start()

	// 5. Create and connect MQTT bridge
	mqttBridge := NewMQTTBridge(cfg, state, mapService, robotWS, elevatorCfg)
	if err := mqttBridge.Connect(); err != nil {
		log.Printf("[MQTT] Initial connect: %v (will keep retrying)", err)
	}

	// 6. Start webhook server
	webhookServer := NewWebhookServer(cfg.ListenAddr, state, cfg)
	if err := webhookServer.Start(); err != nil {
		log.Fatalf("Webhook server failed: %v", err)
	}
	log.Printf("[Webhook] Listening on %s", cfg.ListenAddr)

	// 6. Register webhook with robot
	RegisterWebhook(cfg, cfg.ListenAddr)

	// 7. Fetch initial map ID from ATOM API
	go FetchInitialMapID(mapService, state, cfg)

	// 8. Start map list update loop (every 5 minutes)
	StartMapListLoop(mapService, state)

	// 9. Start tunnel URL watcher
	StartTunnelURLWatcher(mqttBridge)

	// 10. Start MQTT publish loops
	mqttBridge.StartPublishLoops()

	// 10b. Start elevator service (discovery + status monitoring via IoT Gateway)
	elevatorSvc := NewElevatorService(mqttBridge, cfg)
	mqttBridge.elevatorService = elevatorSvc
	elevatorSvc.Start()
	log.Println("[Main] Elevator service started (discovery + status monitoring)")

	// 11. Start USB watchdogs
	// - TCP probe: long-term safety net (3 min unreachable → reload)
	// - Link probe: fast hot-plug if usb0 RX is frozen for 15s
	StartUSBWatchdog(cfg.RobotIP)
	StartUSBLinkWatchdog()

	// 12. Start status logging
	go statusLogger(state)

	log.Println("[Main] All systems started. Waiting for signal...")

	// Wait for termination signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("[Main] Shutting down...")
	elevatorSvc.Stop()
	robotWS.Stop()
	webhookServer.Stop()
	mqttBridge.Stop()
	log.Println("[Main] Goodbye!")
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
