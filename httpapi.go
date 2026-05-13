package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type APIServer struct {
	srv    *ServerConfig
	mgr    *SessionManager
	store  *Store
	mux    *http.ServeMux
	server *http.Server
}

func NewAPIServer(srv *ServerConfig, mgr *SessionManager, store *Store) *APIServer {
	a := &APIServer{srv: srv, mgr: mgr, store: store, mux: http.NewServeMux()}
	a.mux.HandleFunc("/healthz", a.handleHealthz)
	a.mux.HandleFunc("/webhook/", a.handleWebhook)
	a.mux.HandleFunc("/admin/robots", a.handleAdminRobots)
	a.mux.HandleFunc("/admin/robots/", a.handleAdminRobot)
	return a
}

func (a *APIServer) Start() error {
	a.server = &http.Server{Addr: a.srv.ListenAddr, Handler: a.mux, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[API] server error: %v", err)
		}
	}()
	log.Printf("[API] listening on %s", a.srv.ListenAddr)
	return nil
}

func (a *APIServer) Stop() {
	if a.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = a.server.Shutdown(ctx)
	}
}

func (a *APIServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	type rstat struct {
		ID        string `json:"id"`
		LastSeen  string `json:"lastSeen"`
		DataFresh bool   `json:"dataFresh"`
	}
	var robots []rstat
	for _, s := range a.mgr.List() {
		snap := s.State().Snapshot()
		lastSeen := ""
		if !snap.LastUpdate.IsZero() {
			lastSeen = snap.LastUpdate.Format(time.RFC3339)
		}
		robots = append(robots, rstat{ID: s.ID(), LastSeen: lastSeen, DataFresh: !snap.LastUpdate.IsZero() && time.Since(snap.LastUpdate) < 15*time.Second})
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "robots": robots})
}

func (a *APIServer) handleWebhook(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/webhook/")
	// ATOM robots append a data-source suffix to the configured webhook URL
	// (e.g. .../webhook/<id>/imu, .../webhook/<id>/encoder for the odometry
	// streams). Only the first path segment identifies the robot.
	key, _, _ = strings.Cut(key, "/")
	if key == "" {
		http.Error(w, "missing robot key", 400)
		return
	}
	if r.Method == http.MethodGet {
		fmt.Fprint(w, "pi2w-bridge ok")
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	defer r.Body.Close()

	sess := a.mgr.Get(key)
	if sess == nil {
		// Only robots declared in robots.yaml / via /admin/robots are managed;
		// drop webhook data from anything else. (An unknown key often carries an
		// ATOM data-source suffix, e.g. "<id>imu", and shows up only until the
		// robot picks up the trailing-slash webhook URL.)
		log.Printf("[API] webhook for unmanaged robot %q — dropped", key)
		http.Error(w, "unknown robot", http.StatusNotFound)
		return
	}
	// X-Webhook-Secret: ATOM robots don't always send this header on their webhook
	// POSTs, so we fail open on a missing header (the {robotKey} path segment is the
	// only thing gating these POSTs then) but still reject a present-but-wrong secret.
	// TODO: tighten once we confirm the ATOM API can attach a custom header at webhook
	// registration time.
	if want := sess.WebhookSecret(); want != "" {
		if got := r.Header.Get("X-Webhook-Secret"); got != "" && got != want {
			http.Error(w, "bad webhook secret", http.StatusUnauthorized)
			return
		}
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(body) > 0 {
		sess.HandleWebhook(body)
	}
	w.WriteHeader(200)
	fmt.Fprint(w, "ok")
}

func (a *APIServer) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+a.srv.AdminToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (a *APIServer) handleAdminRobots(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		var out []RobotRecord
		if a.store != nil {
			out, _ = a.store.ListRobots()
		}
		live := a.mgr.IDs()
		for i := range out {
			if live[out[i].ID] {
				out[i].Status = "online"
			}
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var rec RobotRecord
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&rec); err != nil || rec.ID == "" {
			http.Error(w, "bad robot record", 400)
			return
		}
		if rec.Source == "" {
			rec.Source = "admin"
		}
		if err := a.mgr.Register(rec); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, 201, map[string]string{"id": rec.ID, "status": "registered"})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *APIServer) handleAdminRobot(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/admin/robots/")
	if id == "" {
		http.Error(w, "missing id", 400)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if a.store == nil {
			http.Error(w, "no store", http.StatusServiceUnavailable)
			return
		}
		rec, err := a.store.GetRobot(id)
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
		writeJSON(w, 200, rec)
	case http.MethodDelete:
		a.mgr.Deregister(id)
		writeJSON(w, 200, map[string]string{"id": id, "status": "deleted"})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// applyWebhookPayload parses a robot webhook payload (single object or array) and
// applies each item to state — ported from the old single-robot webhook.go.
func applyWebhookPayload(state *RobotState, cfg *Config, body []byte, lg *log.Logger) {
	if len(body) == 0 {
		return
	}
	var raw json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		lg.Printf("[Webhook] invalid JSON: %v", err)
		return
	}
	var items []map[string]interface{}
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &items); err != nil {
			lg.Printf("[Webhook] invalid JSON array: %v", err)
			return
		}
	} else {
		var single map[string]interface{}
		if err := json.Unmarshal(raw, &single); err != nil {
			lg.Printf("[Webhook] invalid JSON object: %v", err)
			return
		}
		items = []map[string]interface{}{single}
	}
	mapLoadedSeen := false
	for _, item := range items {
		ApplyWebhookData(state, item)
		if cfg != nil {
			if rs, ok := item["route_status"].(map[string]interface{}); ok {
				if s, _ := rs["status"].(string); s == "map_loaded" {
					mapLoadedSeen = true
				}
			}
		}
	}
	if mapLoadedSeen && cfg != nil {
		go func() {
			if name := queryATOMCurrentMap(cfg); name != "" {
				state.SetMapID(name)
			}
		}()
	}
}
