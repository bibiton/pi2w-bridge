package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// RobotWSClient maintains a persistent WebSocket connection to the robot's
// FastAPI server (port 8000). It subscribes to tracked_pose for real-time
// position updates and exposes methods to send navigation commands.
type RobotWSClient struct {
	url   string
	state *RobotState

	mu   sync.Mutex
	conn *websocket.Conn

	stopCh chan struct{}
}

// NewRobotWSClient creates a new WebSocket client for the robot FastAPI.
func NewRobotWSClient(cfg *Config, state *RobotState) *RobotWSClient {
	// Build ws:// URL from RobotFastAPI (which is http://...)
	u, err := url.Parse(cfg.RobotFastAPI)
	if err != nil {
		u = &url.URL{Host: cfg.RobotIP + ":8000"}
	}
	wsURL := fmt.Sprintf("ws://%s/ws", u.Host)

	return &RobotWSClient{
		url:    wsURL,
		state:  state,
		stopCh: make(chan struct{}),
	}
}

// Start launches the connection loop in background. It auto-reconnects.
func (c *RobotWSClient) Start() {
	go c.connectLoop()
}

// Stop closes the WebSocket connection and stops reconnecting.
func (c *RobotWSClient) Stop() {
	close(c.stopCh)
	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.mu.Unlock()
}

// NavigateTo sends a navigate_to_pose command to the robot.
func (c *RobotWSClient) NavigateTo(x, y, yaw float64) error {
	return c.sendJSON(map[string]interface{}{
		"topic": "navigate_to_pose",
		"data": map[string]interface{}{
			"x":   x,
			"y":   y,
			"yaw": yaw,
		},
	})
}

// CancelNavigation sends a cancel_navigation command.
func (c *RobotWSClient) CancelNavigation() error {
	return c.sendJSON(map[string]interface{}{
		"topic": "cancel_navigation",
		"data":  map[string]interface{}{},
	})
}

// SetInitialPose sends a set_initial_pose command (relocalize).
func (c *RobotWSClient) SetInitialPose(x, y, yaw float64) error {
	return c.sendJSON(map[string]interface{}{
		"topic": "set_initial_pose",
		"data": map[string]interface{}{
			"x":   x,
			"y":   y,
			"yaw": yaw,
		},
	})
}

// SendCmdVel sends a remote control velocity command.
func (c *RobotWSClient) SendCmdVel(linearX, angularZ float64) error {
	return c.sendJSON(map[string]interface{}{
		"topic": "/remote_control",
		"data": map[string]interface{}{
			"linear_x":  linearX,
			"angular_z": angularZ,
		},
	})
}

// IsConnected returns true if the WebSocket is currently connected.
func (c *RobotWSClient) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// --- internal ---

func (c *RobotWSClient) sendJSON(v interface{}) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("robot WS not connected")
	}

	data, err := json.Marshal(v)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("robot WS not connected")
	}
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c *RobotWSClient) connectLoop() {
	for {
		select {
		case <-c.stopCh:
			return
		default:
		}

		if err := c.connect(); err != nil {
			log.Printf("[RobotWS] Connect failed: %v (retry in 3s)", err)
			select {
			case <-time.After(3 * time.Second):
			case <-c.stopCh:
				return
			}
			continue
		}

		// Connected — enable tracked_pose subscription
		c.enableTrackedPose()

		// Read loop (blocks until disconnect)
		c.readLoop()

		log.Printf("[RobotWS] Disconnected, reconnecting in 3s...")
		select {
		case <-time.After(3 * time.Second):
		case <-c.stopCh:
			return
		}
	}
}

func (c *RobotWSClient) connect() error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
	}
	conn, _, err := dialer.Dial(c.url, nil)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	log.Printf("[RobotWS] Connected to %s", c.url)
	return nil
}

func (c *RobotWSClient) enableTrackedPose() {
	err := c.sendJSON(map[string]interface{}{
		"topic": "control_tracked_pose",
		"data":  "start",
	})
	if err != nil {
		log.Printf("[RobotWS] Failed to enable tracked_pose: %v", err)
	} else {
		log.Printf("[RobotWS] Enabled tracked_pose subscription")
	}
}

func (c *RobotWSClient) readLoop() {
	for {
		select {
		case <-c.stopCh:
			return
		default:
		}

		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()
		if conn == nil {
			return
		}

		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[RobotWS] Read error: %v", err)
			c.mu.Lock()
			c.conn = nil
			c.mu.Unlock()
			return
		}

		c.handleMessage(msg)
	}
}

func (c *RobotWSClient) handleMessage(msg []byte) {
	var envelope struct {
		Topic string          `json:"topic"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg, &envelope); err != nil {
		return
	}

	switch envelope.Topic {
	case "/tracked_pose":
		c.handleTrackedPose(envelope.Data)
	case "system":
		// Log system responses
		var s string
		json.Unmarshal(envelope.Data, &s)
		log.Printf("[RobotWS] system: %s", s)
	}
}

func (c *RobotWSClient) handleTrackedPose(data json.RawMessage) {
	var pose struct {
		Pose struct {
			Position struct {
				X float64 `json:"x"`
				Y float64 `json:"y"`
			} `json:"position"`
			Orientation struct {
				X float64 `json:"x"`
				Y float64 `json:"y"`
				Z float64 `json:"z"`
				W float64 `json:"w"`
			} `json:"orientation"`
		} `json:"pose"`
	}
	if err := json.Unmarshal(data, &pose); err != nil {
		return
	}

	// Quaternion to yaw
	q := pose.Pose.Orientation
	if q.W == 0 && q.X == 0 && q.Y == 0 && q.Z == 0 {
		return // invalid
	}
	sinyCosp := 2.0 * (q.W*q.Z + q.X*q.Y)
	cosyCosp := 1.0 - 2.0*(q.Y*q.Y+q.Z*q.Z)
	yaw := math.Atan2(sinyCosp, cosyCosp)

	// Update state
	c.state.mu.Lock()
	c.state.PoseX = pose.Pose.Position.X
	c.state.PoseY = pose.Pose.Position.Y
	c.state.PoseYaw = yaw
	c.state.PositionInit = true
	c.state.LastUpdate = time.Now()
	c.state.mu.Unlock()
}
