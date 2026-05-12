package main

import (
	"fmt"
	"os"
)

// Config is the per-robot-session configuration.
type Config struct {
	// Robot connection
	RobotIP        string
	RobotPort      string // ATOM API port (8080)
	RobotFastAPI   string // http://ip:8000
	RobotFastAPIWS string // ws://ip:8000/ws  (if empty, RobotWSClient derives it)

	WebhookSecret string // X-Webhook-Secret this robot must present

	// MQTT (copied from ServerConfig)
	MQTTBroker string
	MQTTUser   string
	MQTTPass   string
	MQTTPrefix string

	// VDA5050 identity
	Manufacturer string
	SerialNumber string

	// atomros2-tts URL; empty disables voice
	TTSURL string
}

func (c *Config) RobotBaseURL() string {
	return fmt.Sprintf("http://%s:%s", c.RobotIP, c.RobotPort)
}

func (c *Config) TopicPrefix() string {
	return fmt.Sprintf("%s/%s/%s", c.MQTTPrefix, c.Manufacturer, c.SerialNumber)
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
