package main

import (
	"encoding/json"
	"sync/atomic"
	"time"
)

var factsheetHeaderID uint64

// ComposeFactsheet builds the VDA5050 factsheet JSON (retained on connect).
func ComposeFactsheet(cfg *Config) ([]byte, error) {
	hid := atomic.AddUint64(&factsheetHeaderID, 1)

	factsheet := map[string]interface{}{
		"headerId":     hid,
		"timestamp":    time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"version":      "2.0.0",
		"manufacturer": cfg.Manufacturer,
		"serialNumber": cfg.SerialNumber,
		"typeSpecification": map[string]interface{}{
			"seriesName":        "ATOM",
			"seriesDescription": "ATOM Robot Platform",
			"agvKinematic":      "DIFF",
			"agvClass":          "FORKLIFT",
			"maxLoadMass":       0,
			"localizationTypes": []string{"NATURAL"},
			"navigationTypes":   []string{"AUTONOMOUS"},
		},
		"physicalParameters": map[string]interface{}{
			"speedMin":          0.0,
			"speedMax":          1.5,
			"accelerationMax":   0.5,
			"decelerationMax":   0.5,
			"heightMin":         0.0,
			"heightMax":         0.5,
			"width":             0.5,
			"length":            0.6,
		},
		"protocolLimits": map[string]interface{}{
			"maxStringLens": map[string]interface{}{
				"msgLen":       10000,
				"topicSerialLen": 40,
				"topicElemLen":   40,
				"idLen":          36,
				"idNumericalOnly": false,
				"enumLen":        50,
			},
			"maxArrayLens": map[string]interface{}{
				"order.nodes":              100,
				"order.edges":              100,
				"node.actions":             10,
				"edge.actions":             10,
				"actions.actionsParameters": 10,
				"instantActions":           10,
				"trajectoryKnotVector":     0,
			},
			"timing": map[string]interface{}{
				"minOrderInterval":    1.0,
				"minStateInterval":    1.0,
				"defaultStateInterval": 30.0,
			},
		},
		"protocolFeatures": map[string]interface{}{
			"optionalParameters": []map[string]interface{}{
				{"parameter": "order.zoneSetId", "support": "SUPPORTED", "description": ""},
				{"parameter": "order.nodes.nodePosition.allowedDeviationXY", "support": "SUPPORTED", "description": ""},
			},
			"agvActions": []map[string]interface{}{
				{
					"actionType":        "uploadMap",
					"actionDescription": "Upload current map image to presigned URL",
					"actionScopes":      []string{"INSTANT"},
				},
				{
					"actionType":        "getWaypoints",
					"actionDescription": "Get waypoints/POI from current map",
					"actionScopes":      []string{"INSTANT"},
				},
				{
					"actionType":        "stateRequest",
					"actionDescription": "Request immediate state publish",
					"actionScopes":      []string{"INSTANT"},
				},
			},
		},
	}

	return json.Marshal(factsheet)
}
