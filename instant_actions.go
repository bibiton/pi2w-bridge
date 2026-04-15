package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"
)

type InstantActionHandler struct {
	cfg        *Config
	state      *RobotState
	mapService *MapService
	bridge     *MQTTBridge
	robotWS    *RobotWSClient
	client     *http.Client
}

func NewInstantActionHandler(cfg *Config, state *RobotState, ms *MapService, bridge *MQTTBridge, robotWS *RobotWSClient) *InstantActionHandler {
	return &InstantActionHandler{
		cfg:        cfg,
		state:      state,
		mapService: ms,
		bridge:     bridge,
		robotWS:    robotWS,
		client:     &http.Client{Timeout: 30 * time.Second},
	}
}

func (h *InstantActionHandler) Handle(actionID, actionType string, params map[string]string) {
	log.Printf("[Action] Processing: %s (id=%s)", actionType, actionID)

	// Add action state as RUNNING
	h.state.AddActionState(ActionState{
		ActionID:     actionID,
		ActionType:   actionType,
		ActionStatus: "RUNNING",
	})

	go func() {
		var err error
		switch actionType {
		case "uploadMap":
			err = h.handleUploadMap(actionID, params)
		case "getWaypoints":
			err = h.handleGetWaypoints(actionID, params)
		case "stateRequest":
			h.bridge.TriggerStatePublish()
			err = nil
		case "initPosition":
			err = h.handleInitPosition(actionID, params)
		case "navigate":
			err = h.handleNavigate(actionID, params)
		case "cancelOrder":
			// Cancel the order handler's current order first
			if h.bridge.orderHandler != nil {
				h.bridge.orderHandler.CancelCurrentOrder()
			}
			err = h.handleCancelOrder(actionID, params)
		case "stopPause":
			h.state.SetPaused(false)
		case "startPause":
			h.state.SetPaused(true)
		case "switchMap":
			err = h.handleSwitchMap(actionID, params)
		default:
			log.Printf("[Action] Unknown action type: %s", actionType)
			h.state.UpdateActionState(actionID, "FAILED", "Unknown action type")
			return
		}

		if err != nil {
			log.Printf("[Action] %s failed: %v", actionType, err)
			h.state.UpdateActionState(actionID, "FAILED", err.Error())
		} else {
			// uploadMap sets its own resultDescription with metadata;
			// only set generic FINISHED if not already set by the handler.
			if actionType != "uploadMap" {
				log.Printf("[Action] %s completed", actionType)
				h.state.UpdateActionState(actionID, "FINISHED", "")
			} else {
				log.Printf("[Action] %s completed (resultDescription set by handler)", actionType)
			}
		}

		// Trigger state publish to report action result
		h.bridge.TriggerStatePublish()

		// Clean up finished actions after a delay
		time.Sleep(5 * time.Second)
		h.state.RemoveFinishedActions()
	}()
}

func (h *InstantActionHandler) handleUploadMap(actionID string, params map[string]string) error {
	presignedURL := params["url"]
	if presignedURL == "" {
		presignedURL = params["presignedUrl"]
	}
	if presignedURL == "" {
		presignedURL = params["uploadUrl"]
	}
	if presignedURL == "" {
		return fmt.Errorf("missing presigned URL parameter")
	}

	mapName := params["mapName"]
	if mapName == "" {
		mapName = params["mapId"]
	}
	if mapName == "" {
		snap := h.state.Snapshot()
		mapName = snap.MapID
	}
	if mapName == "" {
		return fmt.Errorf("no map name available")
	}

	log.Printf("[Action] uploadMap: fetching map %s", mapName)

	// Determine if this is the current map
	snap := h.state.Snapshot()
	isCurrentMap := (mapName == snap.MapID)

	var pngData []byte
	var meta *MapMeta
	var err error

	if isCurrentMap {
		// Current map: use FastAPI for live data
		log.Printf("[Action] uploadMap: using FastAPI (current map)")
		rawImage, _, imgErr := h.mapService.GetMapImage(mapName)
		if imgErr != nil {
			return fmt.Errorf("get map image: %w", imgErr)
		}
		pngData = rawImage
		meta, err = h.mapService.GetMapMeta(mapName)
		if err != nil {
			log.Printf("[Action] uploadMap: meta fetch failed: %v", err)
		}
	} else {
		// Non-current map: download ZIP from ATOM API, extract in memory
		log.Printf("[Action] uploadMap: downloading ZIP from ATOM API (non-current map)")
		zipData, zipErr := h.mapService.DownloadMapZIP(mapName)
		if zipErr != nil {
			// Map not found on robot
			resultJSON, _ := json.Marshal(map[string]interface{}{
				"status": "not_found",
				"mapId":  mapName,
				"error":  zipErr.Error(),
			})
			h.state.UpdateActionState(actionID, "FAILED", string(resultJSON))
			h.bridge.TriggerStatePublish()
			return zipErr
		}
		pngData, err = ExtractPNGFromZIP(zipData)
		if err != nil {
			return fmt.Errorf("extract PNG from ZIP: %w", err)
		}
		meta, err = ExtractMetaFromZIP(zipData)
		if err != nil {
			log.Printf("[Action] uploadMap: meta extract failed: %v", err)
		}
		// zipData is GC'd after this scope — no disk storage used
	}

	// Beautify the map
	var res float64
	if meta != nil {
		res = meta.Resolution
	}
	beautified, bErr := BeautifyMap(pngData, res)
	if bErr != nil {
		log.Printf("[Action] Beautify failed, using raw image: %v", bErr)
		beautified = pngData
	}

	// Upload to presigned URL
	log.Printf("[Action] uploadMap: uploading %d bytes to presigned URL", len(beautified))
	req, err := http.NewRequest(http.MethodPut, presignedURL, bytes.NewReader(beautified))
	if err != nil {
		return fmt.Errorf("create upload request: %w", err)
	}
	req.Header.Set("Content-Type", "image/png")

	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Build resultDescription JSON with map metadata for platform
	result := map[string]interface{}{
		"status": "ok",
		"mapId":  mapName,
		"size":   len(beautified),
	}
	if meta != nil {
		result["resolution"] = meta.Resolution
		result["origin"] = meta.Origin
		result["width"] = meta.Width
		result["height"] = meta.Height
	}
	resultJSON, _ := json.Marshal(result)
	h.state.UpdateActionState(actionID, "FINISHED", string(resultJSON))

	log.Printf("[Action] uploadMap: success (HTTP %d), meta=%+v", resp.StatusCode, meta)
	return nil
}

// handleInitPosition sets robot pose via FastAPI WebSocket set_initial_pose.
func (h *InstantActionHandler) handleInitPosition(actionID string, params map[string]string) error {
	x, err := strconv.ParseFloat(params["x"], 64)
	if err != nil {
		return fmt.Errorf("invalid x: %v", err)
	}
	y, err := strconv.ParseFloat(params["y"], 64)
	if err != nil {
		return fmt.Errorf("invalid y: %v", err)
	}
	yaw, _ := strconv.ParseFloat(params["theta"], 64)
	if yaw == 0 {
		yaw, _ = strconv.ParseFloat(params["yaw"], 64)
	}

	mapID := params["mapId"]
	if mapID != "" {
		h.state.SetMapID(mapID)
		log.Printf("[Action] initPosition: mapId set to %s", mapID)
	}

	log.Printf("[Action] initPosition: setting pose x=%.3f y=%.3f yaw=%.3f via WebSocket (3 attempts)", x, y, yaw)

	// Send multiple times — AMCL particle filter sometimes needs repeated messages to converge
	for attempt := 1; attempt <= 3; attempt++ {
		if err := h.robotWS.SetInitialPose(x, y, yaw); err != nil {
			if attempt == 3 {
				return fmt.Errorf("SetInitialPose attempt %d: %w", attempt, err)
			}
			log.Printf("[Action] initPosition: attempt %d failed: %v, retrying...", attempt, err)
		}
		if attempt < 3 {
			time.Sleep(1 * time.Second)
		}
	}

	// Update local state immediately
	h.state.mu.Lock()
	h.state.PoseX = x
	h.state.PoseY = y
	h.state.PoseYaw = yaw
	h.state.PositionInit = true
	h.state.LastUpdate = time.Now()
	h.state.mu.Unlock()

	log.Printf("[Action] initPosition: pose set successfully")
	return nil
}

// handleNavigate sends a delivery command to ATOM API to navigate to a target.
func (h *InstantActionHandler) handleNavigate(actionID string, params map[string]string) error {
	target := params["target"]
	if target == "" {
		target = params["location"]
	}
	if target == "" {
		return fmt.Errorf("missing target/location parameter")
	}

	log.Printf("[Action] navigate: sending delivery command to %s via ATOM API", target)

	payload, _ := json.Marshal(map[string]interface{}{
		"delivery_command": map[string]interface{}{
			"deliver_to_location": []string{target},
		},
	})

	url := h.cfg.RobotBaseURL() + "/service/control/commands"
	resp, err := h.client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("ATOM API delivery: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("ATOM API HTTP %d: %s", resp.StatusCode, string(body))
	}

	log.Printf("[Action] navigate: ATOM API responded HTTP %d: %s", resp.StatusCode, string(body))
	return nil
}

// handleCancelOrder cancels current navigation via ATOM API.
func (h *InstantActionHandler) handleCancelOrder(actionID string, params map[string]string) error {
	log.Printf("[Action] cancelOrder: sending stop command to ATOM API")

	payload, _ := json.Marshal(map[string]string{
		"routing_control": "stop",
	})

	url := h.cfg.RobotBaseURL() + "/service/control/commands"
	resp, err := h.client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("ATOM API cancel: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	log.Printf("[Action] cancelOrder: ATOM API responded HTTP %d: %s", resp.StatusCode, string(body))
	return nil
}

// handleSwitchMap switches the robot's map via ATOM API.
func (h *InstantActionHandler) handleSwitchMap(actionID string, params map[string]string) error {
	mapID := params["mapId"]
	if mapID == "" {
		mapID = params["mapName"]
	}
	if mapID == "" {
		return fmt.Errorf("missing mapId parameter")
	}

	log.Printf("[Action] switchMap: switching to map %s", mapID)

	// Step 1: Set map parameter
	setURL := fmt.Sprintf("%s/service/parameter/set/map/%s", h.cfg.RobotBaseURL(), mapID)
	resp, err := h.client.Post(setURL, "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		return fmt.Errorf("set map: %w", err)
	}
	resp.Body.Close()

	time.Sleep(2 * time.Second)

	// Step 2: stop_robot_core
	commandURL := h.cfg.RobotBaseURL() + "/service/control/commands"
	stopPayload, _ := json.Marshal(map[string]string{"robot_control": "stop_robot_core"})
	resp, err = h.client.Post(commandURL, "application/json", bytes.NewReader(stopPayload))
	if err != nil {
		return fmt.Errorf("stop_robot_core: %w", err)
	}
	resp.Body.Close()

	log.Printf("[Action] switchMap: waiting 60s for restart...")
	time.Sleep(60 * time.Second)

	// Step 3: start_robot_core
	startPayload, _ := json.Marshal(map[string]string{"robot_control": "start_robot_core"})
	resp, err = h.client.Post(commandURL, "application/json", bytes.NewReader(startPayload))
	if err != nil {
		return fmt.Errorf("start_robot_core: %w", err)
	}
	resp.Body.Close()

	time.Sleep(30 * time.Second)

	// Step 4: select_mode delivery
	deliveryPayload, _ := json.Marshal(map[string]string{"select_mode": "delivery"})
	resp, err = h.client.Post(commandURL, "application/json", bytes.NewReader(deliveryPayload))
	if err != nil {
		return fmt.Errorf("select_mode delivery: %w", err)
	}
	resp.Body.Close()

	h.state.SetMapID(mapID)
	log.Printf("[Action] switchMap: completed, map=%s", mapID)
	return nil
}

func (h *InstantActionHandler) handleGetWaypoints(actionID string, params map[string]string) error {
	mapID := params["mapId"]
	mapName := params["mapName"]

	// If neither provided, use current map
	if mapID == "" && mapName == "" {
		snap := h.state.Snapshot()
		mapID = snap.MapID
	}
	if mapID == "" {
		mapID = mapName
	}
	if mapID == "" {
		return fmt.Errorf("no map name available")
	}

	// Determine if this is the current map
	snap := h.state.Snapshot()
	isCurrentMap := (mapName == "" || mapID == snap.MapID)

	var pois []MapPOI
	var err error

	if isCurrentMap {
		// Current map: use FastAPI for live POI data
		log.Printf("[Action] getWaypoints: fetching POI from FastAPI (current map %s)", mapID)
		pois, err = h.mapService.GetMapPOI(mapID)
		if err != nil {
			return fmt.Errorf("get POI: %w", err)
		}
	} else {
		// Non-current map: download ZIP from ATOM API, extract path.json
		log.Printf("[Action] getWaypoints: downloading ZIP from ATOM API (map %s)", mapName)
		zipData, zipErr := h.mapService.DownloadMapZIP(mapName)
		if zipErr != nil {
			return fmt.Errorf("download map ZIP: %w", zipErr)
		}
		pois, err = ExtractPOIFromZIP(zipData)
		if err != nil {
			return fmt.Errorf("extract POI from ZIP: %w", err)
		}
		log.Printf("[Action] getWaypoints: extracted %d POIs from ZIP", len(pois))
		// zipData is GC'd — no disk storage
	}

	waypointData := map[string]interface{}{
		"mapId":     mapID,
		"waypoints": pois,
	}

	data, err := json.Marshal(waypointData)
	if err != nil {
		return fmt.Errorf("marshal waypoints: %w", err)
	}

	h.bridge.PublishWaypoints(data)
	log.Printf("[Action] getWaypoints: published %d waypoints for map %s", len(pois), mapID)
	return nil
}
