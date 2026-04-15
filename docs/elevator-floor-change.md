# Pi2W Bridge — 電梯跨樓層導航流程

## 概述

當平台派發的 VDA5050 Order 中，相鄰 Node 的 `mapId` 不同時，Bridge 會自動偵測到「跨樓層」情境，並自主執行完整的電梯搭乘流程。整個過程對平台透明 — 平台只需要在 Node 上標注正確的 `mapId`，不需要知道機器人怎麼搭電梯。

---

## 設定檔 (elevator_config.json)

放在 `/opt/pi2w-bridge/elevator_config.json`（與 pi2w-bridge 同目錄）。

```json
{
  "floors": {
    "20260326_shalun_250C_charge": {
      "floor": 1,
      "elevatorHall": "EV_hall_1F"
    },
    "map_2F": {
      "floor": 2,
      "elevatorHall": "EV_hall_2F"
    },
    "map_3F": {
      "floor": 3,
      "elevatorHall": "EV_hall_3F"
    }
  },
  "elevator": {
    "tunnelMap": "elevator_tunnel",
    "hall": "EV_lobby",
    "cars": {
      "EV01": "EV01_inside",
      "EV02": "EV02_inside"
    }
  }
}
```

### 欄位說明

| 欄位 | 說明 |
|------|------|
| `floors` | key = 機器人上的地圖名稱 (ATOM 的 map name) |
| `floors.*.floor` | 該地圖對應的實體樓層號碼 |
| `floors.*.elevatorHall` | 該樓層電梯梯廳的 ATOM 站點名稱（機器人地圖上的 POI） |
| `elevator.tunnelMap` | 電梯專用的 ATOM 地圖名稱（進電梯後使用的獨立地圖） |
| `elevator.hall` | 電梯地圖 (tunnelMap) 中的梯廳站點名稱 |
| `elevator.cars` | key = 電梯 ID (如 `EV01`)，value = 電梯地圖中該電梯轎廂內的站點名稱 |

---

## 觸發條件

在 `order_handler.go` 的 `executeOrder()` 中，逐一處理 VDA5050 Order 的 Node 時：

```
遍歷 nodes[1..N]:
  取得 node.nodePosition.mapId
  如果 mapId != 上一個 node 的 mapId:
    查詢 elevator_config.json
    如果兩個 mapId 屬於不同 floor:
      → 觸發 handleFloorChange()
```

如果 `elevator_config.json` 不存在，跨樓層功能自動停用，不影響同樓層任務。

---

## 完整電梯流程 (handleFloorChange)

以下以「1F → 2F」為例：

```
目前地圖: 20260326_shalun_250C_charge (1F)
目標地圖: map_2F (2F)
```

### Step 1: 導航到當前樓層的電梯梯廳

```
ATOM API: deliver_to_location → "EV_hall_1F"
等待: robot status = arrived
發送: routing_control = stop (防止 ATOM auto-return)
```

機器人從當前位置移動到 1F 電梯口。

### Step 2: 切換到電梯 Tunnel 地圖

```
ATOM API: /service/parameter/set/map/elevator_tunnel
ATOM API: robot_control = stop_robot_core
等待: 60 秒
ATOM API: robot_control = start_robot_core
等待: 30 秒
ATOM API: select_mode = delivery
```

機器人切換到電梯專用地圖。此時機器人物理位置在電梯口，對應 tunnel 地圖的 `EV_lobby` 站點。

### Step 3: 機器人定位確認

機器人在切換地圖後，應該會在 tunnel 地圖的 `EV_lobby` 位置自動定位（因為物理位置沒變，tunnel 地圖的梯廳位置與實體梯廳位置對齊）。

> 如果需要手動定位，可以用 `SetInitialPose` 設定 tunnel 地圖中 EV_lobby 的座標。

### Step 4: 呼叫電梯到當前樓層 (VDA5050 MQTT)

Bridge 發布 MQTT 訊息到 `{prefix}/elevator/request`：

```json
{
  "action": "call",
  "fromFloor": 1,
  "toFloor": 2,
  "robotId": "adai01",
  "timestamp": "2026-04-14T12:00:00Z"
}
```

平台（或電梯控制系統）收到後呼叫電梯。

### Step 5: 等待電梯到達

Bridge 訂閱 `{prefix}/elevator/response`，等待：

```json
{
  "status": "ready",
  "carId": "EV01",
  "floor": 1
}
```

收到後取得 `carId`（知道是哪台電梯到了）。

超時：5 分鐘。

### Step 6: 進入電梯

根據 `carId` 查詢 `elevator.cars` 對應的站點名稱：

```
EV01 → EV01_inside
```

```
ATOM API: deliver_to_location → "EV01_inside"
等待: robot status = arrived
發送: routing_control = stop
```

機器人從梯廳移動進入電梯轎廂。

### Step 7: 通知電梯前往目標樓層

Bridge 發布：

```json
{
  "action": "go",
  "fromFloor": 1,
  "toFloor": 2,
  "robotId": "adai01",
  "timestamp": "2026-04-14T12:00:30Z"
}
```

電梯開始移動。

### Step 8: 等待電梯到達目標樓層

同 Step 5，等待 `{prefix}/elevator/response`：

```json
{
  "status": "ready",
  "carId": "EV01",
  "floor": 2
}
```

### Step 9: 離開電梯

```
ATOM API: deliver_to_location → "EV_lobby"
等待: robot status = arrived
發送: routing_control = stop
```

機器人從電梯轎廂移動到 tunnel 地圖的梯廳位置。

### Step 10: 切換到目標樓層地圖

```
ATOM API: /service/parameter/set/map/map_2F
ATOM API: robot_control = stop_robot_core
等待: 60 秒
ATOM API: robot_control = start_robot_core
等待: 30 秒
ATOM API: select_mode = delivery
```

### Step 11: 目標樓層定位

機器人在 2F 地圖上，物理位置在 2F 電梯口（`EV_hall_2F`）。
切換地圖後自動定位，或用 `SetInitialPose` 手動定位。

### Step 12: 通知完成

Bridge 發布：

```json
{
  "action": "done",
  "fromFloor": 1,
  "toFloor": 2,
  "robotId": "adai01",
  "timestamp": "2026-04-14T12:02:00Z"
}
```

通知電梯系統機器人已離開電梯。

### Step 13: 繼續執行 Order

跨樓層完成後，`executeOrder()` 繼續處理下一個 Node（已在目標樓層的地圖上），正常發送 `deliver_to_location` 導航到任務站點。

---

## 時序圖

```
Platform         Bridge              ATOM Robot        Elevator System
   |                |                    |                    |
   |  VDA5050 Order |                    |                    |
   |  (nodes with   |                    |                    |
   |   different     |                    |                    |
   |   mapIds)       |                    |                    |
   |--------------->|                    |                    |
   |                |                    |                    |
   |                | detect mapId       |                    |
   |                | change: 1F→2F      |                    |
   |                |                    |                    |
   |                |-- deliver_to ----->| (go to EV_hall_1F) |
   |                |                    |                    |
   |                |<-- arrived --------|                    |
   |                |                    |                    |
   |                |-- switchMap ------>| (elevator_tunnel)  |
   |                |   (90s restart)    |                    |
   |                |                    |                    |
   |                |-- MQTT call ------>|-------------------->|
   |                |  elevator/request  |  call to floor 1   |
   |                |                    |                    |
   |                |<----------------------------------------|
   |                |  elevator/response |  ready, EV01       |
   |                |                    |                    |
   |                |-- deliver_to ----->| (go to EV01_inside)|
   |                |<-- arrived --------|                    |
   |                |                    |                    |
   |                |-- MQTT go -------->|-------------------->|
   |                |  elevator/request  |  go to floor 2     |
   |                |                    |                    |
   |                |<----------------------------------------|
   |                |  elevator/response |  ready at floor 2  |
   |                |                    |                    |
   |                |-- deliver_to ----->| (go to EV_lobby)   |
   |                |<-- arrived --------|                    |
   |                |                    |                    |
   |                |-- switchMap ------>| (map_2F)           |
   |                |   (90s restart)    |                    |
   |                |                    |                    |
   |                |-- MQTT done ------>|-------------------->|
   |                |  (robot exited)    |                    |
   |                |                    |                    |
   |                | continue order...  |                    |
   |                |-- deliver_to ----->| (next waypoint)    |
```

---

## MQTT Topic 格式

### 電梯請求 (Bridge → Platform/Elevator)

**Topic**: `{prefix}/elevator/request`

```json
{
  "action": "call" | "go" | "done",
  "fromFloor": 1,
  "toFloor": 2,
  "robotId": "adai01",
  "timestamp": "2026-04-14T12:00:00Z"
}
```

| action | 說明 |
|--------|------|
| `call` | 呼叫電梯到 fromFloor（機器人準備進入） |
| `go`   | 機器人已在電梯內，請電梯前往 toFloor |
| `done` | 機器人已離開電梯，電梯可自由使用 |

### 電梯回應 (Platform/Elevator → Bridge)

**Topic**: `{prefix}/elevator/response`

```json
{
  "status": "ready",
  "carId": "EV01",
  "floor": 1
}
```

| status | 說明 |
|--------|------|
| `ready` | 電梯已到達指定樓層，門已開啟，機器人可以進出 |

---

## VDA5050 Order 範例

平台下發跨樓層任務的 Order 格式：

```json
{
  "orderId": "cross-floor-001",
  "orderUpdateId": 0,
  "nodes": [
    {
      "nodeId": "origin",
      "sequenceId": 0,
      "released": true,
      "nodePosition": { "x": 7.33, "y": -3.93, "mapId": "20260326_shalun_250C_charge" },
      "actions": []
    },
    {
      "nodeId": "wp1_1F",
      "sequenceId": 2,
      "released": true,
      "nodePosition": { "x": 5.0, "y": -2.0, "mapId": "20260326_shalun_250C_charge" },
      "actions": [
        {
          "actionId": "goto-wp1",
          "actionType": "GoToLocation",
          "blockingType": "HARD",
          "actionParameters": [
            { "key": "stationName", "value": "A01" }
          ]
        }
      ]
    },
    {
      "nodeId": "wp2_2F",
      "sequenceId": 4,
      "released": true,
      "nodePosition": { "x": 10.0, "y": -5.0, "mapId": "map_2F" },
      "actions": [
        {
          "actionId": "goto-wp2",
          "actionType": "GoToLocation",
          "blockingType": "HARD",
          "actionParameters": [
            { "key": "stationName", "value": "B01" }
          ]
        }
      ]
    }
  ],
  "edges": [
    {
      "edgeId": "e1",
      "sequenceId": 1,
      "released": true,
      "startNodeId": "origin",
      "endNodeId": "wp1_1F",
      "actions": []
    },
    {
      "edgeId": "e2",
      "sequenceId": 3,
      "released": true,
      "startNodeId": "wp1_1F",
      "endNodeId": "wp2_2F",
      "actions": []
    }
  ]
}
```

在這個範例中：
1. `origin` 和 `wp1_1F` 都在 `20260326_shalun_250C_charge` (1F) → 正常導航
2. `wp1_1F` → `wp2_2F` 跨了 mapId (`map_2F`) → **觸發電梯流程**
3. 電梯流程完成後，在 2F 繼續執行 `GoToLocation: B01`

---

## 錯誤處理

| 情境 | 處理方式 |
|------|---------|
| `elevator_config.json` 不存在 | 跨樓層功能停用，同樓層任務正常執行 |
| `elevator_config.json` 格式錯誤 | Bridge 啟動失敗 (fatal) |
| mapId 不在 floors 設定中 | 忽略跨樓層偵測，視為同樓層 |
| 導航到梯廳失敗 | Order 標記為 FAILED |
| switchMap 失敗 | Order 標記為 FAILED |
| 等待電梯超時 (5 min) | Order 標記為 FAILED |
| 進/出電梯導航失敗 | Order 標記為 FAILED |
| Order 被取消 (cancelOrder) | 中斷電梯流程，發送 stop 停止機器人 |

---

## 時間估算

| 步驟 | 預估時間 |
|------|---------|
| 導航到梯廳 | 依距離而定 (10s~60s) |
| switchMap (tunnel) | ~90s (stop/start robot core) |
| 等待電梯 | 依電梯系統 (10s~300s) |
| 進入電梯 | ~10s |
| 電梯移動 | 依樓層距離 (10s~60s) |
| 離開電梯 | ~10s |
| switchMap (target floor) | ~90s |
| **總計** | **~3~8 分鐘** |

---

## 檔案變更清單

| 檔案 | 變更 |
|------|------|
| `elevator_config.go` | **新增** — 設定檔 struct 和載入邏輯 |
| `elevator_config.json.example` | **新增** — 設定檔範例 |
| `order_handler.go` | **修改** — 加入 `elevatorCfg`、`robotWS` 欄位；`executeOrder` 加入 mapId 跨樓層偵測；新增 `handleFloorChange()`、`doSwitchMap()`、`publishElevatorRequest()`、`waitForElevatorReady()` |
| `mqtt_bridge.go` | **修改** — `MQTTBridge` 加入 `elevatorCfg`；`NewMQTTBridge` 和 `NewOrderHandler` 簽名更新；新增 `makeElevatorCallback()` |
| `main.go` | **修改** — 載入 `elevator_config.json`，傳入 `NewMQTTBridge` |

---

## 未來可擴展

1. **SetInitialPose 定位**：在 Step 3 和 Step 11 加入精確的 `SetInitialPose` 座標定位（需要知道各梯廳在各地圖中的座標）
2. **多電梯排隊**：目前取第一個回應的電梯，未來可加入多電梯調度邏輯
3. **電梯故障重試**：目前超時即失敗，可加入重試或換電梯邏輯
4. **返程跨樓層**：任務完成後的 `return to counter` 如果涉及跨樓層，需要額外處理
