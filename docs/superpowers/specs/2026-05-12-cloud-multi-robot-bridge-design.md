# pi2w-bridge → 雲端多租戶 Bridge 設計

**日期:** 2026-05-12
**狀態:** 設計確認中 → 待寫實作計畫
**作者:** bibi + Claude

---

## 1. 背景與目標

`pi2w-bridge` 目前是跑在 Raspberry Pi Zero 2 W 上的 VDA5050 ↔ ATOM 機器人協定橋接器,**1 顆 Pi : 1 台機器人,且與機器人同一區網**。要把它搬到雲端,變成**單一服務管理 N 台機器人**。

**前提條件(已與使用者確認):**
- 每台機器人已自行對外開放固定的公開 IP:Port,雲端可直接 call 其 ATOM API(`:8080`)與 FastAPI(`:8000`,含 WebSocket)。不需要 tunnel / VPN。
- 不能修改機器人下位機軟體(但可一次性設定它把 webhook 打到雲端 URL)。
- 機器人「上線」是動態的;不想做 UI;穩定資訊(VDA identity、URL、所屬大樓)變動不頻繁。
- 部署在單一雲端 VM(docker-compose 等級),狀態落地到 SQLite/Postgres。
- **電梯 / 跨樓層功能本次全部移除。**
- 目前在用的參數與密碼直接寫進 `envOrDefault` 的預設值(沿用現值,ENV 可覆蓋),**不留空**、開箱即用。

**非目標:**
- 不重構成 `internal/` 多套件(維持單一 `package main`)。
- 不為了搬遷而重寫 `order_handler.go` 的訂單狀態機內部邏輯。
- 重啟後不做 in-flight order 的真 resume(標 failed,靠 VDA5050 master 重送)。
- 不上 Kubernetes。

---

## 2. 關鍵發現:重構比看起來小

現有 code 已是 DI 友善:`RobotState` / `MQTTBridge` / `RobotWSClient` / `MapService` / `OrderHandler` 全部是 `New...(cfg, ...)` 建構子拿參數,沒有 package-level 的機器人單例。真正「全域」的只有:
1. `main.go` 寫死建一份 — 改成由 `SessionManager` 建 N 份。
2. Pi 主機專用檔(`usb_watchdog.go` / `net_snapshot.go` / `wifi_service.go` / `tunnel_url.go`)— 上雲後直接刪。
3. 沒帶 robot-ID 前綴的 `log.Printf` — 改成 session 注入的帶前綴 logger。

所以 `order_handler.go` 的狀態機**不動內部邏輯**,只是從「被建一次」變成「被建 N 次」。

---

## 3. 架構與檔案佈局

維持單一 `package main`。

### 新增

| 檔案 | 職責 |
|---|---|
| `session.go` | `RobotSession` struct — 持有一台機器人的 `*Config` / `*RobotState` / `*MQTTBridge` / `*RobotWSClient` / `*MapService` / `*OrderHandler`;`Start()` / `Stop()`;內部跑現在 `main.go` 那些 publish/watch loop;提供帶 `[robot=<id>]` 前綴的 logger 傳給下層 |
| `manager.go` | `SessionManager` — `map[robotID]*RobotSession` + `sync.RWMutex`;`Register(RobotRecord)` / `Deregister(id)` / `Get(id)` / `List()`;1-min reaper 重建 errored session(backoff) |
| `store.go` | DB 層:`database/sql` + `modernc.org/sqlite`(純 Go 免 CGO),schema 相容 Postgres;啟動跑內嵌 migration;`robots` / `orders` / `action_states` CRUD |
| `httpapi.go` | 取代 `webhook.go`:`POST /webhook/{robotKey}`、`GET /healthz`、`GET/POST/DELETE /admin/robots`、`GET /admin/robots/{id}` |
| `robots_yaml.go` | `robots.yaml` 載入 + `fsnotify` 熱載入 → diff 後呼叫 `SessionManager.Register/Deregister` |

### 改動(小)

- `main.go` — 建 `store` → 建 `SessionManager` → `store.ListRobots()` 逐一 `Register()` → 載 `robots.yaml` → 起 `httpapi` server → 等 signal。
- `config.go` — 拆成 (a) 全域 `ServerConfig`(DB DSN、listen addr、MQTT broker/user/pass/prefix、`ADMIN_TOKEN`、`TTS_URL` 等,全部 `envOrDefault` 帶現值預設)+ (b) per-robot 來自 `RobotRecord` 的部分(`RobotIP`/`RobotPort`/`RobotFastAPI`/`Manufacturer`/`SerialNumber`/`webhookSecret`)。`LoadConfigForRobot(rec, serverCfg) *Config` 組出每個 session 用的 `*Config`。
- 全專案 `log.Printf` → 帶 robot 前綴(透過 session 注入的 logger;不需要的全域 log 保留)。
- `register.go` 的 `RegisterWebhook` — 改成「session 啟動時告訴該台機器人:webhook 回呼 `https://<cloud>/webhook/<robotKey>`,帶 secret」。
- `mqtt_bridge.go` — 移除 `elevatorService` 欄位與 instantActions 裡轉發電梯動作的分支。

### 刪除

**Pi 主機專用:** `usb_watchdog.go`、`net_snapshot.go`、`wifi_service.go`、`tunnel_url.go`;`main.go` 裡對應的 `StartUSBWatchdog` / `StartUSBLinkWatchdog` / `StartNetworkSnapshotLogger` / `StartTunnelURLWatcher` 呼叫。

**電梯相關:** `elevator_service.go`、`elevator_config.go`、`elevator_config.json`(+`.example`)、`docs/elevator-floor-change.md`、`test_elevator_full.py`、`test_elevator_mqtt.py`;`main.go` 的 `LoadElevatorConfig` / `NewElevatorService` / `elevatorSvc.Start()`;`order_handler.go` 的 `handleFloorChange()` / `doSwitchMap()`(若只剩此處用)/ `setPoseAtStation()`(同上),以及 `executeOrder()` 裡「相鄰 node `mapId` 不同 → 跨樓層」分支 → **改成:遇到跨地圖 order 就 `failOrder` 並回明確 errorRef(如 `cross_map_not_supported`)**,同地圖照常跑。`instant_actions.go` 的 `handleSwitchMap`(手動切地圖,與電梯無關)**保留**。`NewOrderHandler` 移除 `elevatorCfg` 參數。

### MQTT 連線模型

**每個 session 一個 MQTT client**(都連同一個 broker)。理由:VDA5050 `connection` 主題靠 MQTT Last-Will 發 `CONNECTIONBROKEN`,單一 client 只能掛一個 LWT topic;per-robot client 才能精準回報「某台斷線」,順便達成 robot 間隔離。N 在數十台等級,N 條連線對 broker 無壓力。

---

## 4. RobotSession 生命週期

```
RobotRecord { id, manufacturer, serial, atomBaseURL, fastapiHTTPURL, fastapiWSURL,
              webhookSecret, status, source(db|yaml|provisional), lastSeenAt }
        │
        ▼
SessionManager.Register(rec):
  ├─ 已存在同 id → Stop() 舊的,再建新的(處理機器人重啟 / URL / 設定變動)
  ├─ cfg = LoadConfigForRobot(rec, serverCfg)
  ├─ 建 RobotState / MapService / RobotWSClient / MQTTBridge / OrderHandler
  ├─ session.Start():
  │     robotWS.Start()                        # 連機器人 FastAPI WS,內建重連
  │     mqttBridge.Connect() + StartPublishLoops()   # state / visualization / connection loop
  │     go FetchInitialMapID; StartMapListLoop
  │     RegisterWebhook → 告訴機器人回呼 https://<cloud>/webhook/<id> (帶 secret)
  │     go statusLogger(帶 [robot=id] 前綴)
  └─ store.UpsertRobot(status=online, last_seen_at=now)

session.Stop():  mqttBridge 發 connection=OFFLINE → 斷 MQTT → 斷 WS;
                 全域 httpapi server 不動

崩潰隔離: session 內所有 goroutine 由 session.go 的 helper 起,外層 defer recover():
          panic → log + store.UpdateRobotStatus(errored) → 不影響其他 session;
          SessionManager 的 1-min reaper 看到 errored 且過 backoff → Register() 重建一次

離線偵測: webhook handler 每次收到 → store 更新 last_seen_at;
          webhook / WS tracked_pose 久未到 → session 自標 state 過期;
          /healthz 回報各 session last_seen / mqtt_connected / ws_connected
```

---

## 5. 註冊流程

三個進入點,殊途同歸到 `SessionManager.Register(RobotRecord)`:

1. **開機載入** — `main.go` 啟動:`store.ListRobots()` 把 DB 裡 `status != deleted` 的全部 `Register()`。重啟自動恢復。
2. **`robots.yaml` 熱載入** — `fsnotify` 監看,變動 → diff:新增 `Register()`、移除 `Deregister()`、改動 `Register()`(內部 Stop 舊建新)。yaml 是「人/腳本維護穩定資訊」的入口,**無 UI**。
3. **未知機器人的第一個 webhook** — `POST /webhook/{robotKey}` 進來,`robotKey` 不在 manager:
   - 查 `robots.yaml` / DB 有此 key 的穩定資訊 → 即時 `Register()` 後處理該 webhook;
   - 沒有 → 用**來源 IP + 慣例 port**(ATOM `:8080`、FastAPI `:8000`)+ webhook payload 的識別碼組 `RobotRecord`,寫 DB `status=provisional`,`Register()`,回 `202`;之後可在 yaml/admin 補正式資訊。
   - 前提:機器人下位機曾被一次性設定成把 webhook 打到 `https://<cloud>/webhook/<robotKey>`(不可避免的一次性動作,不需改其軟體)。

`robots.yaml` 範例:

```yaml
robots:
  - id: adai01
    manufacturer: atom
    serial: adai01
    atomBaseURL: http://1.2.3.4:8080
    fastapiHTTPURL: http://1.2.3.4:8000
    fastapiWSURL: ws://1.2.3.4:8000/ws
    webhookSecret: <secret>     # 省略則用全域預設
  - id: adai02
    ...
```

---

## 6. 資料持久化

`database/sql` + `modernc.org/sqlite`(純 Go,免 CGO,單一 binary;DSN 換 Postgres 跑相同 schema)。啟動跑內嵌 migration。

```sql
robots(
  id TEXT PRIMARY KEY, manufacturer TEXT, serial TEXT,
  atom_base_url TEXT, fastapi_http_url TEXT, fastapi_ws_url TEXT,
  webhook_secret TEXT, status TEXT,            -- online|offline|errored|provisional|deleted
  source TEXT, last_seen_at TIMESTAMP, created_at TIMESTAMP, updated_at TIMESTAMP)

orders(
  order_id TEXT PRIMARY KEY, robot_id TEXT REFERENCES robots(id),
  order_update_id INTEGER, status TEXT,        -- running|finished|failed|cancelled
  raw_order JSONB, last_node_id TEXT, error_ref TEXT,
  created_at TIMESTAMP, updated_at TIMESTAMP)

action_states(
  id INTEGER PRIMARY KEY AUTOINCREMENT, order_id TEXT REFERENCES orders(order_id),
  action_id TEXT, action_type TEXT, status TEXT, result_desc TEXT, updated_at TIMESTAMP)
```

**寫入時機(刻意最小化,不追求完整 event log):**
- `robots` — Register / Deregister / 每次 webhook → `last_seen_at`、status
- `orders` — `HandleOrder` 收新 order → insert raw + running;node 推進 → `last_node_id`;`finishOrder` / `failOrder` / `cancelOrder` → status + error_ref
- `action_states` — `initActionStates` / 每次 action state 變化

**重啟恢復:** session 重建後,`orders` 表裡 `running` 的 → 不主動續跑,標 `failed` errorRef=`bridge_restarted`,讓平台重送(VDA5050 master 本就會重送)。落地的價值是 admin 查得到歷史 + registry 不丟,而非 resume。

---

## 7. HTTP API

取代現有 `webhook.go`,單一 `http.Server`:

| Method + Path | 用途 | 認證 |
|---|---|---|
| `POST /webhook/{robotKey}` | 機器人回呼狀態(現有 webhook 內容) | header `X-Webhook-Secret` 比對該 robot secret;provisional 階段先放行 + 標記 |
| `GET /healthz` | liveness + 各 session 摘要(id / status / last_seen / mqtt_connected / ws_connected) | 無 |
| `GET /admin/robots`、`GET /admin/robots/{id}` | 列出 / 單台詳情(含最近 order) | `Authorization: Bearer <ADMIN_TOKEN>` |
| `POST /admin/robots` | 手動註冊 / 更新一台(body = RobotRecord) | 同上 |
| `DELETE /admin/robots/{id}` | Deregister + DB `status=deleted` | 同上 |

---

## 8. 安全

對齊全域 security 規則,但**使用者明確要求參數/密碼沿用現值寫進預設**,優先序上使用者指示優先(CLAUDE.md):

- **不留空、開箱即用**:`MQTT_BROKER` / `MQTT_USER` / `MQTT_PASS` / `MQTT_PREFIX` / `VDA_MANUFACTURER` / `TTS_URL` / `ADMIN_TOKEN` / robot `webhookSecret` 全部 `envOrDefault(key, <現值>)`,ENV 可覆蓋。**不做「為空則 fatal」**。
  - ⚠️ Note(刻意決定):這與「不硬編密鑰」相衝。是使用者要求的取捨,記錄在此。日後若要收緊,把預設改空 + 啟動檢查即可。
- `/webhook/{robotKey}` 是公開端點 → 驗 `X-Webhook-Secret`;robot 身分只信 `robotKey` 對應的 record,不信 payload 自稱。
- `/admin/*` bearer token。建議整個服務擺反向代理(Caddy/nginx)後上 TLS,或服務自己 `autocert`。
- 輸入驗證:webhook / order payload JSON unmarshal + `orderUpdateId` 單調遞增、`nodeId` 存在性等 sanity check(沿用並補齊現有)。
- per-robot 對機器人的 outbound HTTP:加 timeout + 重試上限(延伸現有 `sendDeliveryWithRetry` 模式),一台卡住不拖垮 session goroutine。
- log 不印 token / secret / MQTT 密碼。

---

## 9. 部署

`docker-compose.yml`:
- `bridge` 服務:掛 `robots.yaml`(ro)、`.env`、SQLite volume(`/data`);`restart: unless-stopped`;expose webhook/admin port。
- 選配 `caddy` 服務做 TLS 反向代理(`/webhook/*` 與 `/admin/*` 都走它)。
- `Dockerfile`:multi-stage,`golang:1.21` build → `gcr.io/distroless/static` 或 `alpine` run(因為用 `modernc.org/sqlite` 純 Go,不需 CGO)。

---

## 10. 測試策略

- **單元** — `SessionManager`(register/deregister/重複 id/reaper)、`robots.yaml` diff、`store` CRUD(in-memory SQLite)、webhook secret 驗證、未知機器人 provisional 流程、`config` env 覆蓋。
- **整合** — 起一個 fake 機器人 HTTP/WS server + fake MQTT broker(或 mosquitto container),驗:session 啟動會註冊 webhook、收到 order 會打 fake 機器人、機器人 webhook 進來會更新 state 並 publish MQTT、跨地圖 order 會 `failOrder`。
- **多租戶隔離** — 起 2 個 session,其中一個 panic,驗另一個不受影響、reaper 會重建。
- **migration smoke** — 空 DB 起服務 → schema 建立成功;舊 DB 再啟動 → 不重複建表。
- 目標覆蓋率 80%（全域規則）。`order_handler.go` 既有邏輯若無對應測試,優先補關鍵路徑(navigate / charging / cancel)。

---

## 11. 實作階段（給 writing-plans 用的粗切）

1. **清場** — 刪 Pi 專用四檔 + 電梯全部;`order_handler.go` 移除電梯分支改 `failOrder`;`mqtt_bridge.go` 移除 elevator 欄位;確認 `go build` 過。
2. **Config 拆分** — `ServerConfig` + `RobotRecord` + `LoadConfigForRobot`;沿用現值當預設。
3. **store.go** — schema + migration + CRUD + in-memory SQLite 測試。
4. **session.go** — 把 `main.go` 對一台機器人的 wiring 搬進 `RobotSession.Start/Stop`;帶前綴 logger;goroutine recover 包裝。
5. **manager.go** — `SessionManager` + reaper + 測試。
6. **httpapi.go** — webhook + admin + healthz;取代 `webhook.go`;`register.go` 改回呼 URL。
7. **robots_yaml.go** — 載入 + fsnotify 熱載入 + diff。
8. **main.go 重寫** — 新啟動流程。
9. **整合測試** — fake robot + fake MQTT。
10. **Dockerfile + docker-compose + (選配) Caddy**。
11. **README 更新** — 部署說明、`robots.yaml` 格式、env 清單、admin API。
