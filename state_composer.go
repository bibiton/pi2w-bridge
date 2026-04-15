package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sync/atomic"
	"time"
)

var stateHeaderID uint64
var visHeaderID uint64
var connHeaderID uint64

// ComposeState builds the full VDA5050 state JSON.
func ComposeState(snap StateSnapshot, cfg *Config) ([]byte, error) {
	hid := atomic.AddUint64(&stateHeaderID, 1)

	informations := buildInformations(snap, cfg)

	state := map[string]interface{}{
		"headerId":           hid,
		"timestamp":          time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"version":            "2.0.0",
		"manufacturer":       cfg.Manufacturer,
		"serialNumber":       cfg.SerialNumber,
		"orderId":            snap.OrderID,
		"orderUpdateId":      snap.OrderUpdateID,
		"lastNodeId":         snap.LastNodeID,
		"lastNodeSequenceId": snap.LastNodeSequenceID,
		"driving":            snap.Driving,
		"newBaseRequest":     snap.NewBaseRequest,
		"paused":             snap.Paused,
		"operatingMode":      snap.OperatingMode,
		"agvPosition": map[string]interface{}{
			"x":                   round3(snap.PoseX),
			"y":                   round3(snap.PoseY),
			"theta":               round3(snap.PoseYaw),
			"mapId":               snap.MapID,
			"positionInitialized": snap.PositionInit,
		},
		"velocity": map[string]interface{}{
			"vx":    round3(snap.VelocityVX),
			"vy":    round3(snap.VelocityVY),
			"omega": round3(snap.VelocityOmega),
		},
		"batteryState": map[string]interface{}{
			"batteryCharge":  round1(snap.BatteryPercent),
			"batteryVoltage": round1(snap.BatteryVoltage),
			"charging":       snap.BatteryCharging,
		},
		"nodeStates":   snap.NodeStates,
		"edgeStates":   snap.EdgeStates,
		"actionStates": snap.ActionStates,
		"errors":       snap.Errors,
		"loads":        snap.Loads,
		"safetyState": map[string]interface{}{
			"eStop":          "NONE",
			"fieldViolation": false,
		},
		"informations": informations,
	}

	return json.Marshal(state)
}

// ComposeVisualization builds the lightweight VDA5050 visualization JSON.
func ComposeVisualization(snap StateSnapshot, cfg *Config) ([]byte, error) {
	hid := atomic.AddUint64(&visHeaderID, 1)

	vis := map[string]interface{}{
		"headerId":     hid,
		"timestamp":    time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"version":      "2.0.0",
		"manufacturer": cfg.Manufacturer,
		"serialNumber": cfg.SerialNumber,
		"agvPosition": map[string]interface{}{
			"x":                   round3(snap.PoseX),
			"y":                   round3(snap.PoseY),
			"theta":               round3(snap.PoseYaw),
			"mapId":               snap.MapID,
			"positionInitialized": snap.PositionInit,
		},
		"velocity": map[string]interface{}{
			"vx":    round3(snap.VelocityVX),
			"vy":    round3(snap.VelocityVY),
			"omega": round3(snap.VelocityOmega),
		},
		"driving": snap.Driving,
	}

	return json.Marshal(vis)
}

// ComposeConnection builds the VDA5050 connection JSON.
func ComposeConnection(connectionState string, cfg *Config) ([]byte, error) {
	hid := atomic.AddUint64(&connHeaderID, 1)

	conn := map[string]interface{}{
		"headerId":        hid,
		"timestamp":       time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"version":         "2.0.0",
		"manufacturer":    cfg.Manufacturer,
		"serialNumber":    cfg.SerialNumber,
		"connectionState": connectionState,
	}

	return json.Marshal(conn)
}

func buildInformations(snap StateSnapshot, cfg *Config) []map[string]interface{} {
	infos := []map[string]interface{}{
		{
			"infoType":        "agvIp",
			"infoDescription": cfg.RobotIP,
			"infoLevel":       "INFO",
		},
		{
			"infoType":        "robotStatus",
			"infoDescription": snap.Status,
			"infoLevel":       "INFO",
		},
	}

	if snap.Target != "" {
		infos = append(infos, map[string]interface{}{
			"infoType":        "currentTarget",
			"infoDescription": snap.Target,
			"infoLevel":       "INFO",
		})
	}

	if snap.Event != "" {
		infos = append(infos, map[string]interface{}{
			"infoType":        "lastEvent",
			"infoDescription": snap.Event,
			"infoLevel":       "INFO",
		})
	}

	if tunnelURL := GetTunnelURL(); tunnelURL != "" {
		infos = append(infos, map[string]interface{}{
			"infoType":        "tunnelUrl",
			"infoDescription": tunnelURL,
			"infoLevel":       "INFO",
			"infoReferences": []map[string]string{
				{"referenceKey": "url", "referenceValue": tunnelURL},
			},
		})
	}

	for _, mapID := range snap.MapList {
		isCurrent := "false"
		if mapID == snap.MapID {
			isCurrent = "true"
		}
		infos = append(infos, map[string]interface{}{
			"infoType":        "mapList",
			"infoDescription": "Available maps on AGV",
			"infoLevel":       "INFO",
			"infoReferences": []map[string]string{
				{"referenceKey": "mapId", "referenceValue": mapID},
				{"referenceKey": "currentMap", "referenceValue": isCurrent},
			},
		})
	}

	return infos
}

func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

// FormatStateLog returns a short log line for debugging.
func FormatStateLog(snap StateSnapshot) string {
	return fmt.Sprintf("pos=(%.2f,%.2f,%.1f°) map=%s bat=%.0f%% drv=%v status=%s",
		snap.PoseX, snap.PoseY, snap.PoseYaw*180/math.Pi,
		snap.MapID, snap.BatteryPercent, snap.Driving, snap.Status)
}
