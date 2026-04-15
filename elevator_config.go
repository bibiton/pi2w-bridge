package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
)

// ElevatorConfig holds the multi-floor elevator navigation configuration.
// Loaded from elevator_config.json at startup.
type ElevatorConfig struct {
	Floors   map[string]FloorConfig `json:"floors"`
	Elevator ElevatorDef            `json:"elevator"`
}

// FloorConfig maps a robot map name to its floor number and elevator hall station.
type FloorConfig struct {
	Floor           int     `json:"floor"`
	ElevatorHall    string  `json:"elevatorHall"`
	ElevatorHallYaw float64 `json:"elevatorHallYaw"` // SetInitialPose yaw correction (radians), default 0
}

// ElevatorDef defines the elevator tunnel map and car positions.
type ElevatorDef struct {
	TunnelMap    string            `json:"tunnelMap"`
	Hall         string            `json:"hall"`
	Cars         map[string]string `json:"cars"`
	WifiNetworks []WifiNetwork     `json:"wifiNetworks"`
}

// WifiNetwork defines a WiFi network used inside the elevator.
type WifiNetwork struct {
	SSID     string `json:"ssid"`
	Password string `json:"password"`
}

// LoadElevatorConfig loads the elevator configuration from a JSON file.
// Returns nil (no error) if the file does not exist — elevator feature is optional.
func LoadElevatorConfig(path string) (*ElevatorConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("[ElevatorConfig] %s not found — multi-floor disabled", path)
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var cfg ElevatorConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if len(cfg.Floors) == 0 {
		return nil, fmt.Errorf("elevator_config: no floors defined")
	}
	if cfg.Elevator.TunnelMap == "" {
		return nil, fmt.Errorf("elevator_config: missing elevator.tunnelMap")
	}
	if cfg.Elevator.Hall == "" {
		return nil, fmt.Errorf("elevator_config: missing elevator.hall")
	}
	if len(cfg.Elevator.Cars) == 0 {
		return nil, fmt.Errorf("elevator_config: no elevator cars defined")
	}

	log.Printf("[ElevatorConfig] Loaded: %d floors, tunnel=%s, hall=%s, %d cars, %d wifi networks",
		len(cfg.Floors), cfg.Elevator.TunnelMap, cfg.Elevator.Hall, len(cfg.Elevator.Cars), len(cfg.Elevator.WifiNetworks))
	for mapID, fc := range cfg.Floors {
		log.Printf("[ElevatorConfig]   floor %d: map=%s, hall=%s", fc.Floor, mapID, fc.ElevatorHall)
	}
	for carID, station := range cfg.Elevator.Cars {
		log.Printf("[ElevatorConfig]   car %s: station=%s", carID, station)
	}

	return &cfg, nil
}

// GetFloor returns the FloorConfig for a given mapId.
func (ec *ElevatorConfig) GetFloor(mapID string) *FloorConfig {
	if ec == nil {
		return nil
	}
	fc, ok := ec.Floors[mapID]
	if !ok {
		return nil
	}
	return &fc
}

// NeedsFloorChange checks if two mapIds represent different floors.
func (ec *ElevatorConfig) NeedsFloorChange(fromMapID, toMapID string) bool {
	if ec == nil || fromMapID == toMapID || fromMapID == "" || toMapID == "" {
		return false
	}
	fromFloor := ec.GetFloor(fromMapID)
	toFloor := ec.GetFloor(toMapID)
	if fromFloor == nil || toFloor == nil {
		return false
	}
	return fromFloor.Floor != toFloor.Floor
}
