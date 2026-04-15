package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	// Webhook server
	ListenAddr string

	// Robot connection
	RobotIP      string
	RobotPort    string
	RobotFastAPI string // port 8000 FastAPI for map/POI

	// MQTT
	MQTTBroker string
	MQTTUser   string
	MQTTPass   string
	MQTTPrefix string

	// VDA5050 identity
	Manufacturer string
	SerialNumber string
}

func LoadConfig() (*Config, error) {
	// Load .env file (ignore error if not found)
	_ = godotenv.Load()

	cfg := &Config{
		ListenAddr:     envOrDefault("LISTEN_ADDR", ":5201"),
		RobotIP:        envOrDefault("ROBOT_IP", "192.168.168.168"),
		RobotPort:    envOrDefault("ROBOT_PORT", "8080"),
		RobotFastAPI: envOrDefault("ROBOT_FASTAPI_URL", "http://192.168.168.168:8000"),
		MQTTBroker:     envOrDefault("MQTT_BROKER", "wss://nexmqtt.jini.tw:443/mqtt"),
		MQTTUser:       envOrDefault("MQTT_USER", "bibi"),
		MQTTPass:       envOrDefault("MQTT_PASS", "70595145"),
		MQTTPrefix:     envOrDefault("MQTT_PREFIX", "/uagv/v2"),
		Manufacturer:   envOrDefault("VDA_MANUFACTURER", "atom"),
		SerialNumber:   envOrDefault("VDA_SERIAL", "adai01"),
	}

	// Override with mqtt_config.json if it exists
	if err := cfg.loadMQTTConfigFile("mqtt_config.json"); err != nil {
		fmt.Printf("[Config] mqtt_config.json not loaded: %v\n", err)
	}

	return cfg, nil
}

func (c *Config) loadMQTTConfigFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var mc MQTTConfig
	if err := json.Unmarshal(data, &mc); err != nil {
		return err
	}
	if mc.Broker != "" {
		c.MQTTBroker = mc.Broker
	}
	if mc.Username != "" {
		c.MQTTUser = mc.Username
	}
	if mc.Password != "" {
		c.MQTTPass = mc.Password
	}
	if mc.Prefix != "" {
		c.MQTTPrefix = mc.Prefix
	}
	if mc.Manufacturer != "" {
		c.Manufacturer = mc.Manufacturer
	}
	if mc.SerialNumber != "" {
		c.SerialNumber = mc.SerialNumber
	}
	return nil
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
