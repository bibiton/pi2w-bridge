package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	NodeID       string           `json:"nodeId"`
	SequenceID   uint32           `json:"sequenceId"`
	Released     bool             `json:"released"`
	NodePosition *VDA5050Position `json:"nodePosition,omitempty"`
	Actions      []VDA5050Action  `json:"actions"`
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
	ActionID         string               `json:"actionId"`
	ActionType       string               `json:"actionType"`
	BlockingType     string               `json:"blockingType,omitempty"`
	ActionParameters []VDA5050ActionParam `json:"actionParameters,omitempty"`
}

type VDA5050ActionParam struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// --- Order Handler ---

type OrderHandler struct {
	cfg     *Config
	state   *RobotState
	bridge  *MQTTBridge
	robotWS *RobotWSClient
	client  *http.Client
	tts     *TTSClient // nil when TTS_URL is empty — voice prompts are skipped

	mu           sync.Mutex
	currentOrder *VDA5050Order
	cancelCh     chan struct{} // close to cancel current order
}

func NewOrderHandler(cfg *Config, state *RobotState, bridge *MQTTBridge, robotWS *RobotWSClient) *OrderHandler {
	return &OrderHandler{
		cfg:     cfg,
		state:   state,
		bridge:  bridge,
		robotWS: robotWS,
		client:  &http.Client{Timeout: 30 * time.Second},
		tts:     NewTTSClient(cfg.TTSURL),
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

	// Pre-synthesize all playVoice utterances on the TTS service so that
	// reaching a node triggers instant playback (no synth wait at node arrival).
	// Runs in its own goroutine so we never block order acceptance even if
	// the TTS service is slow or unreachable.
	go func() {
		if oh.tts != nil {
			oh.tts.PrepareOrderVoices(&order)
		}
	}()

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

	// Track current mapId for cross-map detection
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

		// --- Cross-map guard ---
		nextMapID := ""
		if node.NodePosition != nil && node.NodePosition.MapID != "" {
			nextMapID = node.NodePosition.MapID
		}
		if nextMapID != "" && nextMapID != currentMapID {
			log.Printf("[Order] cross-map order not supported (%s -> %s); failing order", currentMapID, nextMapID)
			oh.failOrder(order.OrderID, "cross_map_not_supported")
			return
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

// cancelRobotNav sends a delivery cancel command to ATOM API per API v1.0.7.
// Uses the same string-payload pattern as goto_charging / leave_charger.
// Verified via direct curl: delivery_command:"cancel" aborts the current
// delivery/goto_charging/return task (e.g. robot backs off the charger).
func (oh *OrderHandler) cancelRobotNav() {
	payload, _ := json.Marshal(map[string]interface{}{
		"delivery_command":     "cancel",
		"ignore_state_control": "true",
	})
	url := oh.cfg.RobotBaseURL() + "/service/control/commands"
	resp, err := oh.client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("[Order] Cancel nav error: %v", err)
		return
	}
	resp.Body.Close()
	log.Printf("[Order] Cancel nav sent to ATOM API (delivery_command:cancel)")
}

// actionStartCharging sends the ATOM goto_charging command (string payload
// per API v1.0.7) which handles both navigation to the charge_station AND
// the physical backward docking in one command. Then polls for the
// show_charging webhook event to confirm the charging pins made contact.
//
// Payload must include ignore_state_control:"true" (matches reference
// robot_atom.py). Without it, ATOM accepts HTTP 200 but silently ignores
// the command.
func (oh *OrderHandler) actionStartCharging(action *VDA5050Action, cancelCh chan struct{}) error {
	commandURL := oh.cfg.RobotBaseURL() + "/service/control/commands"

	// Fast path: if already charging, the goal is already achieved.
	// Without this, goto_charging is a no-op (robot already on dock) and
	// ATOM never re-fires the "show_charging" webhook, so we'd wait the
	// full 5-minute timeout for nothing.
	if snap := oh.state.Snapshot(); snap.BatteryCharging {
		log.Printf("[Order] startCharging: robot already charging (battery=%.1f%%), skipping goto_charging", snap.BatteryPercent)
		return nil
	}

	// POST {"delivery_command":"goto_charging","ignore_state_control":"true"}
	// String payload (not object) — verified via direct curl that the object
	// form {"delivery_command":{"goto_charging":""}} is a no-op.
	log.Printf("[Order] startCharging: sending goto_charging (string payload)")
	payload, _ := json.Marshal(map[string]interface{}{
		"delivery_command":     "goto_charging",
		"ignore_state_control": "true",
	})
	resp, err := oh.client.Post(commandURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("startCharging: goto_charging request failed: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("startCharging: goto_charging HTTP %d: %s", resp.StatusCode, string(body))
	}
	log.Printf("[Order] startCharging: goto_charging sent (HTTP %d), waiting for charging confirmation", resp.StatusCode)

	// Poll BatteryCharging (set by webhook event "show_charging").
	// Timeout covers: navigation to charger + docking alignment attempts.
	chargeTimeout := time.After(5 * time.Minute)
	chargeTicker := time.NewTicker(2 * time.Second)
	defer chargeTicker.Stop()

	for {
		select {
		case <-cancelCh:
			return fmt.Errorf("startCharging: cancelled during docking")
		case <-chargeTimeout:
			return fmt.Errorf("startCharging: timed out waiting for show_charging (5 min)")
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
			"ignore_state_control": "true",
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
		// Play the audio that was pre-synthesized at order accept (HandleOrder).
		// On TTS failure we log + continue — voice is best-effort, never blocks
		// physical task progress.
		text := getActionParam(action, "text")
		if oh.tts == nil {
			log.Printf("[Order] playVoice: %q (TTS_URL unset, skipping)", text)
		} else if playErr := oh.playVoiceAction(action, text, cancelCh); playErr != nil {
			log.Printf("[Order] playVoice: %q failed: %v (continuing)", text, playErr)
		}
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

// --- Helpers ---

// ensureNotCharging checks if the robot is currently charging and, if so,
// sends leave_charger and waits for charging to stop before returning.
func (oh *OrderHandler) ensureNotCharging(cancelCh chan struct{}) error {
	snap := oh.state.Snapshot()
	if !snap.BatteryCharging {
		return nil
	}

	log.Printf("[Order] Robot is charging, sending leave_charger before task execution")

	commandURL := oh.cfg.RobotBaseURL() + "/service/control/commands"
	// leave_charger uses string payload format per ATOM API v1.0.7.
	payload, _ := json.Marshal(map[string]interface{}{
		"delivery_command":     "leave_charger",
		"ignore_state_control": "true",
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
				log.Printf("[Order] Robot left charger, waiting 5s to stabilize")
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

// playVoiceAction triggers playback of a pre-cached utterance.
// If /play returns 425 (still preparing), it waits briefly and retries —
// this handles the race where the robot reaches a node faster than the
// TTS service finishes background synthesis.
//
// If the cache entry is missing entirely (e.g. pi2w-bridge restarted
// mid-order, losing the pre-prepare burst), it falls back to synth-on-demand
// via /prepare + /play.
func (oh *OrderHandler) playVoiceAction(action *VDA5050Action, text string, cancelCh chan struct{}) error {
	id := action.ActionID

	const maxRetries = 30 // 30 × 200ms = 6s — long enough for fanchen-C synth
	for i := 0; i < maxRetries; i++ {
		select {
		case <-cancelCh:
			return fmt.Errorf("cancelled")
		default:
		}

		_, err := oh.tts.Play(id)
		if err == nil {
			log.Printf("[Order] playVoice: %q played (id=%s)", text, id)
			return nil
		}
		errStr := err.Error()
		// 425 = still preparing → wait + retry
		if strings.Contains(errStr, "HTTP 425") {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		// 404 = not cached → maybe pre-prepare didn't fire or we restarted.
		// Do synth-on-demand: prepare and then loop again.
		if strings.Contains(errStr, "HTTP 404") {
			log.Printf("[Order] playVoice: id=%s not cached, synth-on-demand", id)
			if perr := oh.tts.Prepare(id, text); perr != nil {
				return fmt.Errorf("on-demand prepare: %w", perr)
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		return err
	}
	return fmt.Errorf("/play retried %d times, still not ready", maxRetries)
}
