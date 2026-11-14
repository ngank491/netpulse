package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"
)

var ErrNotFound = errors.New("không tìm thấy")

func init() {
	// Per-connection pragmas: every pooled connection gets the same safety settings.
	sqlite.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, _ string) error {
		for _, p := range []string{"PRAGMA busy_timeout=10000", "PRAGMA foreign_keys=ON", "PRAGMA synchronous=NORMAL"} {
			if _, err := conn.ExecContext(context.Background(), p, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

const schemaV1 = `
CREATE TABLE kv(k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE users(
  id INTEGER PRIMARY KEY,
  username TEXT NOT NULL UNIQUE COLLATE NOCASE,
  pass TEXT NOT NULL,
  role TEXT NOT NULL CHECK(role IN ('admin','viewer')),
  disabled INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL);
CREATE TABLE sessions(
  token TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at INTEGER NOT NULL);
CREATE TABLE routers(
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  mode TEXT NOT NULL,
  host TEXT NOT NULL DEFAULT '',
  port INTEGER NOT NULL DEFAULT 0,
  tls INTEGER NOT NULL DEFAULT 1,
  verify_tls INTEGER NOT NULL DEFAULT 0,
  username TEXT NOT NULL DEFAULT '',
  secret TEXT NOT NULL DEFAULT '',
  push_token TEXT NOT NULL DEFAULT '',
  interval_s INTEGER NOT NULL DEFAULT 15,
  wans TEXT NOT NULL DEFAULT '[]',
  wan_target TEXT NOT NULL DEFAULT '8.8.8.8',
  chat_id TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  map_x REAL, map_y REAL,
  created_at INTEGER NOT NULL);
CREATE TABLE user_routers(
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  router_id INTEGER NOT NULL REFERENCES routers(id) ON DELETE CASCADE,
  PRIMARY KEY(user_id, router_id));
CREATE TABLE monitors(
  id INTEGER PRIMARY KEY,
  router_id INTEGER NOT NULL REFERENCES routers(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  ip TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL DEFAULT 'device',
  parent_id INTEGER,
  enabled INTEGER NOT NULL DEFAULT 1,
  notify INTEGER NOT NULL DEFAULT 1,
  repeat_min INTEGER NOT NULL DEFAULT 0,
  x REAL, y REAL);
CREATE INDEX monitors_router ON monitors(router_id);
CREATE TABLE events(
  id INTEGER PRIMARY KEY,
  router_id INTEGER NOT NULL,
  at INTEGER NOT NULL,
  level TEXT NOT NULL,
  msg TEXT NOT NULL);
CREATE INDEX events_router_at ON events(router_id, at);
CREATE INDEX events_at ON events(at);
CREATE TABLE samples(
  router_id INTEGER NOT NULL,
  at INTEGER NOT NULL,
  cpu REAL, mem REAL,
  data TEXT NOT NULL,
  PRIMARY KEY(router_id, at)) WITHOUT ROWID;
CREATE TABLE probe_hourly(
  monitor_id INTEGER NOT NULL,
  hour INTEGER NOT NULL,
  ok INTEGER NOT NULL,
  total INTEGER NOT NULL,
  PRIMARY KEY(monitor_id, hour)) WITHOUT ROWID;
CREATE TABLE outbox(
  id INTEGER PRIMARY KEY,
  chat_id TEXT NOT NULL,
  text TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  next_at INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0);
CREATE TABLE state(router_id INTEGER PRIMARY KEY, data TEXT NOT NULL);
`

// schemaV2: Zalo channel, client presence tracking and client aliases.
const schemaV2 = `
ALTER TABLE routers ADD COLUMN zalo_chat TEXT NOT NULL DEFAULT '';
ALTER TABLE outbox ADD COLUMN channel TEXT NOT NULL DEFAULT 'telegram';
CREATE TABLE clients(
  router_id INTEGER NOT NULL REFERENCES routers(id) ON DELETE CASCADE,
  mac TEXT NOT NULL,
  data TEXT NOT NULL,
  last_seen INTEGER NOT NULL,
  PRIMARY KEY(router_id, mac)) WITHOUT ROWID;
CREATE TABLE client_alias(
  router_id INTEGER NOT NULL REFERENCES routers(id) ON DELETE CASCADE,
  mac TEXT NOT NULL,
  name TEXT NOT NULL,
  PRIMARY KEY(router_id, mac)) WITHOUT ROWID;
`

type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("mở database: %w", err)
	}
	s := &Store{db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return s.db.Close()
}

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	migrations := []string{schemaV1, schemaV2}
	if v > len(migrations) {
		return fmt.Errorf("database được tạo bởi phiên bản mới hơn (schema %d)", v)
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func nowMs() int64 { return time.Now().UnixMilli() }

// ---------- key/value ----------

func (s *Store) Get(key string) (string, error) {
	var v string
	err := s.db.QueryRow("SELECT v FROM kv WHERE k=?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) Set(key, v string) error {
	_, err := s.db.Exec("INSERT INTO kv(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v", key, v)
	return err
}

func (s *Store) GetJSON(key string, dst any) error {
	v, err := s.Get(key)
	if err != nil || v == "" {
		return err
	}
	return json.Unmarshal([]byte(v), dst)
}

func (s *Store) SetJSON(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.Set(key, string(b))
}

// ---------- users & sessions ----------

type User struct {
	ID       int64   `json:"id"`
	Username string  `json:"username"`
	Role     string  `json:"role"`
	Disabled bool    `json:"disabled"`
	Routers  []int64 `json:"routers"`
	passHash string
}

func (u *User) Admin() bool { return u.Role == "admin" }

func (u *User) CanSee(routerID int64) bool {
	if u.Admin() {
		return true
	}
	for _, id := range u.Routers {
		if id == routerID {
			return true
		}
	}
	return false
}

func (s *Store) CountUsers() (n int, err error) {
	err = s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n)
	return
}

func (s *Store) CountAdmins() (n int, err error) {
	err = s.db.QueryRow("SELECT COUNT(*) FROM users WHERE role='admin' AND disabled=0").Scan(&n)
	return
}

func (s *Store) CreateUser(name, hash, role string) (int64, error) {
	r, err := s.db.Exec("INSERT INTO users(username,pass,role,created_at) VALUES(?,?,?,?)", name, hash, role, nowMs())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, errors.New("tên đăng nhập đã tồn tại")
		}
		return 0, err
	}
	return r.LastInsertId()
}

func (s *Store) scanUser(row *sql.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.passHash, &u.Role, &u.Disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.Routers, err = s.UserRouters(u.ID)
	return &u, err
}

func (s *Store) UserByName(name string) (*User, error) {
	return s.scanUser(s.db.QueryRow("SELECT id,username,pass,role,disabled FROM users WHERE username=?", name))
}

func (s *Store) UserByID(id int64) (*User, error) {
	return s.scanUser(s.db.QueryRow("SELECT id,username,pass,role,disabled FROM users WHERE id=?", id))
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query("SELECT id,username,role,disabled FROM users ORDER BY username")
	if err != nil {
		return nil, err
	}
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.Disabled); err != nil {
			rows.Close()
			return nil, err
		}
		u.Routers = []int64{}
		out = append(out, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	links, err := s.db.Query("SELECT user_id,router_id FROM user_routers ORDER BY router_id")
	if err != nil {
		return nil, err
	}
	defer links.Close()
	for links.Next() {
		var uid, rid int64
		if err := links.Scan(&uid, &rid); err != nil {
			return nil, err
		}
		for i := range out {
			if out[i].ID == uid {
				out[i].Routers = append(out[i].Routers, rid)
			}
		}
	}
	return out, links.Err()
}

func (s *Store) UserRouters(id int64) ([]int64, error) {
	rows, err := s.db.Query("SELECT router_id FROM user_routers WHERE user_id=? ORDER BY router_id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var r int64
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) UpdateUser(id int64, role string, disabled bool, routers []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE users SET role=?, disabled=? WHERE id=?", role, disabled, id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM user_routers WHERE user_id=?", id); err != nil {
		return err
	}
	for _, r := range routers {
		if _, err := tx.Exec("INSERT OR IGNORE INTO user_routers(user_id,router_id) SELECT ?,id FROM routers WHERE id=?", id, r); err != nil {
			return err
		}
	}
	if disabled {
		if _, err := tx.Exec("DELETE FROM sessions WHERE user_id=?", id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SetPassword(id int64, hash string) error {
	_, err := s.db.Exec("UPDATE users SET pass=? WHERE id=?", hash, id)
	return err
}

func (s *Store) DeleteUser(id int64) error {
	_, err := s.db.Exec("DELETE FROM users WHERE id=?", id)
	return err
}

func (s *Store) CreateSession(tokenHash string, uid, exp int64) error {
	_, err := s.db.Exec("INSERT INTO sessions(token,user_id,expires_at) VALUES(?,?,?)", tokenHash, uid, exp)
	return err
}

func (s *Store) SessionUser(tokenHash string) (*User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT u.id,u.username,u.pass,u.role,u.disabled FROM sessions s
		JOIN users u ON u.id=s.user_id WHERE s.token=? AND s.expires_at>? AND u.disabled=0`, tokenHash, nowMs()))
}

func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE token=?", tokenHash)
	return err
}

func (s *Store) DeleteUserSessions(uid int64, exceptHash string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE user_id=? AND token<>?", uid, exceptHash)
	return err
}

// ---------- routers ----------

type WAN struct {
	Iface string `json:"iface"`
	Label string `json:"label"`
}

type Router struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	Mode      string   `json:"mode"` // pull | push | demo
	Host      string   `json:"host"`
	Port      int      `json:"port"`
	TLS       bool     `json:"tls"`
	VerifyTLS bool     `json:"verifyTls"`
	Username  string   `json:"username"`
	Interval  int      `json:"interval"`
	WANs      []WAN    `json:"wans"`
	WANTarget string   `json:"wanTarget"`
	ChatID    string   `json:"chatId"`
	ZaloChat  string   `json:"zaloChat"`
	Enabled   bool     `json:"enabled"`
	MapX      *float64 `json:"mapX"`
	MapY      *float64 `json:"mapY"`

	HasPassword bool `json:"hasPassword"`
	PushPaired  bool `json:"pushPaired"`
	secret      string
	pushToken   string
}

const routerCols = "id,name,mode,host,port,tls,verify_tls,username,secret,push_token,interval_s,wans,wan_target,chat_id,enabled,map_x,map_y,zalo_chat"

type scanner interface{ Scan(...any) error }

func scanRouter(sc scanner) (*Router, error) {
	var r Router
	var wans string
	var mx, my sql.NullFloat64
	err := sc.Scan(&r.ID, &r.Name, &r.Mode, &r.Host, &r.Port, &r.TLS, &r.VerifyTLS, &r.Username, &r.secret,
		&r.pushToken, &r.Interval, &wans, &r.WANTarget, &r.ChatID, &r.Enabled, &mx, &my, &r.ZaloChat)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if json.Unmarshal([]byte(wans), &r.WANs) != nil || r.WANs == nil {
		r.WANs = []WAN{}
	}
	if mx.Valid && my.Valid {
		r.MapX, r.MapY = &mx.Float64, &my.Float64
	}
	r.HasPassword = r.secret != ""
	r.PushPaired = r.pushToken != ""
	return &r, nil
}

func (s *Store) ListRouters() ([]*Router, error) {
	rows, err := s.db.Query("SELECT " + routerCols + " FROM routers ORDER BY name COLLATE NOCASE, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Router
	for rows.Next() {
		r, err := scanRouter(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetRouter(id int64) (*Router, error) {
	return scanRouter(s.db.QueryRow("SELECT "+routerCols+" FROM routers WHERE id=?", id))
}

func (s *Store) SaveRouter(r *Router) error {
	wans, _ := json.Marshal(r.WANs)
	if r.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO routers(name,mode,host,port,tls,verify_tls,username,secret,interval_s,wans,wan_target,chat_id,enabled,created_at,zalo_chat)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.Name, r.Mode, r.Host, r.Port, r.TLS, r.VerifyTLS, r.Username, r.secret,
			r.Interval, string(wans), r.WANTarget, r.ChatID, r.Enabled, nowMs(), r.ZaloChat)
		if err != nil {
			return err
		}
		r.ID, err = res.LastInsertId()
		return err
	}
	_, err := s.db.Exec(`UPDATE routers SET name=?,mode=?,host=?,port=?,tls=?,verify_tls=?,username=?,secret=?,interval_s=?,wans=?,
		wan_target=?,chat_id=?,enabled=?,zalo_chat=? WHERE id=?`, r.Name, r.Mode, r.Host, r.Port, r.TLS, r.VerifyTLS, r.Username, r.secret,
		r.Interval, string(wans), r.WANTarget, r.ChatID, r.Enabled, r.ZaloChat, r.ID)
	return err
}

func (s *Store) SetPushToken(id int64, hash string) error {
	_, err := s.db.Exec("UPDATE routers SET push_token=? WHERE id=?", hash, id)
	return err
}

func (s *Store) DeleteRouter(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		"DELETE FROM probe_hourly WHERE monitor_id IN (SELECT id FROM monitors WHERE router_id=?)",
		"DELETE FROM samples WHERE router_id=?",
		"DELETE FROM events WHERE router_id=?",
		"DELETE FROM state WHERE router_id=?",
		"DELETE FROM routers WHERE id=?",
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------- monitors (ping targets + topology nodes) ----------

type Monitor struct {
	ID        int64    `json:"id"`
	RouterID  int64    `json:"routerId"`
	Name      string   `json:"name"`
	IP        string   `json:"ip"`
	Kind      string   `json:"kind"`
	ParentID  *int64   `json:"parentId"`
	Enabled   bool     `json:"enabled"`
	Notify    bool     `json:"notify"`
	RepeatMin int      `json:"repeatMin"`
	X         *float64 `json:"x"`
	Y         *float64 `json:"y"`
}

// Pingable reports whether the router should probe this node.
func (m *Monitor) Pingable() bool { return m.Enabled && m.IP != "" && m.Kind != "virtual" }

const monitorCols = "id,router_id,name,ip,kind,parent_id,enabled,notify,repeat_min,x,y"

func scanMonitor(sc scanner) (*Monitor, error) {
	var m Monitor
	var parent sql.NullInt64
	var x, y sql.NullFloat64
	err := sc.Scan(&m.ID, &m.RouterID, &m.Name, &m.IP, &m.Kind, &parent, &m.Enabled, &m.Notify, &m.RepeatMin, &x, &y)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if parent.Valid {
		m.ParentID = &parent.Int64
	}
	if x.Valid && y.Valid {
		m.X, m.Y = &x.Float64, &y.Float64
	}
	return &m, nil
}

func (s *Store) ListMonitors(routerID int64) ([]Monitor, error) {
	rows, err := s.db.Query("SELECT "+monitorCols+" FROM monitors WHERE router_id=? ORDER BY name COLLATE NOCASE, id", routerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Monitor{}
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *Store) GetMonitor(id int64) (*Monitor, error) {
	return scanMonitor(s.db.QueryRow("SELECT "+monitorCols+" FROM monitors WHERE id=?", id))
}

func (s *Store) SaveMonitor(m *Monitor) error {
	if m.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO monitors(router_id,name,ip,kind,parent_id,enabled,notify,repeat_min,x,y) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			m.RouterID, m.Name, m.IP, m.Kind, m.ParentID, m.Enabled, m.Notify, m.RepeatMin, m.X, m.Y)
		if err != nil {
			return err
		}
		m.ID, err = res.LastInsertId()
		return err
	}
	_, err := s.db.Exec(`UPDATE monitors SET name=?,ip=?,kind=?,parent_id=?,enabled=?,notify=?,repeat_min=? WHERE id=?`,
		m.Name, m.IP, m.Kind, m.ParentID, m.Enabled, m.Notify, m.RepeatMin, m.ID)
	return err
}

// DeleteMonitor removes a node and re-attaches its children to its own parent.
func (s *Store) DeleteMonitor(m *Monitor) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE monitors SET parent_id=? WHERE parent_id=?", m.ParentID, m.ID); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM probe_hourly WHERE monitor_id=?", m.ID); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM monitors WHERE id=?", m.ID); err != nil {
		return err
	}
	return tx.Commit()
}

type NodePos struct {
	ID int64   `json:"id"` // 0 = the router itself
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
}

func (s *Store) SaveLayout(routerID int64, nodes []NodePos) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, n := range nodes {
		if n.ID == 0 {
			_, err = tx.Exec("UPDATE routers SET map_x=?,map_y=? WHERE id=?", n.X, n.Y, routerID)
		} else {
			_, err = tx.Exec("UPDATE monitors SET x=?,y=? WHERE id=? AND router_id=?", n.X, n.Y, n.ID, routerID)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------- events ----------

type Event struct {
	ID       int64  `json:"id"`
	RouterID int64  `json:"routerId"`
	At       int64  `json:"at"`
	Level    string `json:"level"`
	Msg      string `json:"msg"`
}

func (s *Store) AddEvent(routerID int64, at int64, level, msg string) error {
	_, err := s.db.Exec("INSERT INTO events(router_id,at,level,msg) VALUES(?,?,?,?)", routerID, at, level, msg)
	return err
}

// ListEvents returns newest first. routerIDs == nil means all routers.
func (s *Store) ListEvents(routerIDs []int64, limit int) ([]Event, error) {
	q := "SELECT id,router_id,at,level,msg FROM events"
	args := []any{}
	if routerIDs != nil {
		if len(routerIDs) == 0 {
			return []Event{}, nil
		}
		q += " WHERE router_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(routerIDs)), ",") + ")"
		for _, id := range routerIDs {
			args = append(args, id)
		}
	}
	q += " ORDER BY at DESC, id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.RouterID, &e.At, &e.Level, &e.Msg); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) CountEventsSince(routerID, since int64, level string) (n int, err error) {
	err = s.db.QueryRow("SELECT COUNT(*) FROM events WHERE router_id=? AND at>=? AND level=?", routerID, since, level).Scan(&n)
	return
}

// ---------- history ----------

type Sample struct {
	At   int64
	CPU  sql.NullFloat64
	Mem  sql.NullFloat64
	Data string
}

func (s *Store) AddSample(routerID, at int64, cpu, mem *float64, data []byte) error {
	_, err := s.db.Exec(`INSERT INTO samples(router_id,at,cpu,mem,data) VALUES(?,?,?,?,?)
		ON CONFLICT(router_id,at) DO UPDATE SET cpu=excluded.cpu,mem=excluded.mem,data=excluded.data`, routerID, at, cpu, mem, string(data))
	return err
}

func (s *Store) Samples(routerID, since int64) ([]Sample, error) {
	rows, err := s.db.Query("SELECT at,cpu,mem,data FROM samples WHERE router_id=? AND at>=? ORDER BY at", routerID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var x Sample
		if err := rows.Scan(&x.At, &x.CPU, &x.Mem, &x.Data); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) AddProbe(monitorID int64, at int64, ok bool) error {
	hour := at / 3600000 * 3600000
	okN := 0
	if ok {
		okN = 1
	}
	_, err := s.db.Exec(`INSERT INTO probe_hourly(monitor_id,hour,ok,total) VALUES(?,?,?,1)
		ON CONFLICT(monitor_id,hour) DO UPDATE SET ok=ok+excluded.ok,total=total+1`, monitorID, hour, okN)
	return err
}

// Availability returns the share of successful probes per monitor since a time.
func (s *Store) Availability(routerID, since int64) (map[int64]float64, error) {
	rows, err := s.db.Query(`SELECT p.monitor_id, SUM(p.ok)*1.0/SUM(p.total) FROM probe_hourly p
		JOIN monitors m ON m.id=p.monitor_id WHERE m.router_id=? AND p.hour>=? GROUP BY p.monitor_id`, routerID, since/3600000*3600000)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]float64{}
	for rows.Next() {
		var id int64
		var v float64
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = v
	}
	return out, rows.Err()
}

// ---------- Telegram outbox ----------

type OutMsg struct {
	ID       int64
	Channel  string
	Chat     string
	Text     string
	Created  int64
	Attempts int
}

func (s *Store) Enqueue(channel, chat, text string) error {
	t := nowMs()
	_, err := s.db.Exec("INSERT INTO outbox(channel,chat_id,text,created_at,next_at) VALUES(?,?,?,?,?)", channel, chat, text, t, t)
	return err
}

func (s *Store) DueOutbox(now int64, limit int) ([]OutMsg, error) {
	rows, err := s.db.Query("SELECT id,channel,chat_id,text,created_at,attempts FROM outbox WHERE next_at<=? ORDER BY id LIMIT ?", now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutMsg
	for rows.Next() {
		var m OutMsg
		if err := rows.Scan(&m.ID, &m.Channel, &m.Chat, &m.Text, &m.Created, &m.Attempts); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func idList(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

func (s *Store) DeleteOutbox(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	ph, args := idList(ids)
	_, err := s.db.Exec("DELETE FROM outbox WHERE id IN ("+ph+")", args...)
	return err
}

func (s *Store) RetryOutbox(ids []int64, next int64) error {
	if len(ids) == 0 {
		return nil
	}
	ph, args := idList(ids)
	_, err := s.db.Exec("UPDATE outbox SET attempts=attempts+1, next_at=? WHERE id IN ("+ph+")", append([]any{next}, args...)...)
	return err
}

func (s *Store) DropOutboxBefore(t int64) (int64, error) {
	r, err := s.db.Exec("DELETE FROM outbox WHERE created_at<?", t)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// ---------- persisted runtime state ----------

func (s *Store) LoadState(routerID int64) ([]byte, error) {
	var v string
	err := s.db.QueryRow("SELECT data FROM state WHERE router_id=?", routerID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return []byte(v), err
}

func (s *Store) SaveState(routerID int64, data []byte) error {
	_, err := s.db.Exec("INSERT INTO state(router_id,data) VALUES(?,?) ON CONFLICT(router_id) DO UPDATE SET data=excluded.data", routerID, string(data))
	return err
}

// ---------- maintenance ----------

func (s *Store) Purge(before int64) error {
	for _, q := range []string{
		"DELETE FROM samples WHERE at<?",
		"DELETE FROM events WHERE at<?",
		"DELETE FROM probe_hourly WHERE hour<?",
		"DELETE FROM sessions WHERE expires_at<?",
		"DELETE FROM clients WHERE last_seen<?",
	} {
		arg := before
		if strings.Contains(q, "sessions") {
			arg = nowMs()
		}
		if _, err := s.db.Exec(q, arg); err != nil {
			return err
		}
	}
	_, err := s.db.Exec("PRAGMA optimize")
	return err
}

// Backup writes a consistent copy of the database to path.
func (s *Store) Backup(path string) error {
	_, err := s.db.Exec("VACUUM INTO ?", path)
	return err
}

// ---------- clients (presence) & aliases ----------

func (s *Store) LoadClients(routerID int64) ([][]byte, error) {
	rows, err := s.db.Query("SELECT data FROM clients WHERE router_id=?", routerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, []byte(d))
	}
	return out, rows.Err()
}

type clientRow struct {
	mac      string
	data     []byte
	lastSeen int64
}

func (s *Store) SaveClients(routerID int64, rows []clientRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`INSERT INTO clients(router_id,mac,data,last_seen) VALUES(?,?,?,?)
		ON CONFLICT(router_id,mac) DO UPDATE SET data=excluded.data,last_seen=excluded.last_seen`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, r := range rows {
		if _, err := st.Exec(routerID, r.mac, string(r.data), r.lastSeen); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) LoadAliases(routerID int64) (map[string]string, error) {
	rows, err := s.db.Query("SELECT mac,name FROM client_alias WHERE router_id=?", routerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var m, n string
		if err := rows.Scan(&m, &n); err != nil {
			return nil, err
		}
		out[m] = n
	}
	return out, rows.Err()
}

func (s *Store) SetAlias(routerID int64, mac, name string) error {
	if name == "" {
		_, err := s.db.Exec("DELETE FROM client_alias WHERE router_id=? AND mac=?", routerID, mac)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO client_alias(router_id,mac,name) VALUES(?,?,?)
		ON CONFLICT(router_id,mac) DO UPDATE SET name=excluded.name`, routerID, mac, name)
	return err
}
