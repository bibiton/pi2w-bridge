package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// RegisterWebhook registers this robot's webhook URL with the robot's ATOM API.
// If webhookURLOrAddr already starts with "http://" or "https://", it is used verbatim.
// Otherwise it is treated as a listen-addr (":5201", "0.0.0.0:5201") and the URL is
// constructed from the local IP that can reach the robot.
// It retries on failure with exponential backoff and then switches to delivery mode.
func RegisterWebhook(cfg *Config, webhookURLOrAddr string, stopCh <-chan struct{}) {
	go func() {
		// Wait for server to be ready
		select {
		case <-stopCh:
			return
		case <-time.After(2 * time.Second):
		}

		var webhookURL string
		if strings.HasPrefix(webhookURLOrAddr, "http://") || strings.HasPrefix(webhookURLOrAddr, "https://") {
			webhookURL = webhookURLOrAddr
		} else {
			myIP := getLocalIP(cfg.RobotIP)
			// Extract port from listenAddr (handles ":5201", "0.0.0.0:5201", "5201")
			port := webhookURLOrAddr
			if idx := strings.LastIndex(port, ":"); idx >= 0 {
				port = port[idx+1:]
			}
			webhookURL = fmt.Sprintf("http://%s:%s/", myIP, port)
		}
		log.Printf("[Register] Webhook URL: %s", webhookURL)

		retryInterval := 5 * time.Second
		maxRetry := 30 * time.Second

		for {
			err := doRegister(cfg.RobotBaseURL(), webhookURL)
			if err != nil {
				log.Printf("[Register] Failed: %v (retry in %v)", err, retryInterval)
				select {
				case <-stopCh:
					return
				case <-time.After(retryInterval):
				}
				if retryInterval < maxRetry {
					retryInterval = retryInterval * 2
					if retryInterval > maxRetry {
						retryInterval = maxRetry
					}
				}
				continue
			}
			log.Printf("[Register] Webhook registered successfully!")
			break
		}

		// Wait for robot's Nav2 stack to be fully ready, then switch to delivery mode
		go activateDeliveryMode(cfg, stopCh)

		// Keep re-registering periodically to handle robot restarts
		go keepAliveRegister(cfg.RobotBaseURL(), webhookURL, stopCh)
	}()
}

// sleepOrStop waits for d or until stopCh is closed. It returns true if the
// full duration elapsed, false if stopCh fired (caller should bail out).
func sleepOrStop(d time.Duration, stopCh <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-stopCh:
		return false
	case <-t.C:
		return true
	}
}

func doRegister(robotBaseURL, webhookURL string) error {
	payload := map[string]interface{}{
		"webhook_url": webhookURL,
		"report item": []string{
			"routing status",
			"realtime position",
			"battery level",
		},
		"report mode": "repeat",
		"report rate": 1,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	url := robotBaseURL + "/service/control/commands"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// keepAliveRegister re-registers every 60 seconds to handle robot restarts.
func keepAliveRegister(robotBaseURL, webhookURL string, stopCh <-chan struct{}) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			if err := doRegister(robotBaseURL, webhookURL); err != nil {
				log.Printf("[Register] Keep-alive re-register failed: %v", err)
			}
		}
	}
}

// activateDeliveryMode waits for the robot's Nav2 to be ready,
// reads current map from ATOM API, then switches to delivery mode.
//
// Flow (per ATOM API v1.0.5 official docs):
//  1. GET current_map_name → determine current map
//  2. POST /service/parameter/set/map/{map_id} → select map (if LASTMAP_NAME env overrides)
//  3. POST /service/control/commands → stop_robot_core (stops all ROS services)
//  4. Wait for node_manager auto-restart (via .bashrc on tty1), or start_robot_core as fallback
//  5. start_robot_core defaults to delivery mode with the selected map
func activateDeliveryMode(cfg *Config, stopCh <-chan struct{}) {
	log.Println("[Delivery] Waiting 30s for Nav2 stack to be ready...")
	select {
	case <-stopCh:
		return
	case <-time.After(30 * time.Second):
	}

	client := &http.Client{Timeout: 10 * time.Second}
	baseURL := cfg.RobotBaseURL()
	commandURL := baseURL + "/service/control/commands"

	// Read current map from ATOM API
	currentMap := readLastmap(cfg)
	if currentMap == "" {
		log.Println("[Delivery] WARNING: Could not read current map, skipping delivery mode activation")
		return
	}

	// Check if user wants to override map via env
	desiredMap := os.Getenv("LASTMAP_NAME")
	if desiredMap == "" {
		desiredMap = currentMap
	}

	log.Printf("[Delivery] Current map: %s, Desired map: %s", currentMap, desiredMap)

	// Step 1: Set map parameter (always call to ensure consistency)
	setMapURL := fmt.Sprintf("%s/service/parameter/set/map/%s", baseURL, desiredMap)
	for attempt := 1; attempt <= 3; attempt++ {
		resp, err := client.Post(setMapURL, "application/json", bytes.NewReader([]byte("{}")))
		if err != nil {
			log.Printf("[Delivery] Set map attempt %d failed: %v", attempt, err)
			if !sleepOrStop(5*time.Second, stopCh) {
				return
			}
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		log.Printf("[Delivery] Set map → HTTP %d: %s", resp.StatusCode, string(body))
		break
	}

	if !sleepOrStop(2*time.Second, stopCh) {
		return
	}

	// Wait for map_loaded by polling routing status (up to 90s)
	// No robot_core restart needed — set/map API handles switching directly
	log.Println("[Delivery] Waiting for map_loaded...")
	statusURL := baseURL + "/service/system/routing/status/get"
	mapLoadDeadline := time.NewTimer(90 * time.Second)
	defer mapLoadDeadline.Stop()
	pollTicker := time.NewTicker(3 * time.Second)
	defer pollTicker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-mapLoadDeadline.C:
			log.Println("[Delivery] WARNING: map_loaded timeout (90s), proceeding anyway")
			goto mapLoadDone
		case <-pollTicker.C:
			if sr, err := client.Get(statusURL); err == nil {
				var result map[string]interface{}
				if json.NewDecoder(sr.Body).Decode(&result) == nil {
					if rs, ok := result["route_status"].(map[string]interface{}); ok {
						if status, _ := rs["status"].(string); status == "map_loaded" {
							log.Println("[Delivery] map_loaded detected")
							sr.Body.Close()
							goto mapLoadDone
						}
					}
				}
				sr.Body.Close()
			}
		}
	}
mapLoadDone:

	// Stabilization buffer
	if !sleepOrStop(5*time.Second, stopCh) {
		return
	}

	// Step 6: Ensure delivery mode (select_mode: delivery, NO map_name per official docs)
	deliveryPayload, _ := json.Marshal(map[string]string{"select_mode": "delivery"})
	for attempt := 1; attempt <= 3; attempt++ {
		resp, err := client.Post(commandURL, "application/json", bytes.NewReader(deliveryPayload))
		if err != nil {
			log.Printf("[Delivery] select_mode delivery attempt %d failed: %v", attempt, err)
			if !sleepOrStop(5*time.Second, stopCh) {
				return
			}
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		log.Printf("[Delivery] select_mode delivery → HTTP %d: %s", resp.StatusCode, string(body))
		break
	}

	// Verify final state
	finalMode := getRobotMode(baseURL, client)
	finalMap := readLastmap(cfg)
	log.Printf("[Delivery] Final state — mode: %s, map: %s", finalMode, finalMap)
}

// getRobotMode queries the ATOM API for current robot mode.
func getRobotMode(baseURL string, client *http.Client) string {
	resp, err := client.Get(baseURL + "/service/control/get/robot_mode")
	if err != nil {
		log.Printf("[Delivery] Cannot query robot_mode: %v", err)
		return "unknown"
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result map[string]string
	if err := json.Unmarshal(body, &result); err != nil {
		return "unknown"
	}
	mode := result["robot_mode"]
	if mode == "" {
		mode = result["robot mode"]
	}
	return mode
}

// readLastmap returns the current map name.
// Priority: LASTMAP_NAME env > ATOM API current_map_name.
func readLastmap(cfg *Config) string {
	if name := os.Getenv("LASTMAP_NAME"); name != "" {
		log.Printf("[Delivery] Using LASTMAP_NAME env: %s", name)
		return name
	}

	// Query ATOM API for current map name
	url := cfg.RobotBaseURL() + "/service/parameter/get/current_map_name"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		log.Printf("[Delivery] Cannot query current_map_name: %v", err)
		return ""
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		log.Printf("[Delivery] current_map_name API returned HTTP %d", resp.StatusCode)
		return ""
	}

	var result map[string]string
	if err := json.Unmarshal(body, &result); err != nil {
		log.Printf("[Delivery] Parse current_map_name error: %v", err)
		return ""
	}

	name := result["current_map_name"]
	if name == "" {
		name = result["current map name"]
	}
	if name != "" {
		log.Printf("[Delivery] Current map from ATOM API: %s", name)
	}
	return name
}

// getLocalIP finds the local IP address that can reach the robot.
// If LOCAL_IP env is set, use that directly (useful when auto-detect picks wrong interface).
func getLocalIP(robotIP string) string {
	if ip := os.Getenv("LOCAL_IP"); ip != "" {
		return ip
	}
	conn, err := net.DialTimeout("udp", robotIP+":8080", 2*time.Second)
	if err != nil {
		log.Printf("[Register] Cannot determine local IP: %v, using 127.0.0.1", err)
		return "127.0.0.1"
	}
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}
