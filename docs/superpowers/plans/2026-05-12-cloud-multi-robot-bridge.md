# Cloud Multi-Robot Bridge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn `pi2w-bridge` from a 1-Pi-per-1-robot LAN bridge into a single cloud service that manages N robots over their public IPs, with dynamic registration, SQLite-backed state, and an admin/webhook HTTP API.

**Architecture:** Keep single `package main`. The existing per-instance types (`RobotState`, `MQTTBridge`, `RobotWSClient`, `MapService`, `OrderHandler`) are wrapped by a new `RobotSession`; a `SessionManager` owns `map[robotID]*RobotSession` with crash isolation. Robots enter via DB load / `robots.yaml` hot-reload / first webhook (auto-provisional). Pi-host code (USB watchdog, net snapshot, wifi, tunnel) and elevator / cross-floor code are deleted.

**Tech Stack:** Go 1.21, `eclipse/paho.mqtt.golang`, `gorilla/websocket`, `modernc.org/sqlite` (pure-Go, no CGO), `fsnotify/fsnotify`, `gopkg.in/yaml.v3`, `joho/godotenv`. Docker + docker-compose.

**Spec:** `docs/superpowers/specs/2026-05-12-cloud-multi-robot-bridge-design.md`

---

## File Structure (target end state)

**New files:**
- `serverconfig.go` — global `ServerConfig` (DB DSN, listen addr, MQTT broker creds, admin token, TTS URL, public webhook base URL); `RobotRecord` struct; `LoadServerConfig()`; `LoadConfigForRobot(rec, srv) *Config`.
- `store.go` — `Store` (wraps `*sql.DB`); embedded migrations; `robots` / `orders` / `action_states` CRUD.
- `session.go` — `RobotSession` (`Start()`/`Stop()`), prefixed logger, goroutine `recover` helper.
- `manager.go` — `SessionManager` (`Register`/`Deregister`/`Get`/`List`), 1-min reaper.
- `robots_yaml.go` — load `robots.yaml`, `fsnotify` watch, diff → Register/Deregister.
- `httpapi.go` — replaces `webhook.go`: `POST /webhook/{robotKey}`, `GET /healthz`, `/admin/robots` CRUD.
- `Dockerfile`, `docker-compose.yml`, `robots.example.yaml`.

**Deleted files:**
- Pi-host: `usb_watchdog.go`, `net_snapshot.go`, `wifi_service.go`, `tunnel_url.go`.
- Elevator: `elevator_service.go`, `elevator_config.go`, `elevator_config.json`, `elevator_config.json.example`, `docs/elevator-floor-change.md`, `test_elevator_full.py`, `test_elevator_mqtt.py`.
- `webhook.go` (folded into `httpapi.go`).

**Modified files:**
- `main.go` — new startup flow.
- `config.go` — `Config` keeps only per-robot fields; loaders move to `serverconfig.go`.
- `mqtt_bridge.go` — drop `elevatorCfg` / `elevatorService` fields & uses; accept a `*log.Logger`.
- `order_handler.go` — drop `elevatorCfg` param; replace cross-floor branches with `failOrder(...,"cross_map_not_supported")`; delete `handleFloorChange`/`doSwitchMap`/`setPoseAtStation` if unused after that.
- `register.go` — `RegisterWebhook` points the robot at `https://<cloud>/webhook/<robotKey>`.
- `robot_state.go`, `robot_ws.go`, `map_service.go`, `instant_actions.go`, `state_*.go` — accept/propagate per-session logger (minimal: only where logs need robot prefix).
- `go.mod` — add `modernc.org/sqlite`, `github.com/fsnotify/fsnotify`.

---

## Phase 0 — Branch & baseline

### Task 0: Create working branch, confirm build

**Files:** none

- [ ] **Step 1: Branch off main**

```bash
git checkout -b feat/cloud-multi-robot
```

- [ ] **Step 2: Confirm current build is green**

Run: `go build ./...`
Expected: builds with no error.

- [ ] **Step 3: Confirm test baseline**

Run: `go test ./... 2>&1 | tail -5`
Expected: `no test files` (there are currently none) — that's fine, we add them.

---

## Phase 1 — Clean out Pi-host and elevator code

### Task 1: Delete Pi-host-only files

**Files:**
- Delete: `usb_watchdog.go`, `net_snapshot.go`, `wifi_service.go`, `tunnel_url.go`
- Modify: `main.go`

- [ ] **Step 1: Delete the four files**

```bash
git rm usb_watchdog.go net_snapshot.go wifi_service.go tunnel_url.go
```

- [ ] **Step 2: Remove their callers from `main.go`**

In `main.go`, delete these lines and their surrounding comments:
- `StartTunnelURLWatcher(mqttBridge)`
- `StartUSBWatchdog(cfg.RobotIP)`
- `StartUSBLinkWatchdog(cfg.RobotIP)`
- `StartNetworkSnapshotLogger()`

Also check `register.go` / `wifi_service.go` callers — `wifi_service.go`'s exported funcs (if referenced anywhere) must be removed there too. Run `grep -rn 'WiFi\|Wifi\|wifi' *.go` and remove dangling references.

- [ ] **Step 3: Build**

Run: `go build ./...`
Expected: builds (fix any remaining undefined-symbol errors by removing the dead reference).

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "chore: remove Pi-host-only code (usb watchdog, net snapshot, wifi, tunnel)"
```

### Task 2: Delete elevator code and rewire `order_handler` cross-floor handling

**Files:**
- Delete: `elevator_service.go`, `elevator_config.go`, `elevator_config.json`, `elevator_config.json.example`, `docs/elevator-floor-change.md`, `test_elevator_full.py`, `test_elevator_mqtt.py`
- Modify: `order_handler.go`, `mqtt_bridge.go`, `main.go`

- [ ] **Step 1: Delete elevator files**

```bash
git rm elevator_service.go elevator_config.go elevator_config.json elevator_config.json.example docs/elevator-floor-change.md test_elevator_full.py test_elevator_mqtt.py
```

- [ ] **Step 2: `mqtt_bridge.go` — remove elevator fields & uses**

- Remove field `elevatorCfg *ElevatorConfig` and `elevatorService *ElevatorService` from the `MQTTBridge` struct.
- In `NewMQTTBridge`, remove the `elevatorCfg` parameter and the `elevatorCfg: elevatorCfg` initialiser. New signature:
  `func NewMQTTBridge(cfg *Config, state *RobotState, mapService *MapService, robotWS *RobotWSClient) *MQTTBridge`
- In the body where `NewOrderHandler` is called, drop the `mb.elevatorCfg` argument (see Step 3).
- In `handleInstantActions` (~line 266), delete the `if mb.elevatorService != nil { mb.elevatorService.HandleActionStates(states) }` block. If `states` becomes unused, remove the surrounding parsing too (verify by reading the function).

- [ ] **Step 3: `order_handler.go` — remove elevator dependency, fail cross-map orders**

- `OrderHandler` struct: remove field `elevatorCfg *ElevatorConfig`.
- `NewOrderHandler`: remove the `elevatorCfg *ElevatorConfig` parameter and `elevatorCfg: elevatorCfg` init. New signature:
  `func NewOrderHandler(cfg *Config, state *RobotState, bridge *MQTTBridge, robotWS *RobotWSClient) *OrderHandler`
- In `executeOrder()` around line 227, the branch:
  ```go
  if nextMapID != "" && oh.elevatorCfg != nil && oh.elevatorCfg.NeedsFloorChange(currentMapID, nextMapID) {
      ... oh.handleFloorChange(...) ...
  }
  ```
  Replace the whole branch with:
  ```go
  if nextMapID != "" && nextMapID != currentMapID {
      log.Printf("[Order] cross-map order not supported (%s -> %s); failing order", currentMapID, nextMapID)
      oh.failOrder(order.OrderID, "cross_map_not_supported")
      return
  }
  ```
- Around line 313, the "return trip: take elevator back" branch — delete it entirely (no replacement; we just don't do return-trip floor changes).
- Delete the now-unused methods: `handleFloorChange` (~line 901), and `doSwitchMap` / `setPoseAtStation` **only if** `grep -n 'doSwitchMap\|setPoseAtStation' order_handler.go` shows no remaining callers. If `handleSwitchMap` in `instant_actions.go` calls a different (its own) implementation, leave that alone — it's separate.
- Also remove `currentMapID` / `originMapID` / `nextMapID` bookkeeping that becomes dead after the above (read the function; keep what's still used).

- [ ] **Step 4: `main.go` — remove elevator wiring**

Delete:
- `elevatorCfg, err := LoadElevatorConfig("elevator_config.json")` + its error check
- the `elevatorCfg` argument to `NewMQTTBridge`
- the whole `// 10b. Start elevator service` block (`NewElevatorService`, `mqttBridge.elevatorService = ...`, `elevatorSvc.Start()`, the log line)
- `elevatorSvc.Stop()` in the shutdown section

- [ ] **Step 5: Build**

Run: `go build ./...`
Expected: builds. Fix any leftover references (`grep -rn 'lyevator\|Elevator\|elevator' *.go` should return nothing).

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -m "chore: remove elevator/cross-floor support; cross-map orders now fail with errorRef"
```

---

## Phase 2 — ServerConfig + RobotRecord

### Task 3: Add `serverconfig.go`

**Files:**
- Create: `serverconfig.go`
- Test: `serverconfig_test.go`
- Modify: `config.go`

- [ ] **Step 1: Write the failing test**

`serverconfig_test.go`:
```go
package main

import (
	"os"
	"testing"
)

func TestLoadServerConfig_Defaults(t *testing.T) {
	os.Clearenv()
	c := LoadServerConfig()
	if c.ListenAddr != ":5201" {
		t.Errorf("ListenAddr = %q, want :5201", c.ListenAddr)
	}
	if c.MQTTBroker == "" || c.MQTTUser == "" || c.MQTTPass == "" {
		t.Errorf("MQTT defaults must be non-empty: %+v", c)
	}
	if c.AdminToken == "" {
		t.Errorf("AdminToken default must be non-empty")
	}
	if c.DBPath == "" {
		t.Errorf("DBPath default must be non-empty")
	}
}

func TestLoadServerConfig_EnvOverride(t *testing.T) {
	os.Clearenv()
	os.Setenv("MQTT_BROKER", "wss://example/mqtt")
	os.Setenv("ADMIN_TOKEN", "tok123")
	c := LoadServerConfig()
	if c.MQTTBroker != "wss://example/mqtt" {
		t.Errorf("MQTTBroker not overridden: %q", c.MQTTBroker)
	}
	if c.AdminToken != "tok123" {
		t.Errorf("AdminToken not overridden: %q", c.AdminToken)
	}
}

func TestLoadConfigForRobot(t *testing.T) {
	os.Clearenv()
	srv := LoadServerConfig()
	rec := RobotRecord{
		ID: "adai01", Manufacturer: "atom", Serial: "adai01",
		AtomBaseURL: "http://1.2.3.4:8080", FastAPIHTTPURL: "http://1.2.3.4:8000",
		FastAPIWSURL: "ws://1.2.3.4:8000/ws", WebhookSecret: "s3cr3t",
	}
	cfg := LoadConfigForRobot(rec, srv)
	if cfg.RobotIP != "1.2.3.4" || cfg.RobotPort != "8080" {
		t.Errorf("RobotIP/Port wrong: %q %q", cfg.RobotIP, cfg.RobotPort)
	}
	if cfg.RobotFastAPI != "http://1.2.3.4:8000" {
		t.Errorf("RobotFastAPI wrong: %q", cfg.RobotFastAPI)
	}
	if cfg.Manufacturer != "atom" || cfg.SerialNumber != "adai01" {
		t.Errorf("identity wrong: %+v", cfg)
	}
	if cfg.MQTTBroker != srv.MQTTBroker {
		t.Errorf("MQTT broker should come from server config")
	}
}
```

- [ ] **Step 2: Run test — expect compile failure**

Run: `go test -run TestLoadServerConfig -v`
Expected: FAIL — `undefined: LoadServerConfig`, `undefined: RobotRecord`.

- [ ] **Step 3: Create `serverconfig.go`**

```go
package main

import (
	"net/url"
	"strings"

	"github.com/joho/godotenv"
)

// ServerConfig holds process-wide settings shared by all robot sessions.
type ServerConfig struct {
	ListenAddr      string // webhook + admin HTTP server, e.g. ":5201"
	PublicBaseURL   string // how robots reach our webhook, e.g. "https://bridge.example.com"
	DBPath          string // SQLite path or full DSN (postgres://... also accepted by store.go)

	MQTTBroker string
	MQTTUser   string
	MQTTPass   string
	MQTTPrefix string

	Manufacturer string // default VDA manufacturer when a RobotRecord omits it
	TTSURL       string // default atomros2-tts URL; empty disables voice

	AdminToken         string // bearer token for /admin/*
	DefaultRobotSecret string // X-Webhook-Secret used when a RobotRecord omits one
}

func LoadServerConfig() *ServerConfig {
	_ = godotenv.Load()
	return &ServerConfig{
		ListenAddr:         envOrDefault("LISTEN_ADDR", ":5201"),
		PublicBaseURL:      envOrDefault("PUBLIC_BASE_URL", "http://127.0.0.1:5201"),
		DBPath:             envOrDefault("DB_PATH", "/data/pi2w-bridge.db"),
		MQTTBroker:         envOrDefault("MQTT_BROKER", "wss://nexmqtt.jini.tw:443/mqtt"),
		MQTTUser:           envOrDefault("MQTT_USER", "bibi"),
		MQTTPass:           envOrDefault("MQTT_PASS", "70595145"),
		MQTTPrefix:         envOrDefault("MQTT_PREFIX", "/uagv/v2"),
		Manufacturer:       envOrDefault("VDA_MANUFACTURER", "atom"),
		TTSURL:             envOrDefault("TTS_URL", ""),
		AdminToken:         envOrDefault("ADMIN_TOKEN", "pi2w-admin-changeme"),
		DefaultRobotSecret: envOrDefault("DEFAULT_ROBOT_SECRET", "pi2w-webhook-changeme"),
	}
}

// RobotRecord is the persisted/declared description of one robot.
type RobotRecord struct {
	ID             string `json:"id" yaml:"id"`
	Manufacturer   string `json:"manufacturer" yaml:"manufacturer"`
	Serial         string `json:"serial" yaml:"serial"`
	AtomBaseURL    string `json:"atomBaseURL" yaml:"atomBaseURL"`       // http://ip:8080
	FastAPIHTTPURL string `json:"fastapiHTTPURL" yaml:"fastapiHTTPURL"` // http://ip:8000
	FastAPIWSURL   string `json:"fastapiWSURL" yaml:"fastapiWSURL"`     // ws://ip:8000/ws
	WebhookSecret  string `json:"webhookSecret" yaml:"webhookSecret"`
	Status         string `json:"status" yaml:"-"`  // online|offline|errored|provisional|deleted
	Source         string `json:"source" yaml:"-"`  // db|yaml|provisional
	LastSeenAt     int64  `json:"lastSeenAt" yaml:"-"`
}

// hostPort extracts host and port from a URL like http://1.2.3.4:8080.
func hostPort(rawURL, defPort string) (host, port string) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL, defPort
	}
	h := u.Hostname()
	p := u.Port()
	if p == "" {
		p = defPort
	}
	return h, p
}

// LoadConfigForRobot builds the per-session *Config from a RobotRecord + server defaults.
func LoadConfigForRobot(rec RobotRecord, srv *ServerConfig) *Config {
	mfr := rec.Manufacturer
	if mfr == "" {
		mfr = srv.Manufacturer
	}
	serial := rec.Serial
	if serial == "" {
		serial = rec.ID
	}
	ip, port := hostPort(rec.AtomBaseURL, "8080")
	fastapi := rec.FastAPIHTTPURL
	if fastapi == "" && ip != "" {
		fastapi = "http://" + ip + ":8000"
	}
	secret := rec.WebhookSecret
	if secret == "" {
		secret = srv.DefaultRobotSecret
	}
	return &Config{
		RobotIP:       ip,
		RobotPort:     port,
		RobotFastAPI:  strings.TrimRight(fastapi, "/"),
		RobotFastAPIWS: rec.FastAPIWSURL,
		WebhookSecret: secret,
		MQTTBroker:    srv.MQTTBroker,
		MQTTUser:      srv.MQTTUser,
		MQTTPass:      srv.MQTTPass,
		MQTTPrefix:    srv.MQTTPrefix,
		Manufacturer:  mfr,
		SerialNumber:  serial,
		TTSURL:        srv.TTSURL,
	}
}
```

- [ ] **Step 4: Update `config.go`**

Trim `Config` down to per-robot fields and remove the global loaders (moved to `serverconfig.go`). New `config.go`:
```go
package main

import "fmt"

// Config is the per-robot-session configuration.
type Config struct {
	// Robot connection
	RobotIP        string
	RobotPort      string // ATOM API port (8080)
	RobotFastAPI   string // http://ip:8000
	RobotFastAPIWS string // ws://ip:8000/ws  (if empty, RobotWSClient derives it)

	WebhookSecret string // X-Webhook-Secret this robot must present

	// MQTT (copied from ServerConfig)
	MQTTBroker string
	MQTTUser   string
	MQTTPass   string
	MQTTPrefix string

	// VDA5050 identity
	Manufacturer string
	SerialNumber string

	// atomros2-tts URL; empty disables voice
	TTSURL string
}

func (c *Config) RobotBaseURL() string {
	return fmt.Sprintf("http://%s:%s", c.RobotIP, c.RobotPort)
}

func (c *Config) TopicPrefix() string {
	return fmt.Sprintf("%s/%s/%s", c.MQTTPrefix, c.Manufacturer, c.SerialNumber)
}

func envOrDefault(key, fallback string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return fallback
}
```
Wait — `envOrDefault` already uses `os.Getenv`. Keep it referencing `os.Getenv` directly; don't introduce `getenv`. So actually leave `envOrDefault` in `config.go` exactly as it was (imports `os`). Remove `LoadConfig`, `loadMQTTConfigFile`, the `godotenv`/`encoding/json`/`os` imports that are no longer needed except `os` (kept for `envOrDefault`). Final imports: `"fmt"`, `"os"`.

Also: `RobotWSClient` currently derives its WS URL from `cfg.RobotFastAPI` — check `robot_ws.go`. If it builds `ws://host:8000/ws` itself, then `RobotFastAPIWS` is optional; if `RobotFastAPIWS` is non-empty, make `robot_ws.go` prefer it. Add that small change in `robot_ws.go`'s `connect()`.

- [ ] **Step 5: Delete `mqtt_config.go` / `mqtt_config.json` if now unused**

Run `grep -rn 'MQTTConfig\|mqtt_config' *.go`. If only `config.go` referenced it (now removed), `git rm mqtt_config.go`. Leave `mqtt_config.json` deletion to the implementer's discretion (it's gitignored config; check `.gitignore`).

- [ ] **Step 6: Run tests**

Run: `go build ./... && go test -run 'TestLoadServerConfig|TestLoadConfigForRobot' -v`
Expected: PASS. (`main.go` won't compile yet because it still calls `LoadConfig()` — that's fixed in Phase 7. For now, temporarily comment out the body of `main()` or skip building `main.go`'s logic; simplest: do this task together with a stub `main()` that just `panic("rewired in phase 7")` — but prefer to keep `go build` green by leaving `main.go` until Phase 7 and using `go test` which still compiles the package. If the package won't compile due to `main.go`, apply the Phase 7 `main.go` rewrite now instead — it's a hard dependency. **Recommendation: reorder — do Task 11 (main.go) right after Task 10, and until then keep a minimal `main.go` that compiles.**)

To keep things compiling through Phases 2–6, replace `main.go`'s `main()` body now with:
```go
func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.Fatal("main not wired yet — see Phase 7")
}
```
and keep `statusLogger` (still used by sessions later). Commit that as part of this task.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "feat: add ServerConfig + RobotRecord; slim Config to per-robot fields"
```

---

## Phase 3 — Persistence layer

### Task 4: Add `store.go` with migrations and CRUD

**Files:**
- Create: `store.go`
- Test: `store_test.go`
- Modify: `go.mod` (add `modernc.org/sqlite`)

- [ ] **Step 1: Add the dependency**

```bash
go get modernc.org/sqlite@latest
```

- [ ] **Step 2: Write the failing test**

`store_test.go`:
```go
package main

import (
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStore_RobotRoundTrip(t *testing.T) {
	s := newTestStore(t)
	rec := RobotRecord{ID: "adai01", Manufacturer: "atom", Serial: "adai01",
		AtomBaseURL: "http://1.2.3.4:8080", FastAPIHTTPURL: "http://1.2.3.4:8000",
		FastAPIWSURL: "ws://1.2.3.4:8000/ws", WebhookSecret: "s", Status: "online", Source: "yaml"}
	if err := s.UpsertRobot(rec); err != nil {
		t.Fatalf("UpsertRobot: %v", err)
	}
	got, err := s.GetRobot("adai01")
	if err != nil {
		t.Fatalf("GetRobot: %v", err)
	}
	if got.AtomBaseURL != rec.AtomBaseURL || got.Status != "online" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}

	if err := s.TouchRobot("adai01", "online", time.Now()); err != nil {
		t.Fatalf("TouchRobot: %v", err)
	}
	list, err := s.ListRobots()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListRobots: %v len=%d", err, len(list))
	}

	if err := s.SetRobotStatus("adai01", "deleted"); err != nil {
		t.Fatalf("SetRobotStatus: %v", err)
	}
	list, _ = s.ListActiveRobots()
	if len(list) != 0 {
		t.Errorf("deleted robot still active: %+v", list)
	}
}

func TestStore_OrderLifecycle(t *testing.T) {
	s := newTestStore(t)
	_ = s.UpsertRobot(RobotRecord{ID: "r1", Status: "online", Source: "yaml"})
	if err := s.InsertOrder("ord1", "r1", 0, []byte(`{"orderId":"ord1"}`)); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	if err := s.UpdateOrderNode("ord1", "n2"); err != nil {
		t.Fatalf("UpdateOrderNode: %v", err)
	}
	if err := s.FinishOrder("ord1", "finished", ""); err != nil {
		t.Fatalf("FinishOrder: %v", err)
	}
	if err := s.UpsertActionState("ord1", "a1", "playVoice", "FINISHED", "ok"); err != nil {
		t.Fatalf("UpsertActionState: %v", err)
	}

	// running-order recovery: insert a fresh running order, then fail-all-running
	_ = s.InsertOrder("ord2", "r1", 0, []byte(`{}`))
	n, err := s.FailRunningOrders("bridge_restarted")
	if err != nil || n != 1 {
		t.Fatalf("FailRunningOrders n=%d err=%v", n, err)
	}
}
```

- [ ] **Step 3: Run test — expect compile failure**

Run: `go test -run TestStore -v`
Expected: FAIL — `undefined: OpenStore` etc.

- [ ] **Step 4: Create `store.go`**

```go
package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS robots (
  id TEXT PRIMARY KEY,
  manufacturer TEXT, serial TEXT,
  atom_base_url TEXT, fastapi_http_url TEXT, fastapi_ws_url TEXT,
  webhook_secret TEXT, status TEXT, source TEXT,
  last_seen_at INTEGER, created_at INTEGER, updated_at INTEGER
);
CREATE TABLE IF NOT EXISTS orders (
  order_id TEXT PRIMARY KEY,
  robot_id TEXT,
  order_update_id INTEGER,
  status TEXT,           -- running|finished|failed|cancelled
  raw_order TEXT,
  last_node_id TEXT,
  error_ref TEXT,
  created_at INTEGER, updated_at INTEGER
);
CREATE TABLE IF NOT EXISTS action_states (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  order_id TEXT,
  action_id TEXT,
  action_type TEXT,
  status TEXT,
  result_desc TEXT,
  updated_at INTEGER,
  UNIQUE(order_id, action_id)
);
CREATE INDEX IF NOT EXISTS idx_orders_robot ON orders(robot_id);
`

// OpenStore opens SQLite (path or "file::memory:?cache=shared") and runs migrations.
// A "postgres://" prefix could be supported later by swapping the driver; out of scope now.
func OpenStore(dsn string) (*Store, error) {
	if strings.HasPrefix(dsn, "postgres://") {
		return nil, fmt.Errorf("postgres DSN not yet supported")
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // sqlite: serialize writes
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() int64 { return time.Now().Unix() }

func (s *Store) UpsertRobot(r RobotRecord) error {
	_, err := s.db.Exec(`
INSERT INTO robots (id,manufacturer,serial,atom_base_url,fastapi_http_url,fastapi_ws_url,webhook_secret,status,source,last_seen_at,created_at,updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  manufacturer=excluded.manufacturer, serial=excluded.serial,
  atom_base_url=excluded.atom_base_url, fastapi_http_url=excluded.fastapi_http_url,
  fastapi_ws_url=excluded.fastapi_ws_url, webhook_secret=excluded.webhook_secret,
  status=excluded.status, source=excluded.source, updated_at=excluded.updated_at`,
		r.ID, r.Manufacturer, r.Serial, r.AtomBaseURL, r.FastAPIHTTPURL, r.FastAPIWSURL,
		r.WebhookSecret, nz(r.Status, "offline"), nz(r.Source, "db"), r.LastSeenAt, now(), now())
	return err
}

func nz(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (s *Store) GetRobot(id string) (RobotRecord, error) {
	var r RobotRecord
	err := s.db.QueryRow(`SELECT id,manufacturer,serial,atom_base_url,fastapi_http_url,fastapi_ws_url,webhook_secret,status,source,last_seen_at FROM robots WHERE id=?`, id).
		Scan(&r.ID, &r.Manufacturer, &r.Serial, &r.AtomBaseURL, &r.FastAPIHTTPURL, &r.FastAPIWSURL, &r.WebhookSecret, &r.Status, &r.Source, &r.LastSeenAt)
	return r, err
}

func (s *Store) ListRobots() ([]RobotRecord, error)       { return s.queryRobots("") }
func (s *Store) ListActiveRobots() ([]RobotRecord, error) { return s.queryRobots("WHERE status != 'deleted'") }

func (s *Store) queryRobots(where string) ([]RobotRecord, error) {
	rows, err := s.db.Query(`SELECT id,manufacturer,serial,atom_base_url,fastapi_http_url,fastapi_ws_url,webhook_secret,status,source,last_seen_at FROM robots ` + where)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RobotRecord
	for rows.Next() {
		var r RobotRecord
		if err := rows.Scan(&r.ID, &r.Manufacturer, &r.Serial, &r.AtomBaseURL, &r.FastAPIHTTPURL, &r.FastAPIWSURL, &r.WebhookSecret, &r.Status, &r.Source, &r.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) TouchRobot(id, status string, t time.Time) error {
	_, err := s.db.Exec(`UPDATE robots SET status=?, last_seen_at=?, updated_at=? WHERE id=?`, status, t.Unix(), now(), id)
	return err
}

func (s *Store) SetRobotStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE robots SET status=?, updated_at=? WHERE id=?`, status, now(), id)
	return err
}

// --- orders ---

func (s *Store) InsertOrder(orderID, robotID string, updateID int, raw []byte) error {
	_, err := s.db.Exec(`
INSERT INTO orders (order_id,robot_id,order_update_id,status,raw_order,last_node_id,error_ref,created_at,updated_at)
VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT(order_id) DO UPDATE SET order_update_id=excluded.order_update_id, raw_order=excluded.raw_order, status='running', error_ref='', updated_at=excluded.updated_at`,
		orderID, robotID, updateID, "running", string(raw), "", "", now(), now())
	return err
}

func (s *Store) UpdateOrderNode(orderID, nodeID string) error {
	_, err := s.db.Exec(`UPDATE orders SET last_node_id=?, updated_at=? WHERE order_id=?`, nodeID, now(), orderID)
	return err
}

func (s *Store) FinishOrder(orderID, status, errorRef string) error {
	_, err := s.db.Exec(`UPDATE orders SET status=?, error_ref=?, updated_at=? WHERE order_id=?`, status, errorRef, now(), orderID)
	return err
}

func (s *Store) FailRunningOrders(errorRef string) (int64, error) {
	res, err := s.db.Exec(`UPDATE orders SET status='failed', error_ref=?, updated_at=? WHERE status='running'`, errorRef, now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) UpsertActionState(orderID, actionID, actionType, status, resultDesc string) error {
	_, err := s.db.Exec(`
INSERT INTO action_states (order_id,action_id,action_type,status,result_desc,updated_at)
VALUES (?,?,?,?,?,?)
ON CONFLICT(order_id,action_id) DO UPDATE SET status=excluded.status, result_desc=excluded.result_desc, updated_at=excluded.updated_at`,
		orderID, actionID, actionType, status, resultDesc, now())
	return err
}
```

- [ ] **Step 5: Run tests**

Run: `go test -run TestStore -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -m "feat: add SQLite store (robots/orders/action_states) with migrations"
```

---

## Phase 4 — RobotSession

### Task 5: Add `session.go`

**Files:**
- Create: `session.go`
- Test: `session_test.go`
- Modify: `mqtt_bridge.go` / `robot_ws.go` / `map_service.go` to accept a `*log.Logger` (minimal)

> Note: `RobotSession.Start()` does what `main.go` currently does for one robot. We pass each component a `*log.Logger` with prefix `[robot=<id>] `. To keep the diff small, only thread the logger into constructors that already exist; everything else keeps using the package `log` (acceptable for v1 — improve later).

- [ ] **Step 1: Decide the minimal logger surface**

Add to `mqtt_bridge.go`'s `MQTTBridge` an optional `logger *log.Logger` field set after construction (`mb.logger = lg`); when nil, fall back to a `log.Default()`-equivalent. Do the same for `RobotWSClient`. Simplest: add a package helper:
```go
// session.go
func robotLogger(id string) *log.Logger {
	return log.New(log.Writer(), "[robot="+id+"] ", log.Ldate|log.Ltime|log.Lmicroseconds)
}
```
and have `RobotSession` hold it; pass `sess.log` into the constructors that take it. For components not yet refactored, that's fine.

- [ ] **Step 2: Write the failing test**

`session_test.go`:
```go
package main

import "testing"

func TestNewRobotSession_Wires(t *testing.T) {
	srv := &ServerConfig{MQTTBroker: "tcp://127.0.0.1:1", MQTTPrefix: "/uagv/v2",
		Manufacturer: "atom", DefaultRobotSecret: "s", PublicBaseURL: "http://localhost:5201"}
	rec := RobotRecord{ID: "adai01", Serial: "adai01",
		AtomBaseURL: "http://127.0.0.1:18080", FastAPIHTTPURL: "http://127.0.0.1:18000",
		FastAPIWSURL: "ws://127.0.0.1:18000/ws"}
	sess := NewRobotSession(rec, srv, nil) // nil store ok for construction
	if sess == nil || sess.ID() != "adai01" {
		t.Fatalf("session not wired: %+v", sess)
	}
	if sess.cfg.RobotPort != "18080" {
		t.Errorf("cfg not derived: %+v", sess.cfg)
	}
	// Start with an unreachable robot must not panic; Stop must be safe.
	sess.Start()
	sess.Stop()
}
```

- [ ] **Step 3: Run test — expect compile failure**

Run: `go test -run TestNewRobotSession -v`
Expected: FAIL — `undefined: NewRobotSession`.

- [ ] **Step 4: Create `session.go`**

```go
package main

import (
	"log"
	"sync"
	"time"
)

// RobotSession owns everything for one robot: config, state, MQTT bridge, WS client,
// map service, order handler, and the goroutines that were previously in main().
type RobotSession struct {
	rec   RobotRecord
	cfg   *Config
	srv   *ServerConfig
	store *Store
	log   *log.Logger

	state      *RobotState
	mapService *MapService
	robotWS    *RobotWSClient
	mqttBridge *MQTTBridge

	mu      sync.Mutex
	started bool
	stopCh  chan struct{}
}

func NewRobotSession(rec RobotRecord, srv *ServerConfig, store *Store) *RobotSession {
	cfg := LoadConfigForRobot(rec, srv)
	lg := log.New(log.Writer(), "[robot="+rec.ID+"] ", log.Ldate|log.Ltime|log.Lmicroseconds)
	s := &RobotSession{rec: rec, cfg: cfg, srv: srv, store: store, log: lg, stopCh: make(chan struct{})}
	s.state = NewRobotState()
	s.mapService = NewMapService(cfg)
	s.robotWS = NewRobotWSClient(cfg, s.state)
	s.mqttBridge = NewMQTTBridge(cfg, s.state, s.mapService, s.robotWS)
	// best-effort: attach logger if the field exists (added in Task 5 Step 1)
	s.mqttBridge.logger = lg
	s.robotWS.logger = lg
	return s
}

func (s *RobotSession) ID() string         { return s.rec.ID }
func (s *RobotSession) State() *RobotState  { return s.state }
func (s *RobotSession) WebhookSecret() string { return s.cfg.WebhookSecret }

// HandleWebhook applies a robot webhook payload (single object or array) to state.
// Moved from webhook.go's handleWebhook body so the global httpapi can route to it.
func (s *RobotSession) HandleWebhook(body []byte) {
	applyWebhookPayload(s.state, s.cfg, body, s.log) // implemented in httpapi.go
	if s.store != nil {
		_ = s.store.TouchRobot(s.rec.ID, "online", time.Now())
	}
}

func (s *RobotSession) Start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()

	s.log.Printf("[Session] starting (atom=%s fastapi=%s mqtt=%s topic=%s)",
		s.cfg.RobotBaseURL(), s.cfg.RobotFastAPI, s.cfg.MQTTBroker, s.cfg.TopicPrefix())

	s.robotWS.Start()
	if err := s.mqttBridge.Connect(); err != nil {
		s.log.Printf("[Session] MQTT initial connect: %v (will retry)", err)
	}
	go s.safe("FetchInitialMapID", func() { FetchInitialMapID(s.mapService, s.state, s.cfg) })
	StartMapListLoop(s.mapService, s.state)
	s.mqttBridge.StartPublishLoops()

	// Tell the robot to call our webhook back at PUBLIC_BASE_URL/webhook/<id>.
	webhookURL := s.srv.PublicBaseURL + "/webhook/" + s.rec.ID
	go s.safe("RegisterWebhook", func() { RegisterWebhook(s.cfg, webhookURL) })

	go s.safe("statusLogger", func() { s.statusLoop() })

	if s.store != nil {
		_ = s.store.UpsertRobot(withStatus(s.rec, "online"))
	}
}

func (s *RobotSession) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	s.started = false
	close(s.stopCh)
	s.mu.Unlock()

	s.log.Printf("[Session] stopping")
	s.mqttBridge.Stop() // publishes connection=OFFLINE then disconnects
	s.robotWS.Stop()
	if s.store != nil {
		_ = s.store.SetRobotStatus(s.rec.ID, "offline")
	}
}

func (s *RobotSession) statusLoop() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			snap := s.state.Snapshot()
			if snap.LastUpdate.IsZero() {
				s.log.Printf("[Status] no data from robot yet")
			} else {
				s.log.Printf("[Status] %s (last update %v ago)", FormatStateLog(snap), time.Since(snap.LastUpdate).Round(time.Second))
			}
		}
	}
}

// safe runs fn, recovering from panics so one robot can't crash the process.
func (s *RobotSession) safe(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Printf("[Session] PANIC in %s: %v", name, r)
			if s.store != nil {
				_ = s.store.SetRobotStatus(s.rec.ID, "errored")
			}
		}
	}()
	fn()
}

func withStatus(r RobotRecord, st string) RobotRecord { r.Status = st; return r }
```

> If `RobotState` has no `IsDataFresh` / `FormatStateLog` exactly as named, adjust to whatever exists in `robot_state.go` / `state_*.go`. `FormatStateLog` and `statusLogger` already exist in the current codebase (from `main.go`).

- [ ] **Step 5: Add `logger` fields**

In `mqtt_bridge.go`: add `logger *log.Logger` to `MQTTBridge`; if you want, replace a few `log.Printf` with `mb.lg().Printf` where `func (mb *MQTTBridge) lg() *log.Logger { if mb.logger != nil { return mb.logger }; return log.Default() }`. Keep this minimal — converting every call is optional.
In `robot_ws.go`: same pattern with `logger *log.Logger`.

- [ ] **Step 6: Run tests**

Run: `go build ./... && go test -run 'TestNewRobotSession|TestStore|TestLoadServerConfig|TestLoadConfigForRobot' -v`
Expected: PASS. `applyWebhookPayload` is referenced but not yet defined — define a temporary stub in `session.go` (`func applyWebhookPayload(...) {}`) and remove it in Task 7, OR do Task 7 before running this. Recommendation: add the stub now, real impl in Task 7.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "feat: add RobotSession wrapping per-robot bridge components"
```

---

## Phase 5 — SessionManager

### Task 6: Add `manager.go`

**Files:**
- Create: `manager.go`
- Test: `manager_test.go`

- [ ] **Step 1: Write the failing test**

`manager_test.go`:
```go
package main

import (
	"testing"
)

func TestSessionManager_RegisterDeregister(t *testing.T) {
	srv := &ServerConfig{MQTTBroker: "tcp://127.0.0.1:1", MQTTPrefix: "/uagv/v2",
		Manufacturer: "atom", DefaultRobotSecret: "s", PublicBaseURL: "http://localhost"}
	st := newTestStore(t)
	m := NewSessionManager(srv, st)
	defer m.StopAll()

	rec := RobotRecord{ID: "r1", Serial: "r1", AtomBaseURL: "http://127.0.0.1:18080",
		FastAPIHTTPURL: "http://127.0.0.1:18000", FastAPIWSURL: "ws://127.0.0.1:18000/ws", Source: "yaml"}
	if err := m.Register(rec); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if m.Get("r1") == nil {
		t.Fatalf("Get r1 nil after Register")
	}
	if len(m.List()) != 1 {
		t.Fatalf("List len != 1")
	}
	// re-register same id replaces, no leak
	if err := m.Register(rec); err != nil {
		t.Fatalf("re-Register: %v", err)
	}
	if len(m.List()) != 1 {
		t.Fatalf("List len != 1 after re-register")
	}
	m.Deregister("r1")
	if m.Get("r1") != nil {
		t.Fatalf("Get r1 not nil after Deregister")
	}
	got, _ := st.GetRobot("r1")
	if got.Status != "deleted" {
		t.Errorf("DB status after Deregister = %q, want deleted", got.Status)
	}
}
```

- [ ] **Step 2: Run test — expect compile failure**

Run: `go test -run TestSessionManager -v`
Expected: FAIL — `undefined: NewSessionManager`.

- [ ] **Step 3: Create `manager.go`**

```go
package main

import (
	"log"
	"sync"
	"time"
)

type SessionManager struct {
	srv   *ServerConfig
	store *Store

	mu       sync.RWMutex
	sessions map[string]*RobotSession

	reaperStop chan struct{}
}

func NewSessionManager(srv *ServerConfig, store *Store) *SessionManager {
	m := &SessionManager{srv: srv, store: store, sessions: map[string]*RobotSession{}, reaperStop: make(chan struct{})}
	go m.reaper()
	return m
}

// Register starts (or restarts) a session for rec.
func (m *SessionManager) Register(rec RobotRecord) error {
	if rec.ID == "" {
		return errEmptyID
	}
	m.mu.Lock()
	if old, ok := m.sessions[rec.ID]; ok {
		delete(m.sessions, rec.ID)
		m.mu.Unlock()
		old.Stop()
		m.mu.Lock()
	}
	sess := NewRobotSession(rec, m.srv, m.store)
	m.sessions[rec.ID] = sess
	m.mu.Unlock()
	if m.store != nil {
		_ = m.store.UpsertRobot(rec)
	}
	sess.Start()
	log.Printf("[Manager] registered robot %s (source=%s)", rec.ID, rec.Source)
	return nil
}

func (m *SessionManager) Deregister(id string) {
	m.mu.Lock()
	sess := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if sess != nil {
		sess.Stop()
	}
	if m.store != nil {
		_ = m.store.SetRobotStatus(id, "deleted")
	}
	log.Printf("[Manager] deregistered robot %s", id)
}

func (m *SessionManager) Get(id string) *RobotSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

func (m *SessionManager) List() []*RobotSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*RobotSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

func (m *SessionManager) IDs() map[string]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]bool{}
	for id := range m.sessions {
		out[id] = true
	}
	return out
}

func (m *SessionManager) StopAll() {
	close(m.reaperStop)
	for _, s := range m.List() {
		s.Stop()
	}
}

// LoadFromStore registers every non-deleted robot found in the DB. Call at startup.
func (m *SessionManager) LoadFromStore() {
	if m.store == nil {
		return
	}
	recs, err := m.store.ListActiveRobots()
	if err != nil {
		log.Printf("[Manager] LoadFromStore: %v", err)
		return
	}
	for _, r := range recs {
		if err := m.Register(r); err != nil {
			log.Printf("[Manager] register %s from store: %v", r.ID, err)
		}
	}
}

func (m *SessionManager) reaper() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-m.reaperStop:
			return
		case <-t.C:
			if m.store == nil {
				continue
			}
			recs, err := m.store.ListActiveRobots()
			if err != nil {
				continue
			}
			for _, r := range recs {
				if r.Status == "errored" {
					log.Printf("[Manager] reaper: re-registering errored robot %s", r.ID)
					_ = m.Register(withStatus(r, "online"))
				}
			}
		}
	}
}
```
Add to `manager.go` (or `errors.go`): `var errEmptyID = errors.New("robot id is empty")` (import `errors`).

- [ ] **Step 4: Run tests**

Run: `go test -run 'TestSessionManager|TestStore|TestNewRobotSession' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "feat: add SessionManager with crash reaper and store-backed loading"
```

---

## Phase 6 — HTTP API (webhook + admin)

### Task 7: Add `httpapi.go`, delete `webhook.go`

**Files:**
- Create: `httpapi.go`
- Test: `httpapi_test.go`
- Delete: `webhook.go`

- [ ] **Step 1: Write the failing test**

`httpapi_test.go`:
```go
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestAPI(t *testing.T) (*APIServer, *SessionManager, *Store) {
	t.Helper()
	srv := &ServerConfig{ListenAddr: ":0", AdminToken: "tok", DefaultRobotSecret: "wsec",
		MQTTBroker: "tcp://127.0.0.1:1", MQTTPrefix: "/uagv/v2", Manufacturer: "atom", PublicBaseURL: "http://localhost"}
	st := newTestStore(t)
	mgr := NewSessionManager(srv, st)
	t.Cleanup(mgr.StopAll)
	api := NewAPIServer(srv, mgr, st)
	return api, mgr, st
}

func TestAPI_Healthz(t *testing.T) {
	api, _, _ := newTestAPI(t)
	rec := httptest.NewRecorder()
	api.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "robots") {
		t.Fatalf("healthz: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAPI_AdminAuth(t *testing.T) {
	api, _, _ := newTestAPI(t)
	rec := httptest.NewRecorder()
	api.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/robots", nil))
	if rec.Code != 401 {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/robots", nil)
	req.Header.Set("Authorization", "Bearer tok")
	api.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200 with token, got %d", rec.Code)
	}
}

func TestAPI_AdminRegisterThenWebhook(t *testing.T) {
	api, mgr, _ := newTestAPI(t)
	body := `{"id":"r1","serial":"r1","atomBaseURL":"http://127.0.0.1:18080","fastapiHTTPURL":"http://127.0.0.1:18000","fastapiWSURL":"ws://127.0.0.1:18000/ws","webhookSecret":"abc"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/robots", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	api.mux.ServeHTTP(rec, req)
	if rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	if mgr.Get("r1") == nil {
		t.Fatalf("session r1 not created")
	}
	// webhook with wrong secret -> 401
	rec = httptest.NewRecorder()
	wreq := httptest.NewRequest("POST", "/webhook/r1", strings.NewReader(`[{"foo":1}]`))
	wreq.Header.Set("X-Webhook-Secret", "WRONG")
	api.mux.ServeHTTP(rec, wreq)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for bad secret, got %d", rec.Code)
	}
	// webhook with right secret -> 200
	rec = httptest.NewRecorder()
	wreq = httptest.NewRequest("POST", "/webhook/r1", strings.NewReader(`[{"foo":1}]`))
	wreq.Header.Set("X-Webhook-Secret", "abc")
	api.mux.ServeHTTP(rec, wreq)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestAPI_WebhookUnknownRobotProvisional(t *testing.T) {
	api, mgr, st := newTestAPI(t)
	rec := httptest.NewRecorder()
	wreq := httptest.NewRequest("POST", "/webhook/newbot", strings.NewReader(`[{"foo":1}]`))
	wreq.RemoteAddr = "5.6.7.8:55555"
	// no secret header; provisional path accepts and marks
	api.mux.ServeHTTP(rec, wreq)
	if rec.Code != http.StatusAccepted && rec.Code != 200 {
		t.Fatalf("expected 202/200 for provisional, got %d %s", rec.Code, rec.Body.String())
	}
	if mgr.Get("newbot") == nil {
		t.Fatalf("provisional session not created")
	}
	got, _ := st.GetRobot("newbot")
	if got.Status != "provisional" {
		t.Errorf("status = %q, want provisional", got.Status)
	}
}
```

- [ ] **Step 2: Run test — expect compile failure**

Run: `go test -run TestAPI -v`
Expected: FAIL — `undefined: NewAPIServer`, `APIServer`.

- [ ] **Step 3: Create `httpapi.go`** (port the webhook body from old `webhook.go`)

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

type APIServer struct {
	srv    *ServerConfig
	mgr    *SessionManager
	store  *Store
	mux    *http.ServeMux
	server *http.Server
}

func NewAPIServer(srv *ServerConfig, mgr *SessionManager, store *Store) *APIServer {
	a := &APIServer{srv: srv, mgr: mgr, store: store, mux: http.NewServeMux()}
	a.mux.HandleFunc("/healthz", a.handleHealthz)
	a.mux.HandleFunc("/webhook/", a.handleWebhook)         // /webhook/<robotKey>
	a.mux.HandleFunc("/admin/robots", a.handleAdminRobots) // GET list, POST register
	a.mux.HandleFunc("/admin/robots/", a.handleAdminRobot) // GET/DELETE /admin/robots/<id>
	return a
}

func (a *APIServer) Start() error {
	a.server = &http.Server{Addr: a.srv.ListenAddr, Handler: a.mux, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[API] server error: %v", err)
		}
	}()
	log.Printf("[API] listening on %s", a.srv.ListenAddr)
	return nil
}

func (a *APIServer) Stop() {
	if a.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = a.server.Shutdown(ctx)
	}
}

// ---- /healthz ----

func (a *APIServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	type rstat struct {
		ID        string `json:"id"`
		LastSeen  string `json:"lastSeen"`
		DataFresh bool   `json:"dataFresh"`
	}
	var robots []rstat
	for _, s := range a.mgr.List() {
		snap := s.State().Snapshot()
		robots = append(robots, rstat{ID: s.ID(), LastSeen: snap.LastUpdate.Format(time.RFC3339), DataFresh: time.Since(snap.LastUpdate) < 15*time.Second})
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "robots": robots})
}

// ---- /webhook/<robotKey> ----

func (a *APIServer) handleWebhook(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/webhook/")
	if key == "" {
		http.Error(w, "missing robot key", 400)
		return
	}
	if r.Method == http.MethodGet {
		fmt.Fprint(w, "pi2w-bridge ok")
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	defer r.Body.Close()

	sess := a.mgr.Get(key)
	if sess == nil {
		// unknown robot: try to provision from DB/yaml, else from source IP + conventions.
		if rec, err := a.provisionRobot(key, r); err == nil {
			_ = a.mgr.Register(rec)
			sess = a.mgr.Get(key)
		}
		if sess == nil {
			http.Error(w, "unknown robot", 404)
			return
		}
		// provisional: accept this payload, return 202
		sess.HandleWebhook(body)
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, "provisional ok")
		return
	}
	// known robot: require secret (constant-time compare not critical here but use ==)
	if got := r.Header.Get("X-Webhook-Secret"); got != sess.WebhookSecret() {
		http.Error(w, "bad webhook secret", http.StatusUnauthorized)
		return
	}
	if len(body) > 0 {
		sess.HandleWebhook(body)
	}
	w.WriteHeader(200)
	fmt.Fprint(w, "ok")
}

func (a *APIServer) provisionRobot(key string, r *http.Request) (RobotRecord, error) {
	// 1) DB
	if a.store != nil {
		if rec, err := a.store.GetRobot(key); err == nil && rec.ID != "" && rec.Status != "deleted" {
			return rec, nil
		}
	}
	// 2) source IP + conventional ports
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if host == "" {
		return RobotRecord{}, fmt.Errorf("no source ip")
	}
	rec := RobotRecord{
		ID: key, Serial: key, Manufacturer: a.srv.Manufacturer,
		AtomBaseURL:    "http://" + host + ":8080",
		FastAPIHTTPURL: "http://" + host + ":8000",
		FastAPIWSURL:   "ws://" + host + ":8000/ws",
		WebhookSecret:  a.srv.DefaultRobotSecret,
		Status:         "provisional", Source: "provisional",
	}
	if a.store != nil {
		_ = a.store.UpsertRobot(rec)
	}
	log.Printf("[API] provisioned robot %s from %s", key, host)
	return rec, nil
}

// ---- /admin/* ----

func (a *APIServer) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+a.srv.AdminToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (a *APIServer) handleAdminRobots(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		var out []RobotRecord
		if a.store != nil {
			out, _ = a.store.ListRobots()
		}
		live := a.mgr.IDs()
		for i := range out {
			if live[out[i].ID] {
				out[i].Status = "online"
			}
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var rec RobotRecord
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&rec); err != nil || rec.ID == "" {
			http.Error(w, "bad robot record", 400)
			return
		}
		if rec.Source == "" {
			rec.Source = "admin"
		}
		if err := a.mgr.Register(rec); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, 201, map[string]string{"id": rec.ID, "status": "registered"})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *APIServer) handleAdminRobot(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/admin/robots/")
	if id == "" {
		http.Error(w, "missing id", 400)
		return
	}
	switch r.Method {
	case http.MethodGet:
		rec, err := a.store.GetRobot(id)
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
		writeJSON(w, 200, rec)
	case http.MethodDelete:
		a.mgr.Deregister(id)
		writeJSON(w, 200, map[string]string{"id": id, "status": "deleted"})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// applyWebhookPayload parses a robot webhook payload (single object or array) and
// applies each item to state — this is the body of the old webhook.go handleWebhook.
func applyWebhookPayload(state *RobotState, cfg *Config, body []byte, lg *log.Logger) {
	if len(body) == 0 {
		return
	}
	var raw json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		lg.Printf("[Webhook] invalid JSON: %v", err)
		return
	}
	var items []map[string]interface{}
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &items); err != nil {
			lg.Printf("[Webhook] invalid JSON array: %v", err)
			return
		}
	} else {
		var single map[string]interface{}
		if err := json.Unmarshal(raw, &single); err != nil {
			lg.Printf("[Webhook] invalid JSON object: %v", err)
			return
		}
		items = []map[string]interface{}{single}
	}
	for _, item := range items {
		ApplyWebhookData(state, item)
		if cfg != nil {
			if rs, ok := item["route_status"].(map[string]interface{}); ok {
				if s, _ := rs["status"].(string); s == "map_loaded" {
					go func() {
						if name := queryATOMCurrentMap(cfg); name != "" {
							state.SetMapID(name)
						}
					}()
				}
			}
		}
	}
}
```

> `ApplyWebhookData`, `queryATOMCurrentMap`, `RobotState.SetMapID`, `RobotState.Snapshot` already exist (used by the old `webhook.go` / `map_service.go`). If signatures differ, match the existing ones. Remove the temporary `applyWebhookPayload` stub added in Task 5.

- [ ] **Step 4: Delete `webhook.go`**

```bash
git rm webhook.go
```
Verify nothing else references `WebhookServer` / `NewWebhookServer` (only `main.go` did — fixed in Task 8). Also `register.go`'s `RegisterWebhook(cfg, listenAddr)` signature is fine; it just receives the full URL now (`session.go` passes `PublicBaseURL + "/webhook/" + id`). Inspect `RegisterWebhook` — if it does `"http://"+localIP+listenAddr`, change it to use the passed string verbatim as the webhook URL. `getLocalIP` / `readLastmap` may become unused → remove if so.

- [ ] **Step 5: Run tests**

Run: `go build ./... && go test -run 'TestAPI|TestSessionManager|TestStore|TestNewRobotSession|TestLoadServerConfig|TestLoadConfigForRobot' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -m "feat: add httpapi (webhook + admin); remove old webhook server"
```

---

## Phase 7 — robots.yaml hot-reload

### Task 8: Add `robots_yaml.go`

**Files:**
- Create: `robots_yaml.go`, `robots.example.yaml`
- Test: `robots_yaml_test.go`
- Modify: `go.mod` (add `github.com/fsnotify/fsnotify`)

- [ ] **Step 1: Add dependency**

```bash
go get github.com/fsnotify/fsnotify@latest
```

- [ ] **Step 2: Write the failing test**

`robots_yaml_test.go`:
```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRobotsYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "robots.yaml")
	os.WriteFile(p, []byte(`
robots:
  - id: adai01
    serial: adai01
    atomBaseURL: http://1.2.3.4:8080
    fastapiHTTPURL: http://1.2.3.4:8000
    fastapiWSURL: ws://1.2.3.4:8000/ws
  - id: adai02
    atomBaseURL: http://1.2.3.5:8080
`), 0644)
	recs, err := LoadRobotsYAML(p)
	if err != nil {
		t.Fatalf("LoadRobotsYAML: %v", err)
	}
	if len(recs) != 2 || recs[0].ID != "adai01" || recs[1].ID != "adai02" {
		t.Fatalf("parsed wrong: %+v", recs)
	}
}

func TestLoadRobotsYAML_Missing(t *testing.T) {
	recs, err := LoadRobotsYAML("/nonexistent/robots.yaml")
	if err != nil || len(recs) != 0 {
		t.Fatalf("missing file should be (nil,nil): %v %v", recs, err)
	}
}
```

- [ ] **Step 3: Run test — expect compile failure**

Run: `go test -run TestLoadRobotsYAML -v`
Expected: FAIL — `undefined: LoadRobotsYAML`.

- [ ] **Step 4: Create `robots_yaml.go`**

```go
package main

import (
	"log"
	"os"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

type robotsФile struct {
	Robots []RobotRecord `yaml:"robots"`
}

// LoadRobotsYAML returns the robots declared in path. A missing file is not an error.
func LoadRobotsYAML(path string) ([]RobotRecord, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f robotsФile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	for i := range f.Robots {
		if f.Robots[i].Source == "" {
			f.Robots[i].Source = "yaml"
		}
	}
	return f.Robots, nil
}

// SyncRobotsYAML registers/updates robots from the file and deregisters yaml-sourced
// robots that disappeared from it.
func SyncRobotsYAML(path string, mgr *SessionManager, store *Store) {
	recs, err := LoadRobotsYAML(path)
	if err != nil {
		log.Printf("[robots.yaml] load error: %v", err)
		return
	}
	want := map[string]bool{}
	for _, r := range recs {
		want[r.ID] = true
		_ = mgr.Register(r)
	}
	if store != nil {
		known, _ := store.ListActiveRobots()
		for _, k := range known {
			if k.Source == "yaml" && !want[k.ID] {
				mgr.Deregister(k.ID)
			}
		}
	}
}

// WatchRobotsYAML calls SyncRobotsYAML on startup and whenever the file changes.
func WatchRobotsYAML(path string, mgr *SessionManager, store *Store) {
	SyncRobotsYAML(path, mgr, store)
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("[robots.yaml] watcher disabled: %v", err)
		return
	}
	// watch the dir so the watch survives editor atomic-replace
	dir := "."
	if d := dirOf(path); d != "" {
		dir = d
	}
	if err := w.Add(dir); err != nil {
		log.Printf("[robots.yaml] watch %s: %v", dir, err)
		return
	}
	go func() {
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepathBase(ev.Name) == filepathBase(path) {
					log.Printf("[robots.yaml] change detected (%s), resyncing", ev.Op)
					SyncRobotsYAML(path, mgr, store)
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Printf("[robots.yaml] watch error: %v", err)
			}
		}
	}()
}
```
Add small helpers (or just use `path/filepath`): replace `dirOf` → `filepath.Dir`, `filepathBase` → `filepath.Base` and import `"path/filepath"`. (The placeholder names above must be replaced with the real `filepath` calls before this compiles. Also rename `robotsФile` → `robotsFile` — no Cyrillic; that was a transcription slip, fix it.)

- [ ] **Step 5: Create `robots.example.yaml`**

```yaml
# Copy to robots.yaml (gitignored). Hot-reloaded — edit and save to apply.
robots:
  - id: adai01
    manufacturer: atom
    serial: adai01
    atomBaseURL: http://1.2.3.4:8080
    fastapiHTTPURL: http://1.2.3.4:8000
    fastapiWSURL: ws://1.2.3.4:8000/ws
    webhookSecret: change-me-per-robot
  # - id: adai02
  #   atomBaseURL: http://1.2.3.5:8080   # fastapi/serial/secret will be defaulted
```
Add `robots.yaml` to `.gitignore`.

- [ ] **Step 6: Run tests**

Run: `go build ./... && go test -run 'TestLoadRobotsYAML|TestAPI|TestSessionManager|TestStore' -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "feat: add robots.yaml hot-reload (fsnotify)"
```

---

## Phase 8 — Wire up `main.go`

### Task 9: Rewrite `main.go`

**Files:**
- Modify: `main.go`

- [ ] **Step 1: Replace `main()` with the new startup flow**

```go
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.Println("=== Pi2W Cloud Multi-Robot Bridge ===")

	srv := LoadServerConfig()
	log.Printf("[Config] listen=%s public=%s db=%s mqtt=%s prefix=%s",
		srv.ListenAddr, srv.PublicBaseURL, srv.DBPath, srv.MQTTBroker, srv.MQTTPrefix)

	store, err := OpenStore(srv.DBPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if n, err := store.FailRunningOrders("bridge_restarted"); err == nil && n > 0 {
		log.Printf("[Main] marked %d in-flight order(s) failed (bridge_restarted)", n)
	}

	mgr := NewSessionManager(srv, store)
	mgr.LoadFromStore()

	robotsYAMLPath := envOrDefault("ROBOTS_YAML", "robots.yaml")
	WatchRobotsYAML(robotsYAMLPath, mgr, store)

	api := NewAPIServer(srv, mgr, store)
	if err := api.Start(); err != nil {
		log.Fatalf("api start: %v", err)
	}

	log.Println("[Main] up. Managing", len(mgr.List()), "robot session(s).")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("[Main] shutting down...")
	api.Stop()
	mgr.StopAll()
	log.Println("[Main] bye")
}
```
Keep `statusLogger` only if still referenced (it isn't anymore — `RobotSession.statusLoop` replaced it). Remove `statusLogger` and the `time` import if unused. Remove `FormatStateLog` import worries — it's same package.

- [ ] **Step 2: Build & vet**

Run: `go build ./... && go vet ./...`
Expected: clean. Fix any remaining undefined refs (likely leftover `LoadConfig`, `NewWebhookServer`, elevator symbols — all should already be gone).

- [ ] **Step 3: Run full test suite**

Run: `go test ./... -v 2>&1 | tail -30`
Expected: all PASS.

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "feat: rewire main() for multi-robot cloud service"
```

---

## Phase 9 — Integration test (fake robot + fake MQTT)

### Task 10: End-to-end-ish session test

**Files:**
- Create: `integration_test.go`

- [ ] **Step 1: Write the test**

`integration_test.go`:
```go
package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRobot stands in for the ATOM API + FastAPI HTTP surface a session pokes at startup.
func TestSession_StartsAgainstFakeRobot(t *testing.T) {
	var hits int32
	fr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		// be permissive: anything the bridge calls (register webhook, map list, current map)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"ok","maps":[],"name":""}`))
	}))
	defer fr.Close()

	srv := &ServerConfig{ListenAddr: ":0", AdminToken: "tok", DefaultRobotSecret: "wsec",
		MQTTBroker: "tcp://127.0.0.1:1" /* unreachable; Connect retries, must not block */, MQTTPrefix: "/uagv/v2",
		Manufacturer: "atom", PublicBaseURL: "http://localhost:5201"}
	st := newTestStore(t)
	mgr := NewSessionManager(srv, st)
	defer mgr.StopAll()

	rec := RobotRecord{ID: "fakebot", Serial: "fakebot",
		AtomBaseURL: fr.URL, FastAPIHTTPURL: fr.URL, FastAPIWSURL: "ws://127.0.0.1:1/ws", Source: "test"}
	if err := mgr.Register(rec); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// give startup goroutines a moment
	time.Sleep(300 * time.Millisecond)
	if atomic.LoadInt32(&hits) == 0 {
		t.Errorf("expected the session to call the fake robot at startup")
	}
	// feed a webhook through the session and confirm state updates
	sess := mgr.Get("fakebot")
	sess.HandleWebhook([]byte(`[{"pose":{"x":1,"y":2,"theta":0}}]`))
	snap := sess.State().Snapshot()
	if snap.LastUpdate.IsZero() {
		t.Errorf("state not updated by webhook")
	}
}
```
Adjust assertions to the actual fields on `RobotState.Snapshot()` / what `ApplyWebhookData` reads (`pose` shape may differ — match `webhook.go`'s original debug log which printed `item["pose"]`).

- [ ] **Step 2: Run it**

Run: `go test -run TestSession_StartsAgainstFakeRobot -v`
Expected: PASS. If `mqttBridge.Connect()` blocks on an unreachable broker, change `srv.MQTTBroker` to a value that fails fast, or have the test not assert on MQTT at all (it already doesn't).

- [ ] **Step 3: Run full suite + race detector**

Run: `go test -race ./... 2>&1 | tail -20`
Expected: PASS, no data races. Fix any reported races (likely in `SessionManager` map access or `RobotState` — add locking where flagged).

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "test: integration test for session startup + webhook against fake robot"
```

---

## Phase 10 — Containerization

### Task 11: Dockerfile + docker-compose

**Files:**
- Create: `Dockerfile`, `docker-compose.yml`, `.dockerignore`

- [ ] **Step 1: `Dockerfile`**

```dockerfile
FROM golang:1.21-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pi2w-bridge .

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/pi2w-bridge /pi2w-bridge
VOLUME ["/data"]
EXPOSE 5201
ENTRYPOINT ["/pi2w-bridge"]
```

- [ ] **Step 2: `.dockerignore`**

```
.git
docs
*_test.go
robots.yaml
.env
```

- [ ] **Step 3: `docker-compose.yml`**

```yaml
services:
  bridge:
    build: .
    restart: unless-stopped
    ports:
      - "5201:5201"
    environment:
      LISTEN_ADDR: ":5201"
      PUBLIC_BASE_URL: "https://bridge.example.com"   # what robots POST webhooks to
      DB_PATH: "/data/pi2w-bridge.db"
      ROBOTS_YAML: "/config/robots.yaml"
      MQTT_BROKER: "wss://nexmqtt.jini.tw:443/mqtt"
      MQTT_USER: "bibi"
      MQTT_PASS: "70595145"
      MQTT_PREFIX: "/uagv/v2"
      VDA_MANUFACTURER: "atom"
      TTS_URL: ""
      ADMIN_TOKEN: "change-me"
      DEFAULT_ROBOT_SECRET: "change-me"
    volumes:
      - bridge-data:/data
      - ./robots.yaml:/config/robots.yaml:ro
volumes:
  bridge-data:
```
(Optionally add a `caddy` service for TLS reverse-proxying `/webhook/*` and `/admin/*` — note in README, not required for the plan.)

- [ ] **Step 4: Verify the image builds**

Run: `docker build -t pi2w-bridge:dev .`
Expected: image builds successfully.

- [ ] **Step 5: Smoke-run**

Run: `docker run --rm -e ADMIN_TOKEN=t -e DB_PATH=/tmp/x.db pi2w-bridge:dev 2>&1 | head -5`
Expected: prints `=== Pi2W Cloud Multi-Robot Bridge ===` and `[API] listening on :5201` (then sits idle — Ctrl-C).

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -m "build: add Dockerfile + docker-compose"
```

---

## Phase 11 — Docs

### Task 12: Update README

**Files:**
- Modify: `README.md` (create if absent)

- [ ] **Step 1: Write the new README**

Cover: what the service is now (cloud, multi-robot), architecture diagram (text), how to configure (`.env` table of all env vars with defaults, `robots.yaml` format), how robots register (3 paths), the HTTP API table (`/webhook/{robotKey}`, `/healthz`, `/admin/robots` CRUD with `curl` examples using `Authorization: Bearer`), how to deploy (`docker compose up -d`, TLS note), how to add a robot (append to `robots.yaml` OR `curl -X POST /admin/robots`), what was removed vs the old Pi version (elevator, USB watchdog, wifi, tunnel — and that cross-map orders now fail with `cross_map_not_supported`), and the security note (secrets currently shipped as env defaults — change them in prod).

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs: rewrite README for cloud multi-robot bridge"
```

---

## Phase 12 — Final verification

### Task 13: Full verification pass

- [ ] **Step 1:** `go build ./...` — clean
- [ ] **Step 2:** `go vet ./...` — clean
- [ ] **Step 3:** `go test -race ./...` — all pass, no races
- [ ] **Step 4:** `gofmt -l .` — empty (no unformatted files); run `gofmt -w .` if not
- [ ] **Step 5:** Manual smoke: `DB_PATH=/tmp/s.db ADMIN_TOKEN=t go run .` then in another shell:
  - `curl -s localhost:5201/healthz` → JSON with `"robots"`
  - `curl -s -H 'Authorization: Bearer t' localhost:5201/admin/robots` → `[]` or list
  - `curl -s -XPOST -H 'Authorization: Bearer t' -d '{"id":"t1","atomBaseURL":"http://127.0.0.1:9","fastapiHTTPURL":"http://127.0.0.1:9","fastapiWSURL":"ws://127.0.0.1:9/ws"}' localhost:5201/admin/robots` → `{"id":"t1",...}`
  - `curl -s localhost:5201/healthz` → now shows `t1`
- [ ] **Step 6:** Open a PR.

```bash
git push -u origin feat/cloud-multi-robot
gh pr create --fill
```

---

## Self-review notes (resolved)

- **Spec coverage:** §3 file layout → Tasks 1–2,7,8; §4 session lifecycle → Task 5; §5 registration (3 paths) → Tasks 6 (DB load), 8 (yaml), 7 (provisional webhook); §6 DB schema → Task 4; §7 HTTP API → Task 7; §8 security (env defaults, secret header, bearer) → Tasks 3,7; §9 deployment → Task 11; §10 testing → Tasks 3–10; §11 phasing → mirrored. ✅
- **Ordering caveat:** `main.go` must keep compiling through Phases 2–6 — Task 3 Step 6 replaces `main()` with a stub; Task 9 puts the real one in. The temporary `applyWebhookPayload` stub from Task 5 is replaced by the real one in Task 7. Both noted inline.
- **Naming consistency:** `Store`/`OpenStore`/`SessionManager`/`RobotSession`/`APIServer`/`NewAPIServer`/`RobotRecord`/`ServerConfig`/`LoadServerConfig`/`LoadConfigForRobot` used consistently throughout. `RegisterWebhook` keeps its name but now receives a full URL.
- **Known cleanups for the implementer (flagged inline, not blockers):** the `robots_yaml.go` skeleton has placeholder helper names (`dirOf`/`filepathBase`/`robotsФile`) that MUST be replaced with `filepath.Dir`/`filepath.Base`/`robotsFile` before it compiles; logger threading into `MQTTBridge`/`RobotWSClient` is minimal-effort (add field, fall back to `log.Default()`).
