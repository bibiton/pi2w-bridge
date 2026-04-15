package main

import (
	"sync"
	"time"
)

// RobotState holds the in-memory real-time state of the robot.
// All fields are protected by a mutex for concurrent access.
type RobotState struct {
	mu sync.RWMutex

	// Position
	PoseX   float64
	PoseY   float64
	PoseYaw float64 // radians

	// Map
	MapID string

	// Motion
	Driving       bool
	VelocityVX    float64
	VelocityVY    float64
	VelocityOmega float64

	// Battery
	BatteryPercent  float64
	BatteryVoltage  float64
	BatteryCharging bool

	// Status
	OperatingMode string // "AUTOMATIC", "MANUAL", "SEMIAUTOMATIC", "SERVICE"
	Status        string // raw status from robot: "standby", "delivering", "arrived", etc.
	Target        string
	Event         string

	// Lidar (raw for potential use)
	LastScan     map[string]interface{}
	LastScanRear map[string]interface{}

	// Order state (Phase 1: mostly empty)
	OrderID           string
	OrderUpdateID     uint32
	LastNodeID        string
	LastNodeSequenceID uint32
	NewBaseRequest    bool
	Paused            bool

	// Action states
	ActionStates []ActionState
	NodeStates   []NodeState
	EdgeStates   []EdgeState
	Errors       []AGVError
	Loads        []interface{}

	// Navigation completion notification (set by webhook, read by order handler)
	NavArrivedCh chan struct{}

	// Map list
	MapList []string

	// Timestamps
	LastUpdate       time.Time
	PositionInit     bool
	ConnectionState  string // "ONLINE", "OFFLINE", "CONNECTIONBROKEN"
}

type ActionState struct {
	ActionID          string `json:"actionId"`
	ActionType        string `json:"actionType"`
	ActionDescription string `json:"actionDescription,omitempty"`
	ActionStatus      string `json:"actionStatus"` // WAITING, INITIALIZING, RUNNING, PAUSED, FINISHED, FAILED
	ResultDescription string `json:"resultDescription,omitempty"`
}

type NodeState struct {
	NodeID      string `json:"nodeId"`
	SequenceID  uint32 `json:"sequenceId"`
	Released    bool   `json:"released"`
	NodeDescription string `json:"nodeDescription,omitempty"`
}

type EdgeState struct {
	EdgeID      string `json:"edgeId"`
	SequenceID  uint32 `json:"sequenceId"`
	Released    bool   `json:"released"`
	EdgeDescription string `json:"edgeDescription,omitempty"`
}

type AGVError struct {
	ErrorType        string              `json:"errorType"` // WARNING, FATAL
	ErrorReferences  []ErrorReference    `json:"errorReferences,omitempty"`
	ErrorDescription string              `json:"errorDescription"`
	ErrorLevel       string              `json:"errorLevel"` // WARNING, FATAL
}

type ErrorReference struct {
	ReferenceKey   string `json:"referenceKey"`
	ReferenceValue string `json:"referenceValue"`
}

func NewRobotState() *RobotState {
	return &RobotState{
		OperatingMode:   "AUTOMATIC",
		ConnectionState: "OFFLINE",
		PositionInit:    true,
		ActionStates:    []ActionState{},
		NodeStates:      []NodeState{},
		EdgeStates:      []EdgeState{},
		Errors:          []AGVError{},
		Loads:           []interface{}{},
		MapList:         []string{},
		NavArrivedCh:    make(chan struct{}, 1),
	}
}

// Snapshot returns a copy of the state for reading.
type StateSnapshot struct {
	PoseX          float64
	PoseY          float64
	PoseYaw        float64
	MapID          string
	Driving        bool
	VelocityVX     float64
	VelocityVY     float64
	VelocityOmega  float64
	BatteryPercent float64
	BatteryVoltage float64
	BatteryCharging bool
	OperatingMode  string
	Status         string
	Target         string
	Event          string

	OrderID            string
	OrderUpdateID      uint32
	LastNodeID         string
	LastNodeSequenceID uint32
	NewBaseRequest     bool
	Paused             bool

	ActionStates []ActionState
	NodeStates   []NodeState
	EdgeStates   []EdgeState
	Errors       []AGVError
	Loads        []interface{}

	MapList        []string
	LastUpdate     time.Time
	PositionInit   bool
	ConnectionState string
}

func (rs *RobotState) Snapshot() StateSnapshot {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	// Copy slices to avoid race
	actionStates := make([]ActionState, len(rs.ActionStates))
	copy(actionStates, rs.ActionStates)

	nodeStates := make([]NodeState, len(rs.NodeStates))
	copy(nodeStates, rs.NodeStates)

	edgeStates := make([]EdgeState, len(rs.EdgeStates))
	copy(edgeStates, rs.EdgeStates)

	errors := make([]AGVError, len(rs.Errors))
	copy(errors, rs.Errors)

	loads := make([]interface{}, len(rs.Loads))
	copy(loads, rs.Loads)

	mapList := make([]string, len(rs.MapList))
	copy(mapList, rs.MapList)

	return StateSnapshot{
		PoseX:              rs.PoseX,
		PoseY:              rs.PoseY,
		PoseYaw:            rs.PoseYaw,
		MapID:              rs.MapID,
		Driving:            rs.Driving,
		VelocityVX:         rs.VelocityVX,
		VelocityVY:         rs.VelocityVY,
		VelocityOmega:      rs.VelocityOmega,
		BatteryPercent:     rs.BatteryPercent,
		BatteryVoltage:     rs.BatteryVoltage,
		BatteryCharging:    rs.BatteryCharging,
		OperatingMode:      rs.OperatingMode,
		Status:             rs.Status,
		Target:             rs.Target,
		Event:              rs.Event,
		OrderID:            rs.OrderID,
		OrderUpdateID:      rs.OrderUpdateID,
		LastNodeID:         rs.LastNodeID,
		LastNodeSequenceID: rs.LastNodeSequenceID,
		NewBaseRequest:     rs.NewBaseRequest,
		Paused:             rs.Paused,
		ActionStates:       actionStates,
		NodeStates:         nodeStates,
		EdgeStates:         edgeStates,
		Errors:             errors,
		Loads:              loads,
		MapList:            mapList,
		LastUpdate:         rs.LastUpdate,
		PositionInit:       rs.PositionInit,
		ConnectionState:    rs.ConnectionState,
	}
}

func (rs *RobotState) SetMapList(maps []string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.MapList = maps
}

func (rs *RobotState) SetMapID(id string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.MapID = id
}

func (rs *RobotState) SetConnectionState(state string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.ConnectionState = state
}

func (rs *RobotState) IsDataFresh(timeout time.Duration) bool {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	if rs.LastUpdate.IsZero() {
		return false
	}
	return time.Since(rs.LastUpdate) < timeout
}

func (rs *RobotState) AddActionState(as ActionState) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.ActionStates = append(rs.ActionStates, as)
}

func (rs *RobotState) UpdateActionState(actionID, status, result string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for i := range rs.ActionStates {
		if rs.ActionStates[i].ActionID == actionID {
			rs.ActionStates[i].ActionStatus = status
			if result != "" {
				rs.ActionStates[i].ResultDescription = result
			}
			return
		}
	}
}

func (rs *RobotState) SetPaused(paused bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.Paused = paused
}

func (rs *RobotState) RemoveFinishedActions() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	active := make([]ActionState, 0)
	for _, as := range rs.ActionStates {
		if as.ActionStatus != "FINISHED" && as.ActionStatus != "FAILED" {
			active = append(active, as)
		}
	}
	rs.ActionStates = active
}

// --- Order management ---

func (rs *RobotState) SetOrder(orderID string, updateID uint32) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.OrderID = orderID
	rs.OrderUpdateID = updateID
}

func (rs *RobotState) SetNodeStates(ns []NodeState) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.NodeStates = ns
}

func (rs *RobotState) SetEdgeStates(es []EdgeState) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.EdgeStates = es
}

func (rs *RobotState) SetLastNode(nodeID string, seqID uint32) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.LastNodeID = nodeID
	rs.LastNodeSequenceID = seqID
}

func (rs *RobotState) SetDriving(driving bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.Driving = driving
}

func (rs *RobotState) ClearOrder() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.OrderID = ""
	rs.OrderUpdateID = 0
	rs.LastNodeID = ""
	rs.LastNodeSequenceID = 0
	rs.NewBaseRequest = false
	rs.NodeStates = []NodeState{}
	rs.EdgeStates = []EdgeState{}
	rs.ActionStates = []ActionState{}
	rs.Driving = false
}

// NotifyNavArrived sends a non-blocking signal that navigation has completed.
func (rs *RobotState) NotifyNavArrived() {
	select {
	case rs.NavArrivedCh <- struct{}{}:
	default:
	}
}

// DrainNavArrived clears any pending arrival signal.
func (rs *RobotState) DrainNavArrived() {
	select {
	case <-rs.NavArrivedCh:
	default:
	}
}


