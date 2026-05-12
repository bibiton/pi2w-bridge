package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.Println("=== Pi2W Cloud Multi-Robot Bridge ===")

	srv := LoadServerConfig()
	log.Printf("[Config] listen=%s public=%s db=%s mqtt=%s prefix=%s",
		srv.ListenAddr, srv.PublicBaseURL, srv.DBPath, srv.MQTTBroker, srv.MQTTPrefix)

	store, err := OpenStore(srv.DBPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if n, err := store.FailRunningOrders("bridge_restarted"); err == nil && n > 0 {
		log.Printf("[Main] marked %d in-flight order(s) failed (bridge_restarted)", n)
	}

	mgr := NewSessionManager(srv, store)
	mgr.LoadFromStore()

	appStop := make(chan struct{})
	robotsYAMLPath := envOrDefault("ROBOTS_YAML", "robots.yaml")
	WatchRobotsYAML(robotsYAMLPath, mgr, store, appStop)

	api := NewAPIServer(srv, mgr, store)
	if err := api.Start(); err != nil {
		log.Fatalf("api start: %v", err)
	}

	log.Println("[Main] up. Managing", len(mgr.List()), "robot session(s).")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("[Main] shutting down...")
	close(appStop)
	api.Stop()
	mgr.StopAll()
	log.Println("[Main] bye")
}
