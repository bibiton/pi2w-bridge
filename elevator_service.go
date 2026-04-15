package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// ElevatorService handles elevator discovery, real-time status monitoring,
// and elevator call/enter/exit operations via VDA5050 T-Extension actions.
// It communicates with the IoT Gateway through MQTT instantActions.
type ElevatorService struct {
	bridge *MQTTBridge
	cfg    *Config

	mu       sync.RWMutex
	siteID   string
	siteName string
	lobbies  []ElevatorLobby
	statuses map[string]*ElevatorStatus

	// pending requests: actionId -> response channel
	pendingMu sync.Mutex
	pending   map[string]chan ActionStateMsg

	stopCh chan struct{}
}

// --- Data types ---

type ElevatorLobby struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Elevators []ElevatorInfo `json:"elevators"`
}

type ElevatorInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // online / offline
}

type ElevatorStatus struct {
	ElevatorID   string    `json:"elevatorId"`
	CurrentFloor int       `json:"currentFloor"`
	Direction    string    `json:"direction"` // idle / up / down
	DoorState    string    `json:"doorState"` // open / closed
	Reserved     bool      `json:"reserved"`
	UpdatedAt    time.Time `json:"-"`
}

// ActionStateMsg is the response structure from IoT Gateway inside actionStates.
type ActionStateMsg struct {
	ActionID          string `json:"actionId"`
	ActionType        string `json:"actionType"`
	ActionStatus      string `json:"actionStatus"`
	ResultDescription string `json:"resultDescription"`
}

type DiscoveryResult struct {
	SiteID   string          `json:"siteId"`
	SiteName string          `json:"siteName"`
	Lobbies  []ElevatorLobby `json:"lobbies"`
}

// --- Constructor ---

func NewElevatorService(bridge *MQTTBridge, cfg *Config) *ElevatorService {
	return &ElevatorService{
		bridge:   bridge,
		cfg:      cfg,
		statuses: make(map[string]*ElevatorStatus),
		pending:  make(map[string]chan ActionStateMsg),
		stopCh:   make(chan struct{}),
	}
}

// --- Lifecycle ---

// Start begins elevator discovery and periodic status monitoring.
func (es *ElevatorService) Start() {
	go es.run()
}

func (es *ElevatorService) run() {
	// Wait for MQTT to connect
	time.Sleep(5 * time.Second)

	// Initial discovery
	if err := es.RunDiscovery(); err != nil {
		log.Printf("[ElevatorSvc] Initial discovery failed: %v (will retry in 60s)", err)
		// Retry once after 60s
		time.Sleep(60 * time.Second)
		if err := es.RunDiscovery(); err != nil {
			log.Printf("[ElevatorSvc] Discovery retry failed: %v", err)
		}
	}

	// Status monitoring loop (every 30s)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-es.stopCh:
			return
		case <-ticker.C:
			es.pollAllStatus()
		}
	}
}

func (es *ElevatorService) Stop() {
	close(es.stopCh)
}

// --- Discovery ---

// RunDiscovery sends tw_elevator_discovery and waits for response.
func (es *ElevatorService) RunDiscovery() error {
	actionID := fmt.Sprintf("ev_disc_%d", time.Now().UnixMilli())

	respCh := es.registerPending(actionID)
	defer es.removePending(actionID)

	es.publishTwAction(actionID, "tw_elevator_discovery", nil)
	log.Printf("[ElevatorSvc] Sent tw_elevator_discovery (actionId=%s)", actionID)

	resp, err := es.waitResponse(respCh, 15*time.Second)
	if err != nil {
		return fmt.Errorf("discovery: %w", err)
	}
	if resp.ActionStatus == "FAILED" {
		return fmt.Errorf("discovery FAILED: %s", resp.ResultDescription)
	}

	var result DiscoveryResult
	if err := json.Unmarshal([]byte(resp.ResultDescription), &result); err != nil {
		return fmt.Errorf("parse discovery result: %w", err)
	}

	es.mu.Lock()
	es.siteID = result.SiteID
	es.siteName = result.SiteName
	es.lobbies = result.Lobbies
	es.mu.Unlock()

	elevatorCount := 0
	for _, lobby := range result.Lobbies {
		elevatorCount += len(lobby.Elevators)
		for _, elev := range lobby.Elevators {
			log.Printf("[ElevatorSvc]   %s / %s (id=%s, status=%s)",
				lobby.Name, elev.Name, elev.ID, elev.Status)
		}
	}
	log.Printf("[ElevatorSvc] Discovery OK: site=%s, %d lobby(s), %d elevator(s)",
		result.SiteName, len(result.Lobbies), elevatorCount)

	return nil
}

// --- Status ---

// PollStatus sends tw_elevator_status for a single elevator and returns the result.
func (es *ElevatorService) PollStatus(elevatorID string) (*ElevatorStatus, error) {
	actionID := fmt.Sprintf("ev_st_%d", time.Now().UnixMilli())

	respCh := es.registerPending(actionID)
	defer es.removePending(actionID)

	es.publishTwAction(actionID, "tw_elevator_status", []map[string]interface{}{
		{"key": "elevatorId", "value": elevatorID},
	})

	resp, err := es.waitResponse(respCh, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("status %s: %w", elevatorID, err)
	}
	if resp.ActionStatus == "FAILED" {
		return nil, fmt.Errorf("status FAILED: %s", resp.ResultDescription)
	}

	var status ElevatorStatus
	if err := json.Unmarshal([]byte(resp.ResultDescription), &status); err != nil {
		return nil, fmt.Errorf("parse status: %w", err)
	}
	status.UpdatedAt = time.Now()

	es.mu.Lock()
	es.statuses[elevatorID] = &status
	es.mu.Unlock()

	return &status, nil
}

func (es *ElevatorService) pollAllStatus() {
	es.mu.RLock()
	var ids []string
	for _, lobby := range es.lobbies {
		for _, elev := range lobby.Elevators {
			ids = append(ids, elev.ID)
		}
	}
	es.mu.RUnlock()

	if len(ids) == 0 {
		return
	}

	for _, eid := range ids {
		status, err := es.PollStatus(eid)
		if err != nil {
			log.Printf("[ElevatorSvc] Status %s error: %v", eid[:8], err)
			continue
		}
		name := es.getElevatorName(eid)
		log.Printf("[ElevatorSvc] %s: floor=%d dir=%s door=%s reserved=%v",
			name, status.CurrentFloor, status.Direction, status.DoorState, status.Reserved)
	}
}

// --- Call / Enter / Exit ---

// CallElevator sends tw_elevator_call to call an elevator to a specific floor.
// Returns the actionId for tracking, and waits for FINISHED/FAILED.
func (es *ElevatorService) CallElevator(elevatorID string, fromFloor, toFloor int) error {
	actionID := fmt.Sprintf("ev_call_%d", time.Now().UnixMilli())

	respCh := es.registerPending(actionID)
	defer es.removePending(actionID)

	es.publishTwAction(actionID, "tw_elevator_call", []map[string]interface{}{
		{"key": "elevatorId", "value": elevatorID},
		{"key": "currentFloor", "value": fromFloor},
		{"key": "targetFloor", "value": toFloor},
	})
	log.Printf("[ElevatorSvc] Sent tw_elevator_call: elevator=%s from=%d to=%d (actionId=%s)",
		elevatorID[:8], fromFloor, toFloor, actionID)

	resp, err := es.waitResponse(respCh, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("call elevator: %w", err)
	}
	if resp.ActionStatus == "FAILED" {
		return fmt.Errorf("call elevator FAILED: %s", resp.ResultDescription)
	}

	log.Printf("[ElevatorSvc] tw_elevator_call FINISHED: %s", resp.ResultDescription)
	return nil
}

// EnterElevator sends tw_elevator_enter to notify IoT Gateway that the robot has entered.
// IoT Gateway responds with:
//   - RUNNING "entered, moving to floor X" — elevator departing
//   - RUNNING "arrived at floor X, please exit" — elevator arrived at target floor
//
// This method blocks until the "please exit" RUNNING signal is received,
// which means the elevator has arrived at the target floor.
func (es *ElevatorService) EnterElevator(elevatorID string) error {
	actionID := fmt.Sprintf("ev_enter_%d", time.Now().UnixMilli())

	respCh := es.registerPending(actionID)
	defer es.removePending(actionID)

	es.publishTwAction(actionID, "tw_elevator_enter", []map[string]interface{}{
		{"key": "elevatorId", "value": elevatorID},
	})
	log.Printf("[ElevatorSvc] Sent tw_elevator_enter: elevator=%s (actionId=%s)",
		elevatorID[:8], actionID)

	// Wait for "please exit" signal (RUNNING) or terminal state (FINISHED/FAILED)
	// The "please exit" arrives when elevator reaches the target floor
	resp, err := es.waitResponse(respCh, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("enter elevator: %w", err)
	}
	if resp.ActionStatus == "FAILED" {
		return fmt.Errorf("enter elevator FAILED: %s", resp.ResultDescription)
	}

	log.Printf("[ElevatorSvc] tw_elevator_enter response: status=%s desc=%s",
		resp.ActionStatus, resp.ResultDescription)
	return nil
}

// ExitElevator sends tw_elevator_exit to notify IoT Gateway that the robot has exited.
// The IoT Gateway will release the elevator for normal use.
func (es *ElevatorService) ExitElevator(elevatorID string) error {
	actionID := fmt.Sprintf("ev_exit_%d", time.Now().UnixMilli())

	respCh := es.registerPending(actionID)
	defer es.removePending(actionID)

	es.publishTwAction(actionID, "tw_elevator_exit", []map[string]interface{}{
		{"key": "elevatorId", "value": elevatorID},
	})
	log.Printf("[ElevatorSvc] Sent tw_elevator_exit: elevator=%s (actionId=%s)",
		elevatorID[:8], actionID)

	resp, err := es.waitResponse(respCh, 2*time.Minute)
	if err != nil {
		return fmt.Errorf("exit elevator: %w", err)
	}
	if resp.ActionStatus == "FAILED" {
		return fmt.Errorf("exit elevator FAILED: %s", resp.ResultDescription)
	}

	log.Printf("[ElevatorSvc] tw_elevator_exit FINISHED: %s", resp.ResultDescription)
	return nil
}

// --- Query methods ---

// GetLobbies returns the discovered elevator lobbies.
func (es *ElevatorService) GetLobbies() []ElevatorLobby {
	es.mu.RLock()
	defer es.mu.RUnlock()
	return es.lobbies
}

// GetStatus returns the latest cached status for an elevator.
func (es *ElevatorService) GetStatus(elevatorID string) *ElevatorStatus {
	es.mu.RLock()
	defer es.mu.RUnlock()
	return es.statuses[elevatorID]
}

// GetAllStatuses returns all cached elevator statuses.
func (es *ElevatorService) GetAllStatuses() map[string]*ElevatorStatus {
	es.mu.RLock()
	defer es.mu.RUnlock()
	result := make(map[string]*ElevatorStatus, len(es.statuses))
	for k, v := range es.statuses {
		cp := *v
		result[k] = &cp
	}
	return result
}

// GetSiteInfo returns the discovered site ID and name.
func (es *ElevatorService) GetSiteInfo() (string, string) {
	es.mu.RLock()
	defer es.mu.RUnlock()
	return es.siteID, es.siteName
}

// GetFirstElevatorID returns the ID of the first discovered elevator, or "" if none found.
func (es *ElevatorService) GetFirstElevatorID() string {
	es.mu.RLock()
	defer es.mu.RUnlock()
	for _, lobby := range es.lobbies {
		for _, elev := range lobby.Elevators {
			return elev.ID
		}
	}
	return ""
}

// GetElevatorByName returns the ID of the elevator matching the given name substring, or "" if not found.
func (es *ElevatorService) GetElevatorByName(name string) string {
	es.mu.RLock()
	defer es.mu.RUnlock()
	log.Printf("[ElevatorSvc] GetElevatorByName(%s): %d lobbies", name, len(es.lobbies))
	for _, lobby := range es.lobbies {
		for _, elev := range lobby.Elevators {
			log.Printf("[ElevatorSvc]   checking: %s (id=%s)", elev.Name, elev.ID[:8])
			if strings.Contains(elev.Name, name) {
				return elev.ID
			}
		}
	}
	return ""
}

// --- IoT Gateway response routing ---

// HandleActionStates processes incoming actionStates from IoT Gateway.
// Called by MQTTBridge when it receives a message with actionStates on instantActions topic.
func (es *ElevatorService) HandleActionStates(states []ActionStateMsg) {
	es.pendingMu.Lock()
	defer es.pendingMu.Unlock()

	for _, s := range states {
		log.Printf("[ElevatorSvc] ActionState: id=%s type=%s status=%s desc=%s",
			s.ActionID, s.ActionType, s.ActionStatus, s.ResultDescription)

		ch, ok := es.pending[s.ActionID]
		if !ok {
			continue
		}
		// Forward terminal states (FINISHED/FAILED) and significant RUNNING states
		// IoT Gateway sends RUNNING with "please exit" when elevator arrives at target floor
		if s.ActionStatus == "FINISHED" || s.ActionStatus == "FAILED" ||
			(s.ActionStatus == "RUNNING" && strings.Contains(s.ResultDescription, "please exit")) {
			select {
			case ch <- s:
			default:
			}
		}
	}
}

// --- Internal helpers ---

func (es *ElevatorService) registerPending(actionID string) chan ActionStateMsg {
	ch := make(chan ActionStateMsg, 1)
	es.pendingMu.Lock()
	es.pending[actionID] = ch
	es.pendingMu.Unlock()
	return ch
}

func (es *ElevatorService) removePending(actionID string) {
	es.pendingMu.Lock()
	delete(es.pending, actionID)
	es.pendingMu.Unlock()
}

func (es *ElevatorService) waitResponse(ch chan ActionStateMsg, timeout time.Duration) (ActionStateMsg, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case resp := <-ch:
		return resp, nil
	case <-timer.C:
		return ActionStateMsg{}, fmt.Errorf("timeout (%v)", timeout)
	case <-es.stopCh:
		return ActionStateMsg{}, fmt.Errorf("service stopped")
	}
}

// publishTwAction publishes a VDA5050 instantAction to the robot's instantActions topic.
// IoT Gateway subscribes to +/+/instantActions and will process tw_* actions.
// NOTE: We publish both "instantActions" (VDA5050 standard) and "actions" (legacy IoT Gateway)
// for backwards compatibility until IoT Gateway is fully updated.
func (es *ElevatorService) publishTwAction(actionID, actionType string, params []map[string]interface{}) {
	if params == nil {
		params = []map[string]interface{}{}
	}

	actionObj := map[string]interface{}{
		"actionId":         actionID,
		"actionType":       actionType,
		"blockingType":     "NONE",
		"actionParameters": params,
	}

	msg := map[string]interface{}{
		"headerId":       time.Now().UnixMilli(),
		"timestamp":      time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"version":        "2.0.0",
		"manufacturer":   es.cfg.Manufacturer,
		"serialNumber":   es.cfg.SerialNumber,
		"instantActions": []map[string]interface{}{actionObj},
		"actions":        []map[string]interface{}{actionObj}, // legacy compat
	}

	data, _ := json.Marshal(msg)
	topic := es.cfg.TopicPrefix() + "/instantActions"
	es.bridge.publish(topic, data, 1, false)
}

func (es *ElevatorService) getElevatorName(elevatorID string) string {
	es.mu.RLock()
	defer es.mu.RUnlock()
	for _, lobby := range es.lobbies {
		for _, elev := range lobby.Elevators {
			if elev.ID == elevatorID {
				return elev.Name
			}
		}
	}
	if len(elevatorID) > 8 {
		return elevatorID[:8]
	}
	return elevatorID
}

// isTwAction checks if an action type is a T-Extension action (meant for IoT Gateway).
func isTwAction(actionType string) bool {
	return strings.HasPrefix(actionType, "tw_")
}
