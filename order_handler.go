package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- VDA5050 Order types ---

type VDA5050Order struct {
	HeaderID      int64         `json:"headerId"`
	Timestamp     string        `json:"timestamp"`
	Version       string        `json:"version"`
	Manufacturer  string        `json:"manufacturer"`
	SerialNumber  string        `json:"serialNumber"`
	OrderID       string        `json:"orderId"`
	OrderUpdateID uint32        `json:"orderUpdateId"`
	TaskType      string        `json:"taskType,omitempty"`
	Nodes         []VDA5050Node `json:"nodes"`
	Edges         []VDA5050Edge `json:"edges"`
}

type VDA5050Node struct {
	NodeID       string            `json:"nodeId"`
	SequenceID   uint32            `json:"sequenceId"`
	Released     bool              `json:"released"`
	NodePosition *VDA5050Position  `json:"nodePosition,omitempty"`
	Actions      []VDA5050Action   `json:"actions"`
}

type VDA5050Edge struct {
	EdgeID      string          `json:"edgeId"`
	SequenceID  uint32          `json:"sequenceId"`
	Released    bool            `json:"released"`
	StartNodeID string          `json:"startNodeId"`
	EndNodeID   string          `json:"endNodeId"`
	Actions     []VDA5050Action `json:"actions"`
}

type VDA5050Position struct {
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Theta float64 `json:"theta,omitempty"`
	MapID string  `json:"mapId,omitempty"`
}

type VDA5050Action struct {
	ActionID         string              `json:"actionId"`
	ActionType       string              `json:"actionType"`
	BlockingType     string              `json:"blockingType,omitempty"`
	ActionParameters []VDA5050ActionParam `json:"actionParameters,omitempty"`
}

type VDA5050ActionParam struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// --- Order Handler ---

type OrderHandler struct {
	cfg          *Config
	state        *RobotState
	bridge       *MQTTBridge
	robotWS      *RobotWSClient
	elevatorCfg  *ElevatorConfig
	client       *http.Client

	mu          sync.Mutex
	currentOrder *VDA5050Order
	cancelCh    chan struct{} // close to cancel current order
}

func NewOrderHandler(cfg *Config, state *RobotState, bridge *MQTTBridge, robotWS *RobotWSClient, elevatorCfg *ElevatorConfig) *OrderHandler {
	return &OrderHandler{
		cfg:         cfg,
		state:       state,
		bridge:      bridge,
		robotWS:     robotWS,
		elevatorCfg: elevatorCfg,
		client:      &http.Client{Timeout: 30 * time.Second},
	}
}

// HandleOrder processes an incoming VDA5050 order.
func (oh *OrderHandler) HandleOrder(payload []byte) {
	var order VDA5050Order
	if err := json.Unmarshal(payload, &order); err != nil {
		log.Printf("[Order] Failed to parse order: %v", err)
		return
	}

	if order.OrderID == "" || len(order.Nodes) == 0 {
		log.Printf("[Order] Invalid order: no orderId or nodes")
		return
	}

	log.Printf("[Order] Received order: %s (updateId=%d, taskType=%s, nodes=%d, edges=%d)",
		order.OrderID, order.OrderUpdateID, order.TaskType, len(order.Nodes), len(order.Edges))

	oh.mu.Lock()
	// Cancel any existing order
	if oh.cancelCh != nil {
		close(oh.cancelCh)
	}
	oh.cancelCh = make(chan struct{})
	oh.currentOrder = &order
	cancelCh := oh.cancelCh
	oh.mu.Unlock()

	go oh.executeOrder(&order, cancelCh)
}

// CancelCurrentOrder cancels the currently executing order.
func (oh *OrderHandler) CancelCurrentOrder() {
	oh.mu.Lock()
	defer oh.mu.Unlock()
	if oh.cancelCh != nil {
		close(oh.cancelCh)
		oh.cancelCh = nil
	}
	oh.currentOrder = nil
}

func (oh *OrderHandler) executeOrder(order *VDA5050Order, cancelCh chan struct{}) {
	log.Printf("[Order] === Executing order %s ===", order.OrderID)

	// Set order in state
	oh.state.SetOrder(order.OrderID, order.OrderUpdateID)

	// Build initial nodeStates and edgeStates (all pending)
	oh.initOrderStates(order)

	// Register all actions as WAITING
	oh.initActionStates(order)

	// Trigger state publish
	oh.bridge.TriggerStatePublish()

	// Non-charging task: ensure robot has left charger before proceeding
	if order.TaskType != "charging" {
		if err := oh.ensureNotCharging(cancelCh); err != nil {
			log.Printf("[Order] Failed to leave charger: %v", err)
			oh.failOrder(order.OrderID, err.Error())
			return
		}
	}

	// Sort nodes by sequenceId, edges by sequenceId
	// Platform sends them in order, but let's be safe
	nodes := order.Nodes
	edges := order.Edges

	// Process: origin node (node[0]) is the starting point — mark as reached immediately
	if len(nodes) == 0 {
		log.Printf("[Order] No nodes to process")
		oh.finishOrder(order.OrderID)
		return
	}

	// Mark first node (origin) as reached
	originNode := nodes[0]
	log.Printf("[Order] Origin node: %s (seq=%d)", originNode.NodeID, originNode.SequenceID)
	oh.state.SetLastNode(originNode.NodeID, originNode.SequenceID)
	oh.removeNodeState(originNode.NodeID)

	// Execute origin node actions
	if err := oh.executeNodeActions(order.OrderID, &originNode, cancelCh); err != nil {
		log.Printf("[Order] Origin node actions failed: %v", err)
		oh.failOrder(order.OrderID, err.Error())
		return
	}

	oh.bridge.TriggerStatePublish()

	// Track current mapId for floor-change detection
	currentMapID := ""
	if originNode.NodePosition != nil && originNode.NodePosition.MapID != "" {
		currentMapID = originNode.NodePosition.MapID
	}
	if currentMapID == "" {
		snap := oh.state.Snapshot()
		currentMapID = snap.MapID
	}

	// Process remaining nodes (skip origin at index 0)
	for i := 1; i < len(nodes); i++ {
		select {
		case <-cancelCh:
			log.Printf("[Order] Order %s cancelled", order.OrderID)
			oh.cancelOrder(order.OrderID)
			return
		default:
		}

		node := nodes[i]

		// --- Floor change detection ---
		nextMapID := ""
		if node.NodePosition != nil && node.NodePosition.MapID != "" {
			nextMapID = node.NodePosition.MapID
		}
		if nextMapID != "" && oh.elevatorCfg != nil && oh.elevatorCfg.NeedsFloorChange(currentMapID, nextMapID) {
			log.Printf("[Order] === Floor change detected: %s -> %s ===", currentMapID, nextMapID)
			if err := oh.handleFloorChange(order.OrderID, currentMapID, nextMapID, cancelCh); err != nil {
				log.Printf("[Order] Floor change failed: %v", err)
				oh.failOrder(order.OrderID, err.Error())
				return
			}
			currentMapID = nextMapID
			log.Printf("[Order] === Floor change complete, now on map %s ===", currentMapID)
		} else if nextMapID != "" {
			currentMapID = nextMapID
		}

		// Find the edge leading to this node
		var edge *VDA5050Edge
		for j := range edges {
			if edges[j].EndNodeID == node.NodeID {
				edge = &edges[j]
				break
			}
		}

		// --- Traverse edge ---
		if edge != nil {
			log.Printf("[Order] Traversing edge %s (seq=%d) -> node %s", edge.EdgeID, edge.SequenceID, node.NodeID)
			oh.state.SetDriving(true)
			oh.bridge.TriggerStatePublish()

			// Execute edge actions (if any)
			if len(edge.Actions) > 0 {
				if err := oh.executeEdgeActions(order.OrderID, edge, cancelCh); err != nil {
					log.Printf("[Order] Edge %s actions failed: %v", edge.EdgeID, err)
					oh.failOrder(order.OrderID, err.Error())
					return
				}
			}

			// Navigate to this node
			if err := oh.navigateToNode(order.OrderID, &node, cancelCh); err != nil {
				log.Printf("[Order] Navigation to %s failed: %v", node.NodeID, err)
				oh.failOrder(order.OrderID, err.Error())
				return
			}

			oh.removeEdgeState(edge.EdgeID)
		}

		// --- Arrived at node ---
		log.Printf("[Order] Arrived at node %s (seq=%d)", node.NodeID, node.SequenceID)
		oh.state.SetLastNode(node.NodeID, node.SequenceID)
		oh.state.SetDriving(false)
		oh.removeNodeState(node.NodeID)
		oh.bridge.TriggerStatePublish()

		// Execute node actions
		if err := oh.executeNodeActions(order.OrderID, &node, cancelCh); err != nil {
			log.Printf("[Order] Node %s actions failed: %v", node.NodeID, err)
			oh.failOrder(order.OrderID, err.Error())
			return
		}

		oh.bridge.TriggerStatePublish()
	}

	// Charging task — no counter return needed
	if order.TaskType == "charging" {
		log.Printf("[Order] === Charging task %s completed ===", order.OrderID)
		oh.finishOrder(order.OrderID)
		return
	}

	// All waypoint nodes processed — wait 30s then return to counter
	log.Printf("[Order] All waypoints done, waiting 30s before returning to counter...")

	waitTimer := time.NewTimer(30 * time.Second)
	select {
	case <-cancelCh:
		waitTimer.Stop()
		log.Printf("[Order] Order %s cancelled during counter wait", order.OrderID)
		oh.cancelOrder(order.OrderID)
		return
	case <-waitTimer.C:
	}

	// Navigate back to counter
	log.Printf("[Order] Returning to counter station via ATOM API")
	if err := oh.navigateToStation(order.OrderID, "counter", cancelCh); err != nil {
		log.Printf("[Order] Return to counter failed: %v", err)
		oh.failOrder(order.OrderID, err.Error())
		return
	}

	log.Printf("[Order] === Order %s completed (returned to counter) ===", order.OrderID)
	oh.finishOrder(order.OrderID)
}

// navigateToNode handles the actual robot navigation to a node.
// It checks if the node has a GoToLocation action — if so, use station name.
// Otherwise, the edge traversal itself means the robot needs to move (XY — not implemented yet).
func (oh *OrderHandler) navigateToNode(orderID string, node *VDA5050Node, cancelCh chan struct{}) error {
	// Check if this node has a GoToLocation action
	var gotoAction *VDA5050Action
	for i := range node.Actions {
		if node.Actions[i].ActionType == "GoToLocation" {
			gotoAction = &node.Actions[i]
			break
		}
	}

	if gotoAction != nil {
		// Station-based navigation
		return oh.navigateByStation(orderID, gotoAction, cancelCh)
	}

	// No GoToLocation — this is an XY coordinate navigation
	// For now, just log and consider it arrived (XY nav not implemented)
	if node.NodePosition != nil && (node.NodePosition.X != 0 || node.NodePosition.Y != 0) {
		log.Printf("[Order] XY navigation to (%.3f, %.3f) not implemented, skipping",
			node.NodePosition.X, node.NodePosition.Y)
	}
	return nil
}

// navigateByStation sends a delivery command to ATOM API using station name.
func (oh *OrderHandler) navigateByStation(orderID string, action *VDA5050Action, cancelCh chan struct{}) error {
	stationID := ""
	stationName := ""
	for _, p := range action.ActionParameters {
		switch p.Key {
		case "stationId":
			stationID = p.Value
		case "stationName":
			stationName = p.Value
		}
	}

	target := stationName
	if target == "" {
		target = stationID
	}
	if target == "" {
		return fmt.Errorf("GoToLocation: missing stationId/stationName")
	}

	// Mark action as RUNNING
	oh.state.UpdateActionState(action.ActionID, "RUNNING", "")
	oh.bridge.TriggerStatePublish()

	log.Printf("[Order] GoToLocation: navigating to station '%s' via ATOM API", target)

	if err := oh.sendDeliveryWithRetry(target); err != nil {
		oh.state.UpdateActionState(action.ActionID, "FAILED", err.Error())
		return err
	}

	log.Printf("[Order] GoToLocation: delivery active, waiting for arrival...")

	// Wait for navigation completion (via webhook route_status → arrived/standby)
	if err := oh.waitForNavigation(cancelCh); err != nil {
		oh.state.UpdateActionState(action.ActionID, "FAILED", err.Error())
		return err
	}

	oh.state.UpdateActionState(action.ActionID, "FINISHED", "")
	log.Printf("[Order] GoToLocation: arrived at '%s'", target)

	// Send stop to prevent ATOM auto-return after delivery completion
	oh.cancelRobotNav()

	return nil
}

// navigateToStation sends a delivery command to a named station and waits for arrival.
// Used for implicit navigation (e.g. return to counter) that is not tied to a VDA5050 action.
func (oh *OrderHandler) navigateToStation(orderID string, stationName string, cancelCh chan struct{}) error {
	log.Printf("[Order] navigateToStation: sending delivery to '%s' via ATOM API", stationName)

	oh.state.SetDriving(true)
	oh.bridge.TriggerStatePublish()

	if err := oh.sendDeliveryWithRetry(stationName); err != nil {
		oh.state.SetDriving(false)
		return err
	}

	log.Printf("[Order] navigateToStation: delivery active, waiting for arrival at '%s'...", stationName)

	if err := oh.waitForNavigation(cancelCh); err != nil {
		oh.state.SetDriving(false)
		return err
	}

	oh.state.SetDriving(false)
	log.Printf("[Order] navigateToStation: arrived at '%s'", stationName)

	// Send stop command to prevent ATOM's automatic "return" behavior
	// After a delivery completes, ATOM may auto-navigate back to origin.
	oh.cancelRobotNav()
	log.Printf("[Order] navigateToStation: sent stop to prevent auto-return")

	return nil
}

// waitForNavigation blocks until the robot reports arrived/standby or cancel/timeout.
// It first waits for the robot to start moving (status != arrived/standby), then waits
// for it to arrive at the destination.
func (oh *OrderHandler) waitForNavigation(cancelCh chan struct{}) error {
	// Drain any previous arrival signals
	oh.state.DrainNavArrived()

	timeout := time.NewTimer(5 * time.Minute)
	defer timeout.Stop()

	pollTicker := time.NewTicker(1 * time.Second)
	defer pollTicker.Stop()
	statusURL := oh.cfg.RobotBaseURL() + "/service/system/routing/status/get"

	// Phase 1: Wait for robot to start moving (depart from current location).
	departed := false
	departureDeadline := time.NewTimer(30 * time.Second) // if no departure in 30s, assume already at target
	defer departureDeadline.Stop()
	snap := oh.state.Snapshot()
	log.Printf("[Order] waitForNavigation: initial webhook status=%s driving=%v", snap.Status, snap.Driving)
	// Consider departed if already in an active navigation status (but not map_loaded)
	if snap.Status == "delivering" || snap.Status == "rerouting" || snap.Status == "return" {
		departed = true
	}

	for {
		select {
		case <-cancelCh:
			oh.cancelRobotNav()
			return fmt.Errorf("order cancelled")
		case <-timeout.C:
			return fmt.Errorf("navigation timeout (5 min)")
		case <-oh.state.NavArrivedCh:
			if departed {
				log.Printf("[Order] waitForNavigation: arrived via NavArrivedCh")
				return nil
			}
			log.Printf("[Order] waitForNavigation: ignoring stale arrival signal (not yet departed)")
		case <-departureDeadline.C:
			if !departed {
				log.Printf("[Order] waitForNavigation: no departure detected in 30s, assuming already at target")
				return nil
			}
		case <-pollTicker.C:
			snap := oh.state.Snapshot()
			// Always poll ATOM routing status API for ground truth
			polledStatus := ""
			if resp, err := oh.client.Get(statusURL); err == nil {
				var sr map[string]interface{}
				if json.NewDecoder(resp.Body).Decode(&sr) == nil {
					if rs, ok := sr["route_status"].(map[string]interface{}); ok {
						polledStatus, _ = rs["status"].(string)
					}
				}
				resp.Body.Close()
			}

			if !departed {
				if snap.Status == "delivering" || snap.Status == "rerouting" || snap.Status == "return" ||
					(snap.Driving && polledStatus != "map_loaded" && polledStatus != "standby") ||
					polledStatus == "delivering" || polledStatus == "rerouting" {
					departed = true
					departureDeadline.Stop()
					log.Printf("[Order] waitForNavigation: robot departed (webhook=%s polled=%s)", snap.Status, polledStatus)
					oh.state.DrainNavArrived()
				}
			} else {
				// Phase 2: Wait for arrival
				if snap.Status == "arrived" || (snap.Status == "standby" && !snap.Driving) ||
					polledStatus == "arrived" || polledStatus == "standby" {
					log.Printf("[Order] waitForNavigation: arrived (webhook=%s polled=%s)", snap.Status, polledStatus)
					return nil
				}
				// Log every 5 seconds for debug
				if polledStatus != "" {
					log.Printf("[Order] waitForNavigation: waiting... (webhook=%s polled=%s driving=%v)", snap.Status, polledStatus, snap.Driving)
				}
			}
		}
	}
}

// cancelRobotNav sends a stop command to ATOM API.
func (oh *OrderHandler) cancelRobotNav() {
	payload, _ := json.Marshal(map[string]string{
		"routing_control": "stop",
	})
	url := oh.cfg.RobotBaseURL() + "/service/control/commands"
	resp, err := oh.client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("[Order] Cancel nav error: %v", err)
		return
	}
	resp.Body.Close()
	log.Printf("[Order] Cancel nav sent to ATOM API")
}

// actionStartCharging sends goto_charging to ATOM API, waits for navigation
// to begin, then waits for the webhook show_charging event to confirm docking.
func (oh *OrderHandler) actionStartCharging(action *VDA5050Action, cancelCh chan struct{}) error {
	commandURL := oh.cfg.RobotBaseURL() + "/service/control/commands"
	statusURL := oh.cfg.RobotBaseURL() + "/service/system/routing/status/get"

	// Step 1: POST deliver_to_location: charge_station
	// ATOM's {"goto_charging":""} returns HTTP 200 but doesn't actually navigate.
	// The working command is deliver_to_location with the station named "charge_station"
	// (type=charging in the map's point table).
	log.Printf("[Order] startCharging: sending deliver_to_location charge_station")
	payload, _ := json.Marshal(map[string]interface{}{
		"delivery_command": map[string]interface{}{
			"deliver_to_location": []string{"charge_station"},
		},
	})
	resp, err := oh.client.Post(commandURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("startCharging: deliver_to_location charge_station request failed: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("startCharging: deliver_to_location charge_station HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Step 2: poll route_status — confirm robot starts navigating
	log.Printf("[Order] startCharging: waiting for navigation to start...")
	navTimeout := time.After(30 * time.Second)
	navTicker := time.NewTicker(3 * time.Second)
	defer navTicker.Stop()

	navStarted := false
	for !navStarted {
		select {
		case <-cancelCh:
			return fmt.Errorf("startCharging: cancelled during navigation wait")
		case <-navTimeout:
			log.Printf("[Order] startCharging: navigation start timeout, proceeding anyway")
			navStarted = true
		case <-navTicker.C:
			if sr, err := oh.client.Get(statusURL); err == nil {
				var result map[string]interface{}
				if json.NewDecoder(sr.Body).Decode(&result) == nil {
					if rs, ok := result["route_status"].(map[string]interface{}); ok {
						status, _ := rs["status"].(string)
						status = strings.ToLower(status)
						log.Printf("[Order] startCharging: route_status=%s", status)
						if status == "goto charging" || status == "delivering" || status == "goto_charging" {
							navStarted = true
						}
					}
				}
				sr.Body.Close()
			}
		}
	}
	log.Printf("[Order] startCharging: robot navigating to charger")

	// Step 3: poll BatteryCharging (set by webhook show_charging)
	log.Printf("[Order] startCharging: waiting for charging confirmation...")
	chargeTimeout := time.After(5 * time.Minute)
	chargeTicker := time.NewTicker(2 * time.Second)
	defer chargeTicker.Stop()

	for {
		select {
		case <-cancelCh:
			return fmt.Errorf("startCharging: cancelled during charging wait")
		case <-chargeTimeout:
			return fmt.Errorf("startCharging: timed out waiting for charging (5 min)")
		case <-chargeTicker.C:
			snap := oh.state.Snapshot()
			if snap.BatteryCharging {
				log.Printf("[Order] startCharging: robot is charging (battery=%.1f%%)", snap.BatteryPercent)
				return nil
			}
		}
	}
}

// sendDeliveryWithRetry sends a deliver_to_location command and verifies it activates.
// If delivery doesn't start (status stays map_loaded), it re-selects delivery mode and retries.
func (oh *OrderHandler) sendDeliveryWithRetry(stationName string) error {
	commandURL := oh.cfg.RobotBaseURL() + "/service/control/commands"
	statusURL := oh.cfg.RobotBaseURL() + "/service/system/routing/status/get"

	for attempt := 1; attempt <= 3; attempt++ {
		payload, _ := json.Marshal(map[string]interface{}{
			"delivery_command": map[string]interface{}{
				"deliver_to_location": []string{stationName},
			},
		})

		resp, err := oh.client.Post(commandURL, "application/json", bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("ATOM API delivery to %s: %w", stationName, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 400 {
			return fmt.Errorf("ATOM API HTTP %d: %s", resp.StatusCode, string(body))
		}

		log.Printf("[Order] sendDelivery: sent deliver_to_location '%s' → HTTP %d (attempt %d)", stationName, resp.StatusCode, attempt)

		// Wait 3s then check if delivery actually started
		time.Sleep(3 * time.Second)
		polledStatus := ""
		if sr, err := oh.client.Get(statusURL); err == nil {
			var result map[string]interface{}
			if json.NewDecoder(sr.Body).Decode(&result) == nil {
				if rs, ok := result["route_status"].(map[string]interface{}); ok {
					polledStatus, _ = rs["status"].(string)
				}
			}
			sr.Body.Close()
		}

		if polledStatus == "delivering" || polledStatus == "rerouting" || polledStatus == "backward" {
			log.Printf("[Order] sendDelivery: active (status=%s)", polledStatus)
			return nil
		}

		if attempt < 3 {
			log.Printf("[Order] sendDelivery: NOT active (status=%s), re-selecting delivery mode (attempt %d/3)...", polledStatus, attempt)
			deliveryPayload, _ := json.Marshal(map[string]string{"select_mode": "delivery"})
			if r, err := oh.client.Post(commandURL, "application/json", bytes.NewReader(deliveryPayload)); err == nil {
				r.Body.Close()
			}
			time.Sleep(5 * time.Second)
		} else {
			log.Printf("[Order] sendDelivery: still NOT active after 3 attempts (status=%s), proceeding anyway", polledStatus)
		}
	}
	return nil
}

// executeNodeActions runs all actions on a node sequentially.
func (oh *OrderHandler) executeNodeActions(orderID string, node *VDA5050Node, cancelCh chan struct{}) error {
	for i := range node.Actions {
		action := &node.Actions[i]

		select {
		case <-cancelCh:
			return fmt.Errorf("order cancelled")
		default:
		}

		// GoToLocation is handled during navigation, skip here
		if action.ActionType == "GoToLocation" {
			continue
		}

		if err := oh.executeAction(orderID, action, cancelCh); err != nil {
			return err
		}
	}
	return nil
}

// executeEdgeActions runs all actions on an edge.
func (oh *OrderHandler) executeEdgeActions(orderID string, edge *VDA5050Edge, cancelCh chan struct{}) error {
	for i := range edge.Actions {
		action := &edge.Actions[i]

		select {
		case <-cancelCh:
			return fmt.Errorf("order cancelled")
		default:
		}

		if err := oh.executeAction(orderID, action, cancelCh); err != nil {
			return err
		}
	}
	return nil
}

// executeAction dispatches a single action.
func (oh *OrderHandler) executeAction(orderID string, action *VDA5050Action, cancelCh chan struct{}) error {
	log.Printf("[Order] Executing action: %s (type=%s, id=%s)", action.ActionID, action.ActionType, action.ActionID)

	oh.state.UpdateActionState(action.ActionID, "RUNNING", "")
	oh.bridge.TriggerStatePublish()

	var err error
	switch action.ActionType {
	case "wait":
		err = oh.actionWait(action, cancelCh)
	case "playVoice":
		// Robot has no TTS hardware — mark as finished immediately
		text := getActionParam(action, "text")
		log.Printf("[Order] playVoice: '%s' (no TTS hardware, skipping)", text)
		err = nil
	case "drop":
		// Robot has no drop mechanism — mark as finished immediately
		log.Printf("[Order] drop: (no mechanism, skipping)")
		err = nil
	case "startCharging":
		err = oh.actionStartCharging(action, cancelCh)
	default:
		log.Printf("[Order] Unknown action type: %s, skipping", action.ActionType)
		err = nil
	}

	if err != nil {
		oh.state.UpdateActionState(action.ActionID, "FAILED", err.Error())
		return err
	}

	oh.state.UpdateActionState(action.ActionID, "FINISHED", "")
	oh.bridge.TriggerStatePublish()
	return nil
}

// actionWait sleeps for the specified duration.
func (oh *OrderHandler) actionWait(action *VDA5050Action, cancelCh chan struct{}) error {
	durationStr := getActionParam(action, "duration")
	duration, _ := strconv.ParseFloat(durationStr, 64)
	if duration <= 0 {
		duration = 1 // minimum 1 second
	}

	log.Printf("[Order] wait: %.1f seconds", duration)

	timer := time.NewTimer(time.Duration(duration * float64(time.Second)))
	defer timer.Stop()

	select {
	case <-cancelCh:
		return fmt.Errorf("order cancelled during wait")
	case <-timer.C:
		return nil
	}
}

// --- State management helpers ---

func (oh *OrderHandler) initOrderStates(order *VDA5050Order) {
	nodeStates := make([]NodeState, len(order.Nodes))
	for i, n := range order.Nodes {
		nodeStates[i] = NodeState{
			NodeID:     n.NodeID,
			SequenceID: n.SequenceID,
			Released:   n.Released,
		}
	}
	oh.state.SetNodeStates(nodeStates)

	edgeStates := make([]EdgeState, len(order.Edges))
	for i, e := range order.Edges {
		edgeStates[i] = EdgeState{
			EdgeID:     e.EdgeID,
			SequenceID: e.SequenceID,
			Released:   e.Released,
		}
	}
	oh.state.SetEdgeStates(edgeStates)
}

func (oh *OrderHandler) initActionStates(order *VDA5050Order) {
	oh.state.mu.Lock()
	defer oh.state.mu.Unlock()

	// Clear any existing action states and add all order actions as WAITING
	oh.state.ActionStates = []ActionState{}

	for _, node := range order.Nodes {
		for _, action := range node.Actions {
			oh.state.ActionStates = append(oh.state.ActionStates, ActionState{
				ActionID:     action.ActionID,
				ActionType:   action.ActionType,
				ActionStatus: "WAITING",
			})
		}
	}
	for _, edge := range order.Edges {
		for _, action := range edge.Actions {
			oh.state.ActionStates = append(oh.state.ActionStates, ActionState{
				ActionID:     action.ActionID,
				ActionType:   action.ActionType,
				ActionStatus: "WAITING",
			})
		}
	}
}

func (oh *OrderHandler) removeNodeState(nodeID string) {
	oh.state.mu.Lock()
	defer oh.state.mu.Unlock()
	remaining := make([]NodeState, 0, len(oh.state.NodeStates))
	for _, ns := range oh.state.NodeStates {
		if ns.NodeID != nodeID {
			remaining = append(remaining, ns)
		}
	}
	oh.state.NodeStates = remaining
}

func (oh *OrderHandler) removeEdgeState(edgeID string) {
	oh.state.mu.Lock()
	defer oh.state.mu.Unlock()
	remaining := make([]EdgeState, 0, len(oh.state.EdgeStates))
	for _, es := range oh.state.EdgeStates {
		if es.EdgeID != edgeID {
			remaining = append(remaining, es)
		}
	}
	oh.state.EdgeStates = remaining
}

func (oh *OrderHandler) finishOrder(orderID string) {
	log.Printf("[Order] Order %s finished, clearing state", orderID)
	// Keep orderId in state so platform can see it with all actions FINISHED,
	// then clear after a delay
	oh.bridge.TriggerStatePublish()

	time.Sleep(3 * time.Second)
	oh.state.ClearOrder()
	oh.bridge.TriggerStatePublish()

	oh.mu.Lock()
	oh.currentOrder = nil
	oh.mu.Unlock()
}

func (oh *OrderHandler) failOrder(orderID string, reason string) {
	log.Printf("[Order] Order %s FAILED: %s", orderID, reason)
	oh.bridge.TriggerStatePublish()

	time.Sleep(3 * time.Second)
	oh.state.ClearOrder()
	oh.bridge.TriggerStatePublish()

	oh.mu.Lock()
	oh.currentOrder = nil
	oh.mu.Unlock()
}

func (oh *OrderHandler) cancelOrder(orderID string) {
	log.Printf("[Order] Order %s cancelled, clearing state", orderID)
	oh.cancelRobotNav()
	oh.state.ClearOrder()
	oh.bridge.TriggerStatePublish()

	oh.mu.Lock()
	oh.currentOrder = nil
	oh.mu.Unlock()
}

// --- Elevator / Floor Change ---

// handleFloorChange executes the full elevator flow to move the robot between floors.
//
// Flow (aligned with NexOS IoT Gateway v2 state machine):
//  1.   Navigate to current floor's elevator hall station
//  2.   switchMap to elevator tunnel map
//  3.   SetInitialPose at tunnel's elevator hall point
//  3.5  Re-select delivery mode
//  4+5. tw_elevator_call → blocks until elevator arrives at current floor (FINISHED)
//  5.5  Connect elevator WiFi
//  6a.  Navigate into elevator car
//  6b.  tw_elevator_enter → blocks until elevator arrives at target floor ("please exit" RUNNING)
//  9.   Navigate out of elevator to tunnel hall + tw_elevator_exit (FINISHED)
//  9.5  Disconnect elevator WiFi
//  10.  switchMap to target floor map
//  11.  SetInitialPose at target floor's elevator hall point
//  11.5 Re-select delivery mode
func (oh *OrderHandler) handleFloorChange(orderID, fromMapID, toMapID string, cancelCh chan struct{}) error {
	fromFloor := oh.elevatorCfg.GetFloor(fromMapID)
	toFloor := oh.elevatorCfg.GetFloor(toMapID)
	if fromFloor == nil || toFloor == nil {
		return fmt.Errorf("floor config not found: from=%s to=%s", fromMapID, toMapID)
	}

	ev := oh.elevatorCfg.Elevator
	log.Printf("[Elevator] Floor change: %dF (%s) -> %dF (%s)", fromFloor.Floor, fromMapID, toFloor.Floor, toMapID)

	// Step 1: Navigate to current floor's elevator hall
	log.Printf("[Elevator] Step 1: Navigating to elevator hall '%s' on floor %d", fromFloor.ElevatorHall, fromFloor.Floor)
	if err := oh.navigateToStation(orderID, fromFloor.ElevatorHall, cancelCh); err != nil {
		return fmt.Errorf("navigate to elevator hall: %w", err)
	}
	log.Printf("[Elevator] Step 1: Arrived at elevator hall")

	// Step 2: Switch to elevator tunnel map
	log.Printf("[Elevator] Step 2: Switching to tunnel map '%s'", ev.TunnelMap)
	if err := oh.doSwitchMap(ev.TunnelMap); err != nil {
		return fmt.Errorf("switch to tunnel map: %w", err)
	}
	log.Printf("[Elevator] Step 2: Map switched to tunnel")

	// Step 3: SetInitialPose at tunnel hall point
	log.Printf("[Elevator] Step 3: SetInitialPose at tunnel hall '%s'", ev.Hall)
	if err := oh.setPoseAtStation(ev.Hall); err != nil {
		log.Printf("[Elevator] Step 3: setPose failed (non-fatal): %v", err)
	} else {
		log.Printf("[Elevator] Step 3: setPose at tunnel hall done")
	}

	// Step 3.5: Re-select delivery mode after setPose (Nav2 needs time to reinitialize)
	log.Printf("[Elevator] Step 3.5: Re-selecting delivery mode after setPose...")
	commandURL := oh.cfg.RobotBaseURL() + "/service/control/commands"
	deliveryPayload, _ := json.Marshal(map[string]string{"select_mode": "delivery"})
	if resp, err := oh.client.Post(commandURL, "application/json", bytes.NewReader(deliveryPayload)); err != nil {
		log.Printf("[Elevator] Step 3.5: select_mode delivery failed: %v", err)
	} else {
		resp.Body.Close()
	}
	time.Sleep(5 * time.Second)
	log.Printf("[Elevator] Step 3.5: Delivery mode re-selected, ready for navigation")

	// Get elevator ID — only use A梯 (retry discovery up to 3 times)
	elevatorID := ""
	if oh.bridge.elevatorService != nil {
		for attempt := 1; attempt <= 3; attempt++ {
			elevatorID = oh.bridge.elevatorService.GetElevatorByName("A梯")
			if elevatorID != "" {
				break
			}
			log.Printf("[Elevator] A梯 not found (attempt %d/3), running discovery...", attempt)
			if err := oh.bridge.elevatorService.RunDiscovery(); err != nil {
				log.Printf("[Elevator] Discovery failed: %v", err)
				time.Sleep(3 * time.Second)
			}
		}
	}
	if elevatorID == "" {
		return fmt.Errorf("A梯 not available")
	}
	log.Printf("[Elevator] Using A梯 (id=%s)", elevatorID)

	// Step 4+5: Call elevator to current floor via tw_elevator_call (blocks until ready)
	log.Printf("[Elevator] Step 4: Calling elevator to floor %d (tw_elevator_call)", fromFloor.Floor)
	if err := oh.bridge.elevatorService.CallElevator(elevatorID, fromFloor.Floor, toFloor.Floor); err != nil {
		return fmt.Errorf("call elevator: %w", err)
	}
	log.Printf("[Elevator] Step 5: Elevator arrived at floor %d", fromFloor.Floor)

	// Step 5.5: Connect elevator WiFi before entering
	if len(ev.WifiNetworks) > 0 {
		log.Printf("[Elevator] Step 5.5: Connecting elevator WiFi...")
		if err := ConnectElevatorWifi(ev.WifiNetworks); err != nil {
			log.Printf("[Elevator] Step 5.5: WiFi connect failed (non-fatal): %v", err)
		} else {
			log.Printf("[Elevator] Step 5.5: Elevator WiFi connected")
		}
	}

	// Step 6: Enter elevator — navigate to car station first, then notify IoT Gateway
	carStation, ok := ev.Cars["A"]
	if !ok {
		return fmt.Errorf("car A not found in elevator config")
	}
	log.Printf("[Elevator] Step 6a: Navigating to car A station '%s'", carStation)
	if err := oh.navigateToStation(orderID, carStation, cancelCh); err != nil {
		return fmt.Errorf("enter elevator: %w", err)
	}
	log.Printf("[Elevator] Step 6a: Inside elevator car A")

	// Step 6b+7+8: Notify IoT Gateway robot entered → elevator departs → wait for "please exit" signal
	log.Printf("[Elevator] Step 6b: Notifying IoT Gateway (tw_elevator_enter), waiting for arrival at floor %d...", toFloor.Floor)
	if err := oh.bridge.elevatorService.EnterElevator(elevatorID); err != nil {
		return fmt.Errorf("elevator travel: %w", err)
	}
	log.Printf("[Elevator] Step 8: Elevator arrived at floor %d", toFloor.Floor)

	// Step 9: Exit elevator — navigate to tunnel hall + notify IoT Gateway
	log.Printf("[Elevator] Step 9: Exiting elevator -> hall '%s'", ev.Hall)
	if err := oh.navigateToStation(orderID, ev.Hall, cancelCh); err != nil {
		return fmt.Errorf("exit elevator: %w", err)
	}
	log.Printf("[Elevator] Step 9: Exited elevator to hall")
	if err := oh.bridge.elevatorService.ExitElevator(elevatorID); err != nil {
		log.Printf("[Elevator] Step 9: exit notify failed (non-fatal): %v", err)
	}

	// Step 9.5: Disconnect elevator WiFi after exiting
	if len(ev.WifiNetworks) > 0 {
		log.Printf("[Elevator] Step 9.5: Disconnecting elevator WiFi...")
		if err := DisconnectElevatorWifi(ev.WifiNetworks); err != nil {
			log.Printf("[Elevator] Step 9.5: WiFi disconnect failed (non-fatal): %v", err)
		} else {
			log.Printf("[Elevator] Step 9.5: Elevator WiFi disconnected")
		}
	}

	// Step 10: Switch to target floor map
	log.Printf("[Elevator] Step 10: Switching to target floor map '%s'", toMapID)
	if err := oh.doSwitchMap(toMapID); err != nil {
		return fmt.Errorf("switch to target floor map: %w", err)
	}

	// Step 11: SetInitialPose at target floor's elevator hall
	log.Printf("[Elevator] Step 11: SetInitialPose at floor %d, elevator hall '%s'",
		toFloor.Floor, toFloor.ElevatorHall)
	if err := oh.setPoseAtStation(toFloor.ElevatorHall); err != nil {
		log.Printf("[Elevator] Step 11: setPose failed (non-fatal): %v", err)
	} else {
		log.Printf("[Elevator] Step 11: setPose at target floor hall done")
	}

	// Step 11.5: Re-select delivery mode after setPose
	log.Printf("[Elevator] Step 11.5: Re-selecting delivery mode after setPose...")
	commandURL2 := oh.cfg.RobotBaseURL() + "/service/control/commands"
	deliveryPayload2, _ := json.Marshal(map[string]string{"select_mode": "delivery"})
	if resp, err := oh.client.Post(commandURL2, "application/json", bytes.NewReader(deliveryPayload2)); err != nil {
		log.Printf("[Elevator] Step 11.5: select_mode delivery failed: %v", err)
	} else {
		resp.Body.Close()
	}
	time.Sleep(5 * time.Second)
	log.Printf("[Elevator] Step 11.5: Delivery mode re-selected")

	log.Printf("[Elevator] Floor change complete: now on floor %d (%s)", toFloor.Floor, toMapID)
	return nil
}

// doSwitchMap executes the ATOM API map switch sequence:
// 1. set map parameter
// 2. wait for map_loaded (polling routing status, up to 90s)
// 3. select_mode delivery + wait 5s
// 4. state.SetMapID(mapID)
//
// No robot_core restart needed — the ATOM API handles map switching internally.
func (oh *OrderHandler) doSwitchMap(mapID string) error {
	switchStart := time.Now()
	commandURL := oh.cfg.RobotBaseURL() + "/service/control/commands"

	log.Printf("[SwitchMap] Start: target map=%s", mapID)

	// Step 1: Set map parameter
	setURL := fmt.Sprintf("%s/service/parameter/set/map/%s", oh.cfg.RobotBaseURL(), mapID)
	resp, err := oh.client.Post(setURL, "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		return fmt.Errorf("set map: %w", err)
	}
	resp.Body.Close()
	log.Printf("[SwitchMap] Map parameter set to %s (elapsed: %v)", mapID, time.Since(switchStart))

	// Step 2: Wait for map_loaded by polling routing status (up to 90s)
	statusURL := oh.cfg.RobotBaseURL() + "/service/system/routing/status/get"
	deadline := time.After(90 * time.Second)
	for {
		select {
		case <-deadline:
			log.Printf("[SwitchMap] WARNING: map_loaded timeout (90s), proceeding anyway")
			goto mapReady
		case <-time.After(3 * time.Second):
			if resp, err := oh.client.Get(statusURL); err == nil {
				var sr map[string]interface{}
				if json.NewDecoder(resp.Body).Decode(&sr) == nil {
					status := ""
					if rs, ok := sr["route_status"].(map[string]interface{}); ok {
						status, _ = rs["status"].(string)
					}
					if status == "map_loaded" {
						log.Printf("[SwitchMap] map_loaded detected (elapsed: %v)", time.Since(switchStart))
						goto mapReady
					}
					log.Printf("[SwitchMap] polling route_status=%s (elapsed: %v)", status, time.Since(switchStart))
				}
				resp.Body.Close()
			}
		}
	}
mapReady:

	// Step 3: select_mode delivery + wait for ready
	deliveryPayload, _ := json.Marshal(map[string]string{"select_mode": "delivery"})
	resp, err = oh.client.Post(commandURL, "application/json", bytes.NewReader(deliveryPayload))
	if err != nil {
		return fmt.Errorf("select_mode delivery: %w", err)
	}
	resp.Body.Close()
	log.Printf("[SwitchMap] select_mode delivery sent (elapsed: %v)", time.Since(switchStart))

	time.Sleep(5 * time.Second)
	log.Printf("[SwitchMap] Delivery mode ready (elapsed: %v)", time.Since(switchStart))

	// Step 4: Update state
	oh.state.SetMapID(mapID)
	log.Printf("[SwitchMap] Completed: map=%s (total: %v)", mapID, time.Since(switchStart))
	return nil
}


// setPoseAtStation queries POI data from FastAPI to find a station's coordinates,
// then calls SetInitialPose via WebSocket to relocalize the robot.
func (oh *OrderHandler) setPoseAtStation(stationName string) error {
	url := oh.cfg.RobotFastAPI + "/poi_data"
	resp, err := oh.client.Get(url)
	if err != nil {
		return fmt.Errorf("get poi_data: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var raw struct {
		Point map[string]struct {
			Name  string  `json:"name"`
			X     float64 `json:"x"`
			Y     float64 `json:"y"`
			Angle float64 `json:"angle"`
		} `json:"point"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("parse poi_data: %w", err)
	}

	for _, p := range raw.Point {
		if p.Name == stationName {
			// Convert degrees to radians, normalize to [-pi, pi]
			yawDeg := p.Angle
			if yawDeg > 180 {
				yawDeg -= 360
			}
			yawRad := yawDeg * math.Pi / 180.0
			log.Printf("[Elevator] setPoseAtStation: %s -> x=%.3f y=%.3f yaw=%.1f° (%.4f rad)",
				stationName, p.X, p.Y, p.Angle, yawRad)
			return oh.robotWS.SetInitialPose(p.X, p.Y, yawRad)
		}
	}
	return fmt.Errorf("station '%s' not found in POI data", stationName)
}

// ensureNotCharging checks if the robot is currently charging and, if so,
// sends leave_charger and waits for charging to stop before returning.
func (oh *OrderHandler) ensureNotCharging(cancelCh chan struct{}) error {
	snap := oh.state.Snapshot()
	if !snap.BatteryCharging {
		return nil
	}

	log.Printf("[Order] Robot is charging, sending leave_charger before task execution")

	commandURL := oh.cfg.RobotBaseURL() + "/service/control/commands"
	payload, _ := json.Marshal(map[string]interface{}{
		"delivery_command": map[string]interface{}{
			"leave_charger": "",
		},
	})
	resp, err := oh.client.Post(commandURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("leave_charger request failed: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("leave_charger HTTP %d: %s", resp.StatusCode, string(body))
	}
	log.Printf("[Order] leave_charger sent (HTTP %d)", resp.StatusCode)

	// Poll BatteryCharging == false (set by webhook event "remove_charging")
	timeout := time.NewTimer(60 * time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-cancelCh:
			return fmt.Errorf("order cancelled while leaving charger")
		case <-timeout.C:
			return fmt.Errorf("timeout waiting for robot to leave charger (60s)")
		case <-ticker.C:
			snap := oh.state.Snapshot()
			if !snap.BatteryCharging {
				log.Printf("[Order] Robot left charger, re-selecting delivery mode")
				// Re-select delivery mode to ensure navigation stack is ready
				deliveryPayload, _ := json.Marshal(map[string]string{"select_mode": "delivery"})
				if r, err := oh.client.Post(commandURL, "application/json", bytes.NewReader(deliveryPayload)); err != nil {
					log.Printf("[Order] ensureNotCharging: select_mode delivery failed: %v", err)
				} else {
					r.Body.Close()
				}
				log.Printf("[Order] ensureNotCharging: waiting 5s to stabilize")
				select {
				case <-cancelCh:
					return fmt.Errorf("order cancelled while stabilizing after leaving charger")
				case <-time.After(5 * time.Second):
					return nil
				}
			}
		}
	}
}

// --- Helpers ---

func getActionParam(action *VDA5050Action, key string) string {
	for _, p := range action.ActionParameters {
		if p.Key == key {
			return p.Value
		}
	}
	return ""
}
