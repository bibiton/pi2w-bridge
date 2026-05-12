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
  status TEXT,
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
func OpenStore(dsn string) (*Store, error) {
	if strings.HasPrefix(dsn, "postgres://") {
		return nil, fmt.Errorf("postgres DSN not yet supported")
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func storeNow() int64 { return time.Now().Unix() }

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
		r.WebhookSecret, storeNZ(r.Status, "offline"), storeNZ(r.Source, "db"), r.LastSeenAt, storeNow(), storeNow())
	return err
}

func storeNZ(s, def string) string {
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

func (s *Store) ListRobots() ([]RobotRecord, error) { return s.queryRobots("") }
func (s *Store) ListActiveRobots() ([]RobotRecord, error) {
	return s.queryRobots("WHERE status != 'deleted'")
}

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
	_, err := s.db.Exec(`UPDATE robots SET status=?, last_seen_at=?, updated_at=? WHERE id=?`, status, t.Unix(), storeNow(), id)
	return err
}

func (s *Store) SetRobotStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE robots SET status=?, updated_at=? WHERE id=?`, status, storeNow(), id)
	return err
}

// --- orders ---

func (s *Store) InsertOrder(orderID, robotID string, updateID int, raw []byte) error {
	_, err := s.db.Exec(`
INSERT INTO orders (order_id,robot_id,order_update_id,status,raw_order,last_node_id,error_ref,created_at,updated_at)
VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT(order_id) DO UPDATE SET order_update_id=excluded.order_update_id, raw_order=excluded.raw_order, status='running', error_ref='', updated_at=excluded.updated_at`,
		orderID, robotID, updateID, "running", string(raw), "", "", storeNow(), storeNow())
	return err
}

func (s *Store) UpdateOrderNode(orderID, nodeID string) error {
	_, err := s.db.Exec(`UPDATE orders SET last_node_id=?, updated_at=? WHERE order_id=?`, nodeID, storeNow(), orderID)
	return err
}

func (s *Store) FinishOrder(orderID, status, errorRef string) error {
	_, err := s.db.Exec(`UPDATE orders SET status=?, error_ref=?, updated_at=? WHERE order_id=?`, status, errorRef, storeNow(), orderID)
	return err
}

func (s *Store) FailRunningOrders(errorRef string) (int64, error) {
	res, err := s.db.Exec(`UPDATE orders SET status='failed', error_ref=?, updated_at=? WHERE status='running'`, errorRef, storeNow())
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
		orderID, actionID, actionType, status, resultDesc, storeNow())
	return err
}
