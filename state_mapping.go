package main

import (
	"fmt"
	"strings"
	"time"
)

// ApplyWebhookData maps raw webhook JSON data to RobotState fields.
// Mirrors the logic in atom_bridge_server.py BridgeHandler.do_POST.
func ApplyWebhookData(rs *RobotState, item map[string]interface{}) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	rs.LastUpdate = time.Now()

	// --- Route status ---
	rawStatus := ""
	if routeStatus, ok := item["route_status"].(map[string]interface{}); ok {
		if s, ok := routeStatus["status"].(string); ok {
			rawStatus = s
		}
	}
	if rawStatus == "" {
		for _, key := range []string{"routing status", "routing_status", "status"} {
			if s, ok := item[key].(string); ok && s != "" {
				rawStatus = s
				break
			}
		}
	}
	if rawStatus != "" {
		rawStatus = strings.ToLower(rawStatus)
		rs.Status = rawStatus
		applyDrivingState(rs, rawStatus)
	}

	// --- Target ---
	if tgt := extractTarget(item, "target"); tgt != "" {
		rs.Target = tgt
	}
	// Also check nested route_status.target
	if routeStatus, ok := item["route_status"].(map[string]interface{}); ok {
		if tgt := extractTarget(routeStatus, "target"); tgt != "" {
			rs.Target = tgt
		}
	}

	// --- Battery ---
	if _, hasBat := item["battery_level"]; hasBat {
		rs.BatteryPercent = getFloat(item, "battery_level")
	} else if _, hasBat := item["battery"]; hasBat {
		rs.BatteryPercent = getFloat(item, "battery")
	}

	// --- Event ---
	// ATOM API v1.0.7: "show_charging" = 充電中, "remove_charging" = 未充電
	if evt, ok := item["event"]; ok {
		switch v := evt.(type) {
		case string:
			rs.Event = v
			if v == "show_charging" {
				rs.BatteryCharging = true
			} else if v == "remove_charging" || v == "" {
				rs.BatteryCharging = false
			}
		case map[string]interface{}:
			if name, ok := v["name"].(string); ok {
				rs.Event = name
				if name == "show_charging" {
					rs.BatteryCharging = true
				} else if name == "remove_charging" {
					rs.BatteryCharging = false
				}
			}
		}
	}

	// --- Pose ---
	if pose, ok := item["pose"].(map[string]interface{}); ok {
		if pos, ok := pose["position"].(map[string]interface{}); ok {
			ori, _ := pose["orientation"].(map[string]interface{})
			applyPose(rs, pos, ori)
		}
		// Also extract velocity from pose (pmx-action-api format: {position, velocity})
		if vel, ok := pose["velocity"].(map[string]interface{}); ok {
			rs.VelocityVX = getFloatFromMap(vel, "x")
			rs.VelocityVY = getFloatFromMap(vel, "y")
			rs.VelocityOmega = getFloatFromMap(vel, "z")
		}
	}

	// --- Lidar ---
	if lf, ok := item["lidar_front"].(map[string]interface{}); ok {
		if _, hasRanges := lf["ranges"]; hasRanges {
			rs.LastScan = lf
		}
	}
	if lr, ok := item["lidar_rear"].(map[string]interface{}); ok {
		if _, hasRanges := lr["ranges"]; hasRanges {
			rs.LastScanRear = lr
		}
	}
	// Handle flat lidar data with header.frame_id
	if _, hasRanges := item["ranges"]; hasRanges {
		if header, ok := item["header"].(map[string]interface{}); ok {
			frameID, _ := header["frame_id"].(string)
			if strings.Contains(strings.ToLower(frameID), "rear") || strings.Contains(strings.ToLower(frameID), "back") {
				rs.LastScanRear = item
			} else {
				rs.LastScan = item
			}
		}
	}
}

func applyDrivingState(rs *RobotState, status string) {
	switch status {
	case "delivering", "return", "rerouting":
		rs.Driving = true
		rs.OperatingMode = "AUTOMATIC"
	case "standby", "waiting", "arrived", "none":
		rs.Driving = false
		// Notify order handler that navigation completed
		if status == "arrived" || status == "standby" {
			rs.NotifyNavArrived()
		}
	case "blocking":
		rs.Driving = false
	case "goto charging":
		rs.Driving = true
		rs.BatteryCharging = true
	default:
		// Keep current state
	}
}

func applyPose(rs *RobotState, pos, ori map[string]interface{}) {
	// Skip if no orientation — this is velocity data, not a real pose
	if ori == nil {
		return
	}

	qx := getFloatFromMap(ori, "x")
	qy := getFloatFromMap(ori, "y")
	qz := getFloatFromMap(ori, "z")
	qw := getFloatFromMap(ori, "w")

	// Skip invalid quaternion (all zeros)
	if qw == 0 && qx == 0 && qy == 0 && qz == 0 {
		return
	}

	rs.PoseX = getFloatFromMap(pos, "x")
	rs.PoseY = getFloatFromMap(pos, "y")
	rs.PoseYaw = QuatToYaw(qx, qy, qz, qw)
	rs.PositionInit = true
}

// extractTarget handles target as string or as dict (e.g. {"deliver_to_location": ["C01"]}).
func extractTarget(m map[string]interface{}, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		if t != "None" {
			return t
		}
	case map[string]interface{}:
		if dc, ok := t["delivery_command"].(map[string]interface{}); ok {
			if locs, ok := dc["deliver_to_location"].([]interface{}); ok && len(locs) > 0 {
				if s, ok := locs[0].(string); ok {
					return s
				}
			}
			if loc, ok := dc["deliver_to_location"].(string); ok {
				return loc
			}
		}
	}
	return ""
}

func getFloat(m map[string]interface{}, key string) float64 {
	v, ok := m[key]
	if !ok {
		return 0
	}
	return toFloat64(v)
}

func getFloatFromMap(m map[string]interface{}, key string) float64 {
	v, ok := m[key]
	if !ok {
		return 0
	}
	return toFloat64(v)
}

func toFloat64(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case string:
		var f float64
		fmt.Sscanf(strings.TrimSuffix(n, "%"), "%f", &f)
		return f
	default:
		return 0
	}
}
