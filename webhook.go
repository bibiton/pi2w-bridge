package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

type WebhookServer struct {
	state  *RobotState
	cfg    *Config
	server *http.Server
	addr   string
}

func NewWebhookServer(addr string, state *RobotState, cfg *Config) *WebhookServer {
	return &WebhookServer{
		state: state,
		cfg:   cfg,
		addr:  addr,
	}
}

func (ws *WebhookServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", ws.handleWebhook)
	mux.HandleFunc("/health", ws.handleHealth)

	ws.server = &http.Server{
		Addr:         ws.addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	log.Printf("[Webhook] Starting server on %s", ws.addr)
	go func() {
		if err := ws.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[Webhook] Server error: %v", err)
		}
	}()
	return nil
}

func (ws *WebhookServer) Stop() {
	if ws.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		ws.server.Shutdown(ctx)
	}
}

func (ws *WebhookServer) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "pi2w-bridge ok")
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
	if err != nil {
		http.Error(w, "Read error", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if len(body) == 0 {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
		return
	}

	// Parse JSON — can be a single object or an array
	var items []map[string]interface{}

	var raw json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &items); err != nil {
			http.Error(w, "Invalid JSON array", http.StatusBadRequest)
			return
		}
	} else {
		var single map[string]interface{}
		if err := json.Unmarshal(raw, &single); err != nil {
			http.Error(w, "Invalid JSON object", http.StatusBadRequest)
			return
		}
		items = []map[string]interface{}{single}
	}

	// Debug: log webhook pose data
	for _, item := range items {
		if pose, ok := item["pose"]; ok {
			poseJSON, _ := json.Marshal(pose)
			log.Printf("[Webhook] pose data: %s", string(poseJSON))
		}
	}

	// Apply each item to state
	for _, item := range items {
		ApplyWebhookData(ws.state, item)
		// If map_loaded status received, refresh mapId from ATOM API
		if ws.cfg != nil {
			if rs, ok := item["route_status"].(map[string]interface{}); ok {
				s, _ := rs["status"].(string)
				log.Printf("[Webhook] route_status=%s", s)
				if s == "map_loaded" {
					log.Printf("[Webhook] map_loaded event received, notifying MapLoadedCh")
					ws.state.NotifyMapLoaded()
					go func() {
						if name := queryATOMCurrentMap(ws.cfg); name != "" {
							ws.state.SetMapID(name)
							log.Printf("[Webhook] Map updated from ATOM API: %s", name)
						}
					}()
				}
			}
		}
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}

func (ws *WebhookServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fresh := ws.state.IsDataFresh(10 * time.Second)
	status := "healthy"
	if !fresh {
		status = "no_data"
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     status,
		"data_fresh": fresh,
	})
}
