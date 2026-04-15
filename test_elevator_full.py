#!/usr/bin/env python3
"""
Test tw_elevator_discovery + tw_elevator_status via MQTT.
Uses standard VDA5050 "instantActions" field name.
"""
import json, ssl, time
import paho.mqtt.client as mqtt

BROKER = "nexmqtt.jini.tw"
PORT = 443
WS_PATH = "/mqtt"
USER = "bibi"
PASS = "70595145"
PREFIX = "/uagv/v2"
MFR = "Atom"
SN = "chunjiao"

TOPIC_IA = f"{PREFIX}/{MFR}/{SN}/instantActions"
all_msgs = []

def on_connect(client, userdata, flags, rc):
    if rc == 0:
        print(f"[OK] Connected to {BROKER}", flush=True)
        client.subscribe(f"{PREFIX}/{MFR}/{SN}/#", 1)
        print(f"[OK] Subscribed to {MFR}/{SN}/#", flush=True)
    else:
        print(f"[ERR] Connect failed rc={rc}", flush=True)

def on_message(client, userdata, msg):
    try:
        data = json.loads(msg.payload)
        topic = msg.topic
        if any(topic.endswith(s) for s in ["/state", "/visualization", "/connection", "/factsheet", "/waypoints"]):
            return
        ts = time.strftime("%H:%M:%S")
        if "actionStates" in data:
            for s in data.get("actionStates", []):
                at = s.get("actionType", "")
                status = s.get("actionStatus", "")
                desc = s.get("resultDescription", "")[:500]
                print(f"[{ts}] RESPONSE: {at} status={status}", flush=True)
                if desc:
                    print(f"         desc={desc}", flush=True)
                all_msgs.append({"type": "response", "data": s})
        elif "instantActions" in data:
            for a in data.get("instantActions", []):
                atype = a.get("actionType", "")
                aid = a.get("actionId", "")
                print(f"[{ts}] ECHO: {atype} id={aid}", flush=True)
                all_msgs.append({"type": "action", "data": a})
    except Exception as e:
        print(f"[ERR] parse: {e}", flush=True)

client = mqtt.Client(client_id=f"test-elevator-mac-{int(time.time())}", transport="websockets")
client.username_pw_set(USER, PASS)
client.ws_set_options(path=WS_PATH)
client.tls_set(cert_reqs=ssl.CERT_NONE)
client.tls_insecure_set(True)
client.on_connect = on_connect
client.on_message = on_message

print(f"Connecting to wss://{BROKER}:{PORT}{WS_PATH} ...", flush=True)
client.connect(BROKER, PORT)
client.loop_start()
time.sleep(3)

# TEST: tw_elevator_discovery using standard "instantActions" field
print("\n" + "="*60, flush=True)
print("TEST: tw_elevator_discovery (standard VDA5050 field)", flush=True)
print("="*60, flush=True)
msg1 = {
    "headerId": int(time.time()*1000),
    "timestamp": time.strftime("%Y-%m-%dT%H:%M:%S.000Z", time.gmtime()),
    "version": "2.0.0",
    "manufacturer": MFR,
    "serialNumber": SN,
    "instantActions": [{
        "actionId": f"disc_{int(time.time())}",
        "actionType": "tw_elevator_discovery",
        "blockingType": "NONE",
        "actionParameters": []
    }]
}
client.publish(TOPIC_IA, json.dumps(msg1), qos=1)
print(f"[SENT] tw_elevator_discovery -> {TOPIC_IA}", flush=True)
print("Waiting 10s...", flush=True)
time.sleep(10)

responses = [m for m in all_msgs if m["type"] == "response"]
if responses:
    print(f"\nSUCCESS! Got {len(responses)} response(s):", flush=True)
    for r in responses:
        print(json.dumps(r["data"], indent=2, ensure_ascii=False), flush=True)
else:
    print("\nFAILED — no response (IoT Gateway not yet redeployed?)", flush=True)

client.loop_stop()
client.disconnect()
print("\nDone.", flush=True)
