package main

import (
	"encoding/json"
	"os"
)

// MQTTConfig represents the mqtt_config.json file structure.
type MQTTConfig struct {
	Broker       string `json:"broker"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	Prefix       string `json:"prefix"`
	Manufacturer string `json:"manufacturer"`
	SerialNumber string `json:"serial_number"`
}

func LoadMQTTConfig(path string) (*MQTTConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var mc MQTTConfig
	if err := json.Unmarshal(data, &mc); err != nil {
		return nil, err
	}
	return &mc, nil
}

func SaveMQTTConfig(path string, mc *MQTTConfig) error {
	data, err := json.MarshalIndent(mc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
