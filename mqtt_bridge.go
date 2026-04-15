package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type MQTTBridge struct {
	client     mqtt.Client
	cfg        *Config
	state      *RobotState
	mapService *MapService
	robotWS    *RobotWSClient
	elevatorCfg *ElevatorConfig

	// Channels for triggering immediate publishes
	stateRequestCh chan struct{}
	stopCh         chan struct{}

	// InstantAction handler
	actionHandler *InstantActionHandler

	// Order handler
	orderHandler *OrderHandler

	// Elevator service (discovery + status + call/enter/exit)
	elevatorService *ElevatorService
}

func NewMQTTBridge(cfg *Config, state *RobotState, mapService *MapService, robotWS *RobotWSClient, elevatorCfg *ElevatorConfig) *MQTTBridge {
	return &MQTTBridge{
		cfg:            cfg,
		state:          state,
		mapService:     mapService,
		robotWS:        robotWS,
		elevatorCfg:    elevatorCfg,
		stateRequestCh: make(chan struct{}, 1),
		stopCh:         make(chan struct{}),
	}
}

func (mb *MQTTBridge) Connect() error {
	// Initialize handlers before connecting so onConnect subscriptions
	// can handle messages immediately
	mb.actionHandler = NewInstantActionHandler(mb.cfg, mb.state, mb.mapService, mb, mb.robotWS)
	mb.orderHandler = NewOrderHandler(mb.cfg, mb.state, mb, mb.robotWS, mb.elevatorCfg)

	opts := mqtt.NewClientOptions()
	opts.AddBroker(mb.cfg.MQTTBroker)
	opts.SetClientID(fmt.Sprintf("atomros2-bridge-%d", time.Now().UnixNano()))
	if mb.cfg.MQTTUser != "" {
		opts.SetUsername(mb.cfg.MQTTUser)
	}
	if mb.cfg.MQTTPass != "" {
		opts.SetPassword(mb.cfg.MQTTPass)
	}
	opts.SetKeepAlive(30 * time.Second)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(5 * time.Second)
	opts.SetMaxReconnectInterval(30 * time.Second)
	opts.SetCleanSession(true)

	// Last Will: connection OFFLINE
	connTopic := mb.cfg.TopicPrefix() + "/connection"
	willPayload, _ := ComposeConnection("CONNECTIONBROKEN", mb.cfg)
	opts.SetWill(connTopic, string(willPayload), 1, true)

	opts.SetOnConnectHandler(func(c mqtt.Client) {
		log.Printf("[MQTT] Connected to %s", mb.cfg.MQTTBroker)
		mb.onConnect(c)
	})

	opts.SetConnectionLostHandler(func(c mqtt.Client, err error) {
		log.Printf("[MQTT] Connection lost: %v", err)
		mb.state.SetConnectionState("CONNECTIONBROKEN")
	})

	mb.client = mqtt.NewClient(opts)
	token := mb.client.Connect()
	if !token.WaitTimeout(10 * time.Second) {
		return fmt.Errorf("MQTT connect timeout")
	}
	if token.Error() != nil {
		return fmt.Errorf("MQTT connect: %w", token.Error())
	}

	return nil
}

func (mb *MQTTBridge) onConnect(c mqtt.Client) {
	prefix := mb.cfg.TopicPrefix()

	// Publish connection ONLINE
	mb.state.SetConnectionState("ONLINE")
	mb.publishConnection("ONLINE")

	// Publish factsheet (retained)
	mb.publishFactsheet()

	// Subscribe to instantActions
	topic := prefix + "/instantActions"
	c.Subscribe(topic, 1, func(_ mqtt.Client, msg mqtt.Message) {
		log.Printf("[MQTT] Received instantActions: %s", string(msg.Payload()))
		mb.handleInstantActions(msg.Payload())
	})
	log.Printf("[MQTT] Subscribed to %s", topic)

	// Subscribe to order
	orderTopic := prefix + "/order"
	c.Subscribe(orderTopic, 1, func(_ mqtt.Client, msg mqtt.Message) {
		log.Printf("[MQTT] Received order: %s", string(msg.Payload()))
		if mb.orderHandler != nil {
			mb.orderHandler.HandleOrder(msg.Payload())
		}
	})
	log.Printf("[MQTT] Subscribed to %s", orderTopic)
}

func (mb *MQTTBridge) StartPublishLoops() {
	// State publish loop (1Hz)
	go mb.stateLoop()

	// Visualization publish loop (5Hz = 200ms)
	go mb.visualizationLoop()

	// Connection publish loop (every 15s)
	go mb.connectionLoop()
}

func (mb *MQTTBridge) stateLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-mb.stopCh:
			return
		case <-mb.stateRequestCh:
			mb.publishState()
		case <-ticker.C:
			mb.publishState()
		}
	}
}

func (mb *MQTTBridge) visualizationLoop() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-mb.stopCh:
			return
		case <-ticker.C:
			mb.publishVisualization()
		}
	}
}

func (mb *MQTTBridge) connectionLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-mb.stopCh:
			return
		case <-ticker.C:
			snap := mb.state.Snapshot()
			connState := snap.ConnectionState
			// If no fresh data from robot, mark as CONNECTIONBROKEN
			if !mb.state.IsDataFresh(10 * time.Second) {
				connState = "CONNECTIONBROKEN"
			}
			mb.publishConnection(connState)
		}
	}
}

func (mb *MQTTBridge) publishState() {
	snap := mb.state.Snapshot()
	data, err := ComposeState(snap, mb.cfg)
	if err != nil {
		log.Printf("[MQTT] ComposeState error: %v", err)
		return
	}
	topic := mb.cfg.TopicPrefix() + "/state"
	mb.publish(topic, data, 0, false)
}

func (mb *MQTTBridge) publishVisualization() {
	snap := mb.state.Snapshot()
	data, err := ComposeVisualization(snap, mb.cfg)
	if err != nil {
		log.Printf("[MQTT] ComposeVisualization error: %v", err)
		return
	}
	topic := mb.cfg.TopicPrefix() + "/visualization"
	mb.publish(topic, data, 0, false)
}

func (mb *MQTTBridge) publishConnection(connState string) {
	data, err := ComposeConnection(connState, mb.cfg)
	if err != nil {
		log.Printf("[MQTT] ComposeConnection error: %v", err)
		return
	}
	topic := mb.cfg.TopicPrefix() + "/connection"
	mb.publish(topic, data, 1, true)
}

func (mb *MQTTBridge) publishFactsheet() {
	data, err := ComposeFactsheet(mb.cfg)
	if err != nil {
		log.Printf("[MQTT] ComposeFactsheet error: %v", err)
		return
	}
	topic := mb.cfg.TopicPrefix() + "/factsheet"
	mb.publish(topic, data, 1, true)
	log.Printf("[MQTT] Factsheet published (retained)")
}

// PublishWaypoints publishes waypoint data to the waypoints topic.
func (mb *MQTTBridge) PublishWaypoints(data []byte) {
	topic := mb.cfg.TopicPrefix() + "/waypoints"
	mb.publish(topic, data, 1, false)
	log.Printf("[MQTT] Published waypoints (%d bytes)", len(data))
}

// TriggerStatePublish triggers an immediate state publish.
func (mb *MQTTBridge) TriggerStatePublish() {
	select {
	case mb.stateRequestCh <- struct{}{}:
	default:
	}
}

func (mb *MQTTBridge) publish(topic string, payload []byte, qos byte, retained bool) {
	if mb.client == nil || !mb.client.IsConnected() {
		return
	}
	token := mb.client.Publish(topic, qos, retained, payload)
	token.WaitTimeout(2 * time.Second)
	if token.Error() != nil {
		log.Printf("[MQTT] Publish error on %s: %v", topic, token.Error())
	}
}

func (mb *MQTTBridge) handleInstantActions(payload []byte) {
	// Parse the raw JSON to check for both actions and actionStates
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		log.Printf("[MQTT] Failed to parse instantActions: %v", err)
		return
	}

	// --- Handle actionStates (responses from IoT Gateway) ---
	if rawStates, ok := raw["actionStates"]; ok {
		var states []ActionStateMsg
		if err := json.Unmarshal(rawStates, &states); err == nil && len(states) > 0 {
			if mb.elevatorService != nil {
				mb.elevatorService.HandleActionStates(states)
			}
			return // actionStates messages are responses, not actions to execute
		}
	}

	// --- Handle instantActions / actions (commands from platform) ---
	type actionEntry struct {
		ActionID         string                   `json:"actionId"`
		ActionType       string                   `json:"actionType"`
		ActionParameters []map[string]interface{} `json:"actionParameters"`
	}

	var actions []actionEntry
	if rawIA, ok := raw["instantActions"]; ok {
		json.Unmarshal(rawIA, &actions)
	}
	if len(actions) == 0 {
		if rawA, ok := raw["actions"]; ok {
			json.Unmarshal(rawA, &actions)
		}
	}

	if mb.actionHandler == nil {
		log.Printf("[MQTT] actionHandler not ready, ignoring instantActions")
		return
	}

	for _, action := range actions {
		// Skip tw_* actions — they are T-Extension actions meant for IoT Gateway,
		// not for the Bridge to process locally.
		if isTwAction(action.ActionType) {
			log.Printf("[MQTT] Skipping tw_ action: %s (handled by IoT Gateway)", action.ActionType)
			continue
		}

		params := make(map[string]string)
		for _, p := range action.ActionParameters {
			key, _ := p["key"].(string)
			val, _ := p["value"].(string)
			if key != "" {
				params[key] = val
			}
		}
		mb.actionHandler.Handle(action.ActionID, action.ActionType, params)
	}
}

func (mb *MQTTBridge) Stop() {
	close(mb.stopCh)

	// Publish OFFLINE connection
	mb.publishConnection("OFFLINE")

	if mb.client != nil && mb.client.IsConnected() {
		mb.client.Disconnect(1000)
	}
}
