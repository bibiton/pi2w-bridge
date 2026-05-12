package main

import (
	"log"
	"sync"
	"time"
)

// RobotSession owns everything for one robot: config, state, MQTT bridge, WS client,
// map service, and the goroutines that were previously in main().
type RobotSession struct {
	rec   RobotRecord
	cfg   *Config
	srv   *ServerConfig
	store *Store
	log   *log.Logger

	state      *RobotState
	mapService *MapService
	robotWS    *RobotWSClient
	mqttBridge *MQTTBridge

	mu      sync.Mutex
	started bool
	stopCh  chan struct{}
}

// NewRobotSession constructs a fully-wired session for rec. store may be nil (unit tests, etc.).
func NewRobotSession(rec RobotRecord, srv *ServerConfig, store *Store) *RobotSession {
	cfg := LoadConfigForRobot(rec, srv)
	lg := log.New(log.Writer(), "[robot="+rec.ID+"] ", log.Ldate|log.Ltime|log.Lmicroseconds)
	s := &RobotSession{rec: rec, cfg: cfg, srv: srv, store: store, log: lg, stopCh: make(chan struct{})}
	s.state = NewRobotState()
	s.mapService = NewMapService(cfg)
	s.robotWS = NewRobotWSClient(cfg, s.state)
	s.mqttBridge = NewMQTTBridge(cfg, s.state, s.mapService, s.robotWS)
	s.mqttBridge.logger = lg
	s.robotWS.logger = lg
	return s
}

func (s *RobotSession) ID() string            { return s.rec.ID }
func (s *RobotSession) State() *RobotState    { return s.state }
func (s *RobotSession) WebhookSecret() string { return s.cfg.WebhookSecret }

// HandleWebhook applies a robot webhook payload (single object or array) to state.
func (s *RobotSession) HandleWebhook(body []byte) {
	applyWebhookPayload(s.state, s.cfg, body, s.log)
	if s.store != nil {
		status := "online"
		if s.rec.Source == "provisional" {
			status = "provisional"
		}
		_ = s.store.TouchRobot(s.rec.ID, status, time.Now())
	}
}

func (s *RobotSession) Start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()

	s.log.Printf("[Session] starting (atom=%s fastapi=%s mqtt=%s topic=%s)",
		s.cfg.RobotBaseURL(), s.cfg.RobotFastAPI, s.cfg.MQTTBroker, s.cfg.TopicPrefix())

	s.robotWS.Start()
	go s.safe("mqttConnect", func() {
		if err := s.mqttBridge.Connect(); err != nil {
			s.log.Printf("[Session] MQTT initial connect: %v (will retry)", err)
		}
		s.mqttBridge.StartPublishLoops()
	})
	go s.safe("FetchInitialMapID", func() { FetchInitialMapID(s.mapService, s.state, s.cfg) })
	StartMapListLoop(s.mapService, s.state)

	webhookURL := s.srv.PublicBaseURL + "/webhook/" + s.rec.ID
	go s.safe("RegisterWebhook", func() { RegisterWebhook(s.cfg, webhookURL) })

	go s.safe("statusLogger", func() { s.statusLoop() })

	if s.store != nil && s.rec.Source != "provisional" {
		_ = s.store.UpsertRobot(withStatus(s.rec, "online"))
	}
}

func (s *RobotSession) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	s.started = false
	close(s.stopCh)
	s.mu.Unlock()

	s.log.Printf("[Session] stopping")
	s.mqttBridge.Stop()
	s.robotWS.Stop()
	if s.store != nil {
		_ = s.store.SetRobotStatus(s.rec.ID, "offline")
	}
}

func (s *RobotSession) statusLoop() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			snap := s.state.Snapshot()
			if snap.LastUpdate.IsZero() {
				s.log.Printf("[Status] no data from robot yet")
			} else {
				s.log.Printf("[Status] %s (last update %v ago)", FormatStateLog(snap), time.Since(snap.LastUpdate).Round(time.Second))
			}
		}
	}
}

// safe runs fn, recovering from panics so one robot can't crash the process.
func (s *RobotSession) safe(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Printf("[Session] PANIC in %s: %v", name, r)
			if s.store != nil {
				_ = s.store.SetRobotStatus(s.rec.ID, "errored")
			}
		}
	}()
	fn()
}

func withStatus(r RobotRecord, st string) RobotRecord { r.Status = st; return r }
