package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/url"
	"os"
	"strconv"

	"github.com/gorilla/websocket"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintf(os.Stderr, "Usage: setpose <x> <y> <angle_degrees>\n")
		os.Exit(1)
	}
	x, _ := strconv.ParseFloat(os.Args[1], 64)
	y, _ := strconv.ParseFloat(os.Args[2], 64)
	angleDeg, _ := strconv.ParseFloat(os.Args[3], 64)

	// Convert degrees to radians, normalize
	if angleDeg > 180 {
		angleDeg -= 360
	}
	yaw := angleDeg * math.Pi / 180.0

	u := url.URL{Scheme: "ws", Host: "192.168.2.100:8000", Path: "/ws"}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	msg := map[string]interface{}{
		"topic": "set_initial_pose",
		"data": map[string]interface{}{
			"x":   x,
			"y":   y,
			"yaw": yaw,
		},
	}
	data, _ := json.Marshal(msg)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Fatalf("write: %v", err)
	}

	_, resp, err := conn.ReadMessage()
	if err != nil {
		log.Fatalf("read: %v", err)
	}
	fmt.Printf("Response: %s\n", resp)

	// Send 2 more times for AMCL convergence
	for i := 0; i < 2; i++ {
		conn.WriteMessage(websocket.TextMessage, data)
		conn.ReadMessage()
	}
	fmt.Printf("setPose done: x=%.3f y=%.3f yaw=%.4f (%.1f°)\n", x, y, yaw, angleDeg)
}
