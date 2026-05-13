package main

import (
	"errors"
	"log"
	"sync"
	"time"
)

var errEmptyRobotID = errors.New("robot id is empty")

type SessionManager struct {
	srv   *ServerConfig
	store *Store

	mu       sync.RWMutex
	sessions map[string]*RobotSession

	reaperStop chan struct{}
}

func NewSessionManager(srv *ServerConfig, store *Store) *SessionManager {
	m := &SessionManager{srv: srv, store: store, sessions: map[string]*RobotSession{}, reaperStop: make(chan struct{})}
	go m.reaper()
	return m
}

// Register starts (or restarts) a session for rec.
func (m *SessionManager) Register(rec RobotRecord) error {
	if rec.ID == "" {
		return errEmptyRobotID
	}
	m.mu.Lock()
	old := m.sessions[rec.ID]
	sess := NewRobotSession(rec, m.srv, m.store)
	m.sessions[rec.ID] = sess
	m.mu.Unlock()

	// Stop the replaced session before starting the new one so we never have
	// two live MQTT clients fighting the same VDA5050 connection topic.
	if old != nil {
		old.Stop()
	}
	sess.Start()
	if m.store != nil {
		_ = m.store.UpsertRobot(rec)
	}
	log.Printf("[Manager] registered robot %s (source=%s)", rec.ID, rec.Source)
	return nil
}

func (m *SessionManager) Deregister(id string) {
	m.mu.Lock()
	sess := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if sess != nil {
		sess.Stop()
	}
	if m.store != nil {
		_ = m.store.SetRobotStatus(id, "deleted")
	}
	log.Printf("[Manager] deregistered robot %s", id)
}

func (m *SessionManager) Get(id string) *RobotSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

func (m *SessionManager) List() []*RobotSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*RobotSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

func (m *SessionManager) IDs() map[string]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]bool{}
	for id := range m.sessions {
		out[id] = true
	}
	return out
}

func (m *SessionManager) StopAll() {
	close(m.reaperStop)
	for _, s := range m.List() {
		s.Stop()
	}
}

// LoadFromStore registers every non-deleted robot found in the DB. Call at startup.
func (m *SessionManager) LoadFromStore() {
	if m.store == nil {
		return
	}
	recs, err := m.store.ListActiveRobots()
	if err != nil {
		log.Printf("[Manager] LoadFromStore: %v", err)
		return
	}
	for _, r := range recs {
		if err := m.Register(r); err != nil {
			log.Printf("[Manager] register %s from store: %v", r.ID, err)
		}
	}
}

func (m *SessionManager) reaper() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-m.reaperStop:
			return
		case <-t.C:
			if m.store == nil {
				continue
			}
			recs, err := m.store.ListActiveRobots()
			if err != nil {
				continue
			}
			for _, r := range recs {
				if r.Status == "errored" {
					log.Printf("[Manager] reaper: re-registering errored robot %s", r.ID)
					_ = m.Register(withStatus(r, "online"))
				}
			}
		}
	}
}
