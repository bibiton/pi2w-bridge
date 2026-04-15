#!/usr/bin/env python3
"""
Test tw_elevator_discovery and tw_elevator_status via MQTT.
Publishes instantActions to the robot's topic and listens for responses.
"""
import json
import ssl
import sys
import time
import paho.mqtt.client as mqtt

BROKER = "nexmqtt.jini.tw"
PORT = 443
WS_PATH = "/mqtt"
USER = "bibi"
PASS = "70595145"
PREFIX = "/uagv/v2"
MFR = "atom"
SN = "adai01"

TOPIC = f"{PREFIX}/{MFR}/{SN}/instantActions"

received = []

def on_connect(client, userdata, flags, rc):
    if rc == 0:
        print(f"[OK] Connected to {BROKER}")
        # Subscribe to instantActions to see responses from IoT Gateway
        client.subscribe(TOPIC, 1)
        print(f"[OK] Subscribed to {TOPIC}")
    else:
        print(f"[ERR] Connect failed rc={rc}")

def on_message(client, userdata, msg):
    try:
        data = json.loads(msg.payload)
        # Filter out our own published messages by checking for actionStates
        if "actionStates" in data:
            print(f"\n=== RESPONSE from IoT Gateway ===")
            print(json.dumps(data, indent=2, ensure_ascii=False))
            received.append(data)
        elif "instantActions" in data:
            # This is our own message echoed back, or another instantAction
            actions = data.get("instantActions", [])
            for a in actions:
                if a.get("actionType", "").startswith("tw_"):
                    print(f"[echo] Sent: {a['actionType']} (actionId={a['actionId']})")
        else:
            print(f"\n=== OTHER MSG on {msg.topic} ===")
            print(json.dumps(data, indent=2, ensure_ascii=False)[:500])
    except:
        print(f"[raw] {msg.payload[:200]}")

def send_discovery(client):
    msg = {
        "headerId": int(time.time() * 1000),
        "timestamp": time.strftime("%Y-%m-%dT%H:%M:%S.000Z", time.gmtime()),
        "version": "2.0.0",
        "manufacturer": MFR,
        "serialNumber": SN,
        "instantActions": [
            {
                "actionId": f"discovery_{int(time.time())}",
                "actionType": "tw_elevator_discovery",
                "blockingType": "NONE",
                "actionParameters": []
            }
        ]
    }
    payload = json.dumps(msg)
    client.publish(TOPIC, payload, qos=1)
    print(f"\n[SENT] tw_elevator_discovery -> {TOPIC}")

def send_status(client, elevator_id):
    msg = {
        "headerId": int(time.time() * 1000),
        "timestamp": time.strftime("%Y-%m-%dT%H:%M:%S.000Z", time.gmtime()),
        "version": "2.0.0",
        "manufacturer": MFR,
        "serialNumber": SN,
        "instantActions": [
            {
                "actionId": f"status_{int(time.time())}",
                "actionType": "tw_elevator_status",
                "blockingType": "NONE",
                "actionParameters": [
                    {"key": "elevatorId", "value": elevator_id}
                ]
            }
        ]
    }
    payload = json.dumps(msg)
    client.publish(TOPIC, payload, qos=1)
    print(f"\n[SENT] tw_elevator_status (elevatorId={elevator_id}) -> {TOPIC}")

def main():
    client = mqtt.Client(
        client_id=f"test-elevator-{int(time.time())}",
        transport="websockets"
    )
    client.username_pw_set(USER, PASS)
    client.ws_set_options(path=WS_PATH)
    client.tls_set(cert_reqs=ssl.CERT_NONE)
    client.tls_insecure_set(True)

    client.on_connect = on_connect
    client.on_message = on_message

    print(f"Connecting to wss://{BROKER}:{PORT}{WS_PATH} ...")
    client.connect(BROKER, PORT)
    client.loop_start()

    time.sleep(2)

    # 1. Send discovery
    print("\n" + "="*50)
    print("TEST 1: tw_elevator_discovery")
    print("="*50)
    send_discovery(client)

    # Wait for response
    print("Waiting 10s for response...")
    time.sleep(10)

    if received:
        print(f"\n[RESULT] Got {len(received)} response(s)")
    else:
        print("\n[RESULT] No response from IoT Gateway (may not be connected to this broker)")

    # If we got a discovery response with elevator IDs, try status
    elevator_ids = []
    for r in received:
        for s in r.get("actionStates", []):
            desc = s.get("resultDescription", "")
            try:
                data = json.loads(desc)
                for lobby in data.get("lobbies", []):
                    for elev in lobby.get("elevators", []):
                        elevator_ids.append(elev["id"])
                        print(f"  Found elevator: {elev['name']} (id={elev['id']}, status={elev.get('status')})")
            except:
                pass

    if elevator_ids:
        print("\n" + "="*50)
        print(f"TEST 2: tw_elevator_status (first elevator: {elevator_ids[0]})")
        print("="*50)
        received.clear()
        send_status(client, elevator_ids[0])
        print("Waiting 10s for response...")
        time.sleep(10)
        if received:
            print(f"\n[RESULT] Got {len(received)} response(s)")
        else:
            print("\n[RESULT] No response")
    else:
        print("\n[SKIP] No elevator IDs found, skipping status test")

    client.loop_stop()
    client.disconnect()
    print("\nDone.")

if __name__ == "__main__":
    main()
