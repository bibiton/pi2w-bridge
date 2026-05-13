# pi2w-bridge

A Go service that bridges a VDA5050 fleet-management platform (over MQTT/WebSocket) and N ATOM delivery robots (over each robot's public HTTP/WebSocket API). One process manages all robots; each robot gets its own `RobotSession` with its own MQTT client and WebSocket connection. State is persisted to SQLite.

## Architecture

```
VDA5050 platform  <--MQTT(wss)-->  pi2w-bridge (cloud VM)  <--HTTP/WS-->  robot A (public ip:8080 ATOM API, ip:8000 FastAPI)
                                        |                                  robot B ...
                                        |                                  robot N ...
                                   SQLite (/data)
                                   robots.yaml (hot-reloaded)
                                   HTTP API :5201 (/webhook/{robotKey}, /healthz, /admin/robots)
```

**Key source files:**

| File | Responsibility |
|---|---|
| `main.go` | Service startup |
| `serverconfig.go` | `ServerConfig` + `RobotRecord` types |
| `store.go` | SQLite: `robots` / `orders` / `action_states` tables |
| `session.go` | Per-robot `RobotSession` (MQTT client + WS connection) |
| `manager.go` | `SessionManager` + crash reaper |
| `robots_yaml.go` | `robots.yaml` fsnotify hot-reload |
| `httpapi.go` | Webhook + admin HTTP API |
| `mqtt_bridge.go`, `order_handler.go`, `instant_actions.go`, `robot_ws.go`, `state_*.go`, … | VDA5050 / ATOM protocol logic |

## Configuration

All settings have working defaults so the service starts out of the box. Override them in production via environment variables or a `.env` file (loaded by [godotenv](https://github.com/joho/godotenv)).

| Env var | Default | Meaning |
|---|---|---|
| `LISTEN_ADDR` | `:5201` | HTTP server bind address (webhook + admin) |
| `PUBLIC_BASE_URL` | `http://127.0.0.1:5201` | Base URL robots use to reach our webhook; sessions register `<PUBLIC_BASE_URL>/webhook/<robotID>` with each robot |
| `DB_PATH` | `pi2w-bridge.db` | SQLite file path (relative to the process's working directory) |
| `ROBOTS_YAML` | `robots.yaml` | Path to the robots declaration file (hot-reloaded) |
| `MQTT_BROKER` | `wss://nexmqtt.jini.tw:443/mqtt` | VDA5050 MQTT broker |
| `MQTT_USER` | `bibi` | MQTT username |
| `MQTT_PASS` | `70595145` | MQTT password |
| `MQTT_PREFIX` | `/uagv/v2` | VDA5050 topic prefix |
| `VDA_MANUFACTURER` | `Atom` | Default VDA5050 `manufacturer` when a robot record omits it |
| `ROBOT_PORT` | `8080` | Default port for a robot's HTTP/WS API when `atomBaseURL` omits one |
| `TTS_URL` | *(empty)* | atomros2-tts service URL; empty disables `playVoice` |
| `ADMIN_TOKEN` | `pi2w-admin-changeme` | Bearer token for `/admin/*` — **change in prod** |
| `DEFAULT_ROBOT_SECRET` | `pi2w-webhook-changeme` | `X-Webhook-Secret` used when a robot record omits one — **change in prod** |

## robots.yaml

Declare every robot the bridge should manage. Copy the committed example and edit:

```bash
cp robots.example.yaml robots.yaml   # robots.yaml is gitignored
```

```yaml
robots:
  - id: adai01            # also the {robotKey} in the webhook path; defaults serial to this if omitted
    manufacturer: Atom    # optional, falls back to VDA_MANUFACTURER
    serial: adai01        # optional, falls back to id
    atomBaseURL: http://1.2.3.4:8080      # the robot's public ATOM API (port falls back to ROBOT_PORT)
    fastapiHTTPURL: http://1.2.3.4:8080   # optional, defaults to the same host:port as atomBaseURL
    fastapiWSURL: ws://1.2.3.4:8080/ws    # optional; "disabled" (or none/off/-) turns the WS client off
    webhookSecret: change-me-per-robot    # optional, falls back to DEFAULT_ROBOT_SECRET
```

`fastapiWSURL` is the robot's FastAPI WebSocket — used only for the `initPosition` instant action (pushing an initial pose to relocalize) and a redundant `tracked_pose` stream. If the robot doesn't expose it, set `fastapiWSURL: disabled` so the bridge doesn't loop on a doomed reconnect; `initPosition` then returns an error and everything else (orders, telemetry, maps, voice) is unaffected.

Save the file — changes apply immediately (fsnotify watches it). Removing a robot from the file deregisters it.

## Robot Registration

A robot is managed only if it's declared — there is no auto-provisioning. Two ways to declare one (both funnel to the same session-start logic):

1. **`robots.yaml`** — declared robots are registered; the file is hot-reloaded at runtime. (On startup, every non-deleted robot already in the SQLite DB — i.e. previously declared — is also loaded.)
2. **`POST /admin/robots`** — register/update a robot at runtime via the admin API.

A `POST /webhook/{robotKey}` for a `robotKey` that isn't a managed robot is dropped (`404`). Sessions register `<PUBLIC_BASE_URL>/webhook/<robotID>/` (note the trailing slash) with each robot; ATOM robots concatenate a data-source name onto that URL for the odometry streams (e.g. `…/webhook/<robotID>/imu`, `…/webhook/<robotID>/encoder`), so the bridge uses only the **first path segment** after `/webhook/` as the robot key. (A robot still configured with an old slash-less URL will briefly POST to `…/webhook/<robotID>imu` and get `404`s until it re-reads the registration — the bridge re-registers every 60 s.)

There is no UI. Onboarding a robot means adding a block to `robots.yaml` or calling `POST /admin/robots`.

## HTTP API

| Method + Path | Auth | Purpose |
|---|---|---|
| `POST /webhook/{robotKey}[/{source}]` | `X-Webhook-Secret: <secret>` header — a missing header is accepted, a *wrong* one is rejected (ATOM robots don't always send it) | Robot pushes its status/pose updates here; only the first path segment is the `{robotKey}` |
| `GET /healthz` | none | `{"status":"ok","robots":[{"id","lastSeen","dataFresh"}]}` |
| `GET /admin/robots` | `Authorization: Bearer <ADMIN_TOKEN>` | List all robots (with live `online` status) |
| `POST /admin/robots` | `Authorization: Bearer <ADMIN_TOKEN>` | Register/update a robot (body = robot record JSON) |
| `GET /admin/robots/{id}` | `Authorization: Bearer <ADMIN_TOKEN>` | One robot's stored record |
| `DELETE /admin/robots/{id}` | `Authorization: Bearer <ADMIN_TOKEN>` | Deregister + mark `deleted` in DB |

**`curl` examples:**

```bash
# Health check
curl -s localhost:5201/healthz

# List robots
curl -s -H 'Authorization: Bearer change-me' localhost:5201/admin/robots

# Register / update a robot
curl -s -XPOST -H 'Authorization: Bearer change-me' \
  -d '{"id":"adai01","atomBaseURL":"http://1.2.3.4:8080","fastapiWSURL":"ws://1.2.3.4:8000/ws","webhookSecret":"abc"}' \
  localhost:5201/admin/robots

# Deregister a robot
curl -s -XDELETE -H 'Authorization: Bearer change-me' localhost:5201/admin/robots/adai01
```

## Deployment

```bash
cp robots.example.yaml robots.yaml
# Edit robots.yaml with your robot entries.
# In docker-compose.yml, set PUBLIC_BASE_URL, ADMIN_TOKEN, DEFAULT_ROBOT_SECRET,
# and MQTT_* variables as needed.
docker compose up -d --build
```

- SQLite persists in the `bridge-data` Docker volume.
- `robots.yaml` is bind-mounted read-only at `/config/robots.yaml` inside the container.
- For TLS, place a reverse proxy (Caddy, nginx) in front and terminate TLS there. `/webhook/*` is a public endpoint and should be HTTPS in production.

## Local Development

```bash
DB_PATH=/tmp/s.db ADMIN_TOKEN=t go run .
```

## What Changed vs the Pi Version

This service previously ran on a Raspberry Pi Zero 2 W, one Pi per robot, on the same LAN as the robot.

The following Pi-host-specific components were **removed** in the cloud migration:

- **USB-gadget network watchdogs** (`usb_watchdog.go`) — ARP-probe, wrong-peer detection, packet-level hot-plug.
- **Network-state snapshot logger** (`net_snapshot.go`) — persistent `/var/log/pi2w-snapshots.log` writer.
- **Wi-Fi configuration helper** (`wifi_service.go`).
- **Cloudflare tunnel URL watcher** (`tunnel_url.go`).

**Elevator / cross-floor navigation** (`elevator_service.go`, `elevator_config.go`, elevator flow in `order_handler.go`) was also removed. Cross-map orders now fail immediately with `errorRef: cross_map_not_supported` instead of orchestrating an elevator sequence. This feature may be re-added later with a robot-side mechanism.

## Security

The default values for `ADMIN_TOKEN`, `DEFAULT_ROBOT_SECRET`, and the MQTT credentials allow the service to start without any configuration. **You must change `ADMIN_TOKEN` and `DEFAULT_ROBOT_SECRET` (and ideally the MQTT credentials) before deploying to any publicly reachable host.**

`/webhook/*` is intentionally public (robots POST to it). Front it with TLS. The per-robot `X-Webhook-Secret` is enforced *only when the robot actually sends it* (ATOM firmware doesn't attach custom headers to its webhook POSTs, so the bridge fails open on a missing header and the `{robotKey}` path segment is the effective gate) — treat the webhook endpoint as low-trust and don't expose `/admin/*` publicly.
