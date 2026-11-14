package app

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

const cookieName = "np_session"
const sessionTTL = 7 * 24 * time.Hour

type Server struct {
	cfg      *Config
	st       *Store
	box      *Box
	hub      *Hub
	n        *Notifier
	settings *SettingsStore
	lim      *limiter
	pwSem    chan struct{} // bounds concurrent PBKDF2 work

	setupMu   sync.Mutex
	setupCode string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /api/push/{id}", s.push)
	mux.HandleFunc("GET /api/setup", s.setupStatus)
	mux.HandleFunc("POST /api/setup", s.setup)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)

	user := func(p string, h func(http.ResponseWriter, *http.Request, *User)) {
		mux.HandleFunc(p, s.authed(false, h))
	}
	admin := func(p string, h func(http.ResponseWriter, *http.Request, *User)) {
		mux.HandleFunc(p, s.authed(true, h))
	}

	user("GET /api/me", s.me)
	user("POST /api/me/password", s.changePassword)
	user("GET /api/overview", s.overview)
	user("GET /api/routers/{id}", s.routerDetail)
	user("GET /api/routers/{id}/history", s.history)
	user("GET /api/events", s.events)

	admin("GET /api/admin/routers", s.adminRouters)
	admin("POST /api/admin/routers", s.saveRouter)
	admin("PUT /api/admin/routers/{id}", s.saveRouter)
	admin("DELETE /api/admin/routers/{id}", s.deleteRouter)
	admin("POST /api/admin/routers/test", s.testRouter)
	admin("POST /api/admin/routers/{id}/push-script", s.pushScript)
	admin("POST /api/admin/routers/{id}/monitors", s.saveMonitor)
	admin("PUT /api/admin/monitors/{mid}", s.saveMonitor)
	admin("DELETE /api/admin/monitors/{mid}", s.deleteMonitor)
	admin("PUT /api/admin/routers/{id}/layout", s.saveLayout)
	admin("GET /api/admin/users", s.listUsers)
	admin("POST /api/admin/users", s.saveUser)
	admin("PUT /api/admin/users/{uid}", s.saveUser)
	admin("DELETE /api/admin/users/{uid}", s.deleteUser)
	admin("GET /api/admin/settings", s.getSettings)
	admin("PUT /api/admin/settings", s.putSettings)
	admin("POST /api/admin/settings/telegram-test", s.testTelegram)
	admin("GET /api/admin/backup", s.backup)

	sub, _ := fs.Sub(webFS, "web")
	files := http.FileServerFS(sub)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			errJSON(w, 404, "không có API này")
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	return s.wrap(mux)
}

func (s *Server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.Printf("panic %s %s: %v", r.Method, r.URL.Path, v)
				errJSON(w, 500, "lỗi nội bộ")
			}
		}()
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
			if r.Method != http.MethodGet && r.Method != http.MethodHead && !strings.HasPrefix(r.URL.Path, "/api/push/") {
				// Custom header cannot be sent cross-site without a CORS preflight we never allow.
				if r.Header.Get("X-Requested-With") != "netpulse" {
					errJSON(w, 403, "yêu cầu không hợp lệ (thiếu header chống CSRF)")
					return
				}
				r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		errJSON(w, 400, "dữ liệu gửi lên không hợp lệ")
		return false
	}
	return true
}

func pathID(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.PathValue(name), 10, 64)
	return n
}

func serverErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		errJSON(w, 404, "không tìm thấy")
		return
	}
	log.Printf("lỗi: %v", err)
	errJSON(w, 500, "lỗi máy chủ, xem log")
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
			return strings.TrimSpace(strings.Split(xf, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) isHTTPS(r *http.Request) bool {
	return r.TLS != nil || (s.cfg.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https")
}

// ---------- login rate limiting ----------

type limiter struct {
	mu sync.Mutex
	m  map[string]*limEntry
}

type limEntry struct {
	fails int
	until time.Time
	last  time.Time
}

func newLimiter() *limiter { return &limiter{m: map[string]*limEntry{}} }

func (l *limiter) wait(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[key]; e != nil {
		return time.Until(e.until)
	}
	return 0
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.m) > 10000 {
		for k, e := range l.m {
			if now.Sub(e.last) > time.Hour {
				delete(l.m, k)
			}
		}
	}
	e := l.m[key]
	if e == nil || now.Sub(e.last) > time.Hour {
		e = &limEntry{}
		l.m[key] = e
	}
	e.fails++
	e.last = now
	if e.fails >= 5 {
		d := time.Minute << min(e.fails-5, 4) // 1, 2, 4, 8, 16 minutes
		e.until = now.Add(min(d, 15*time.Minute))
	}
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	delete(l.m, key)
	l.mu.Unlock()
}

func (s *Server) checkPassword(hash, pw string) bool {
	s.pwSem <- struct{}{}
	defer func() { <-s.pwSem }()
	return verifyPassword(hash, pw)
}

// ---------- auth ----------

func (s *Server) authed(admin bool, h func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil || len(c.Value) != 64 {
			errJSON(w, 401, "cần đăng nhập")
			return
		}
		u, err := s.st.SessionUser(sha256Hex(c.Value))
		if errors.Is(err, ErrNotFound) {
			errJSON(w, 401, "phiên đăng nhập đã hết hạn")
			return
		}
		if err != nil {
			serverErr(w, err)
			return
		}
		if admin && !u.Admin() {
			errJSON(w, 403, "chỉ quản trị viên được làm thao tác này")
			return
		}
		h(w, r, u)
	}
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *User) error {
	token := randomToken(32)
	if err := s.st.CreateSession(sha256Hex(token), u.ID, time.Now().Add(sessionTTL).UnixMilli()); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: s.isHTTPS(r) || s.cfg.CookieSecure, MaxAge: int(sessionTTL.Seconds())})
	return nil
}

func (s *Server) meData(u *User) map[string]any {
	return map[string]any{"id": u.ID, "username": u.Username, "role": u.Role, "routers": u.Routers, "version": Version, "demo": s.cfg.Demo}
}

func (s *Server) setupStatus(w http.ResponseWriter, r *http.Request) {
	n, err := s.st.CountUsers()
	if err != nil {
		serverErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"needed": n == 0})
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var in struct{ Code, Username, Password string }
	if !readJSON(w, r, &in) {
		return
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if n, err := s.st.CountUsers(); err != nil || n > 0 {
		errJSON(w, 409, "hệ thống đã được khởi tạo")
		return
	}
	key := s.clientIP(r) + "|setup"
	if s.lim.wait(key) > 0 {
		errJSON(w, 429, "thử sai quá nhiều lần, đợi vài phút")
		return
	}
	if s.setupCode == "" || !equalHash(strings.ToUpper(strings.TrimSpace(in.Code)), s.setupCode) {
		s.lim.fail(key)
		errJSON(w, 403, "mã khởi tạo không đúng (xem cửa sổ/log của NetPulse)")
		return
	}
	if err := validUsername(in.Username); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	if err := validPassword(in.Password); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	id, err := s.st.CreateUser(in.Username, hashPassword(in.Password), "admin")
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	s.setupCode = ""
	u, err := s.st.UserByID(id)
	if err != nil {
		serverErr(w, err)
		return
	}
	if err := s.startSession(w, r, u); err != nil {
		serverErr(w, err)
		return
	}
	log.Printf("đã tạo tài khoản quản trị %q", u.Username)
	writeJSON(w, 200, s.meData(u))
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Password string }
	if !readJSON(w, r, &in) {
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	key := s.clientIP(r) + "|" + strings.ToLower(in.Username)
	if d := s.lim.wait(key); d > 0 {
		errJSON(w, 429, fmt.Sprintf("sai quá nhiều lần, thử lại sau %d giây", int(d.Seconds())+1))
		return
	}
	if len(in.Username) > 64 || len(in.Password) > 256 {
		errJSON(w, 401, "sai tên đăng nhập hoặc mật khẩu")
		return
	}
	u, err := s.st.UserByName(in.Username)
	ok := false
	if err == nil && !u.Disabled {
		ok = s.checkPassword(u.passHash, in.Password)
	} else {
		s.checkPassword(dummyHash, in.Password) // same timing for unknown users
	}
	if !ok {
		s.lim.fail(key)
		errJSON(w, 401, "sai tên đăng nhập hoặc mật khẩu")
		return
	}
	s.lim.reset(key)
	if err := s.startSession(w, r, u); err != nil {
		serverErr(w, err)
		return
	}
	writeJSON(w, 200, s.meData(u))
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.st.DeleteSession(sha256Hex(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, u *User) { writeJSON(w, 200, s.meData(u)) }

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct{ Old, New string }
	if !readJSON(w, r, &in) {
		return
	}
	key := "pw|" + strconv.FormatInt(u.ID, 10)
	if s.lim.wait(key) > 0 {
		errJSON(w, 429, "thử sai quá nhiều lần, đợi vài phút")
		return
	}
	if !s.checkPassword(u.passHash, in.Old) {
		s.lim.fail(key)
		errJSON(w, 403, "mật khẩu hiện tại không đúng")
		return
	}
	if err := validPassword(in.New); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	if err := s.st.SetPassword(u.ID, hashPassword(in.New)); err != nil {
		serverErr(w, err)
		return
	}
	c, _ := r.Cookie(cookieName)
	s.st.DeleteUserSessions(u.ID, sha256Hex(c.Value)) // log out every other device
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- read APIs ----------

func (s *Server) visibleIDs(u *User) []int64 {
	if u.Admin() {
		return nil
	}
	return u.Routers
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request, u *User) {
	evs, err := s.st.ListEvents(s.visibleIDs(u), 30)
	if err != nil {
		serverErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"routers": s.hub.Views(u.CanSee, false), "events": evs, "now": nowMs()})
}

func (s *Server) routerDetail(w http.ResponseWriter, r *http.Request, u *User) {
	id := pathID(r, "id")
	if !u.CanSee(id) {
		errJSON(w, 404, "không tìm thấy router")
		return
	}
	v, ok := s.hub.View(id)
	if !ok {
		errJSON(w, 404, "không tìm thấy router")
		return
	}
	writeJSON(w, 200, map[string]any{"router": v, "now": nowMs()})
}

func (s *Server) history(w http.ResponseWriter, r *http.Request, u *User) {
	id := pathID(r, "id")
	if !u.CanSee(id) {
		errJSON(w, 404, "không tìm thấy router")
		return
	}
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	hours = clampInt(hours, 1, 24*31)
	if r.URL.Query().Get("hours") == "" {
		hours = 24
	}
	iface := r.URL.Query().Get("iface")
	samples, err := s.st.Samples(id, nowMs()-int64(hours)*3600000)
	if err != nil {
		serverErr(w, err)
		return
	}
	bucket := max(int64(60000), int64(hours)*3600000/600/60000*60000)
	type point struct {
		T   int64    `json:"t"`
		CPU *float64 `json:"cpu"`
		Mem *float64 `json:"mem"`
		Rx  *float64 `json:"rx"`
		Tx  *float64 `json:"tx"`
	}
	type agg struct{ cpu, mem, rx, tx, nc, nm, ni float64 }
	var points []point
	var cur agg
	var curT int64 = -1
	emit := func() {
		if curT < 0 {
			return
		}
		p := point{T: curT}
		avg := func(sum, n float64) *float64 {
			if n == 0 {
				return nil
			}
			v := math.Round(sum/n*10) / 10
			return &v
		}
		p.CPU, p.Mem, p.Rx, p.Tx = avg(cur.cpu, cur.nc), avg(cur.mem, cur.nm), avg(cur.rx, cur.ni), avg(cur.tx, cur.ni)
		points = append(points, p)
	}
	for _, x := range samples {
		t := x.At / bucket * bucket
		if t != curT {
			emit()
			cur, curT = agg{}, t
		}
		if x.CPU.Valid {
			cur.cpu += x.CPU.Float64
			cur.nc++
		}
		if x.Mem.Valid {
			cur.mem += x.Mem.Float64
			cur.nm++
		}
		if iface != "" {
			var d map[string][2]float64
			if json.Unmarshal([]byte(x.Data), &d) == nil {
				if v, ok := d[iface]; ok {
					cur.rx += v[0]
					cur.tx += v[1]
					cur.ni++
				}
			}
		}
	}
	emit()
	if points == nil {
		points = []point{}
	}
	writeJSON(w, 200, map[string]any{"points": points, "bucket": bucket})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, u *User) {
	limit := clampInt(atoi(r.URL.Query().Get("limit"), 200), 1, 2000)
	ids := s.visibleIDs(u)
	if rid := atoi(r.URL.Query().Get("router"), 0); rid > 0 {
		if !u.CanSee(int64(rid)) {
			errJSON(w, 404, "không tìm thấy router")
			return
		}
		ids = []int64{int64(rid)}
	}
	evs, err := s.st.ListEvents(ids, limit)
	if err != nil {
		serverErr(w, err)
		return
	}
	writeJSON(w, 200, evs)
}

func atoi(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// ---------- router admin ----------

var (
	reHost     = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	reIfName   = regexp.MustCompile(`^[^\x00-\x1f"\\$]{1,64}$`)
	reUsername = regexp.MustCompile(`^[A-Za-z0-9._-]{2,32}$`)
)

func validUsername(u string) error {
	if !reUsername.MatchString(u) {
		return errors.New("tên đăng nhập 2–32 ký tự: chữ, số, dấu . _ -")
	}
	return nil
}

type routerInput struct {
	Name      string  `json:"name"`
	Mode      string  `json:"mode"`
	Host      string  `json:"host"`
	Port      int     `json:"port"`
	TLS       bool    `json:"tls"`
	VerifyTLS bool    `json:"verifyTls"`
	Username  string  `json:"username"`
	Password  *string `json:"password"` // nil = keep
	Interval  int     `json:"interval"`
	WANs      []WAN   `json:"wans"`
	WANTarget string  `json:"wanTarget"`
	ChatID    string  `json:"chatId"`
	Enabled   bool    `json:"enabled"`
}

func (s *Server) applyRouter(r *Router, in routerInput) error {
	in.Name, in.Host, in.Username = strings.TrimSpace(in.Name), strings.TrimSpace(in.Host), strings.TrimSpace(in.Username)
	in.ChatID = strings.TrimSpace(in.ChatID)
	if n := len([]rune(in.Name)); n < 1 || n > 64 {
		return errors.New("tên router 1–64 ký tự")
	}
	switch in.Mode {
	case "pull":
		if net.ParseIP(in.Host) == nil && !reHost.MatchString(in.Host) {
			return errors.New("địa chỉ router không hợp lệ (IP hoặc tên miền, không kèm http://)")
		}
		if in.Port < 0 || in.Port > 65535 {
			return errors.New("cổng không hợp lệ")
		}
		if in.Username == "" || len(in.Username) > 64 {
			return errors.New("cần user đăng nhập router")
		}
		if in.Password != nil && len(*in.Password) > 128 {
			return errors.New("mật khẩu router quá dài")
		}
		if in.Password == nil && r.secret == "" {
			return errors.New("cần mật khẩu router")
		}
	case "push":
	case "demo":
		if !s.cfg.Demo {
			return errors.New("chế độ demo chỉ dùng khi chạy với -demo")
		}
	default:
		return errors.New("chế độ phải là pull hoặc push")
	}
	if in.Interval == 0 {
		in.Interval = 15
	}
	if in.Interval < 5 || in.Interval > 300 {
		return errors.New("chu kỳ đọc 5–300 giây")
	}
	if len(in.WANs) > 8 {
		return errors.New("tối đa 8 WAN")
	}
	seen := map[string]bool{}
	for i := range in.WANs {
		in.WANs[i].Iface, in.WANs[i].Label = strings.TrimSpace(in.WANs[i].Iface), strings.TrimSpace(in.WANs[i].Label)
		if !reIfName.MatchString(in.WANs[i].Iface) || seen[in.WANs[i].Iface] || len([]rune(in.WANs[i].Label)) > 40 {
			return errors.New("danh sách WAN không hợp lệ hoặc bị trùng")
		}
		seen[in.WANs[i].Iface] = true
	}
	if in.WANTarget == "" {
		in.WANTarget = "8.8.8.8"
	}
	if ip := net.ParseIP(in.WANTarget); ip == nil {
		return errors.New("IP kiểm tra Internet không hợp lệ")
	}
	if in.ChatID != "" && !reChat.MatchString(in.ChatID) {
		return errors.New("Chat ID nhóm không hợp lệ")
	}
	r.Name, r.Mode, r.Host, r.Port, r.TLS, r.VerifyTLS, r.Username = in.Name, in.Mode, in.Host, in.Port, in.TLS, in.VerifyTLS, in.Username
	r.Interval, r.WANs, r.WANTarget, r.ChatID, r.Enabled = in.Interval, in.WANs, in.WANTarget, in.ChatID, in.Enabled
	if r.WANs == nil {
		r.WANs = []WAN{}
	}
	if in.Password != nil {
		r.secret = s.box.Seal(*in.Password)
	}
	return nil
}

func (s *Server) adminRouters(w http.ResponseWriter, r *http.Request, u *User) {
	rs, err := s.st.ListRouters()
	if err != nil {
		serverErr(w, err)
		return
	}
	if rs == nil {
		rs = []*Router{}
	}
	writeJSON(w, 200, map[string]any{"routers": rs, "uninstall": uninstallScript})
}

func (s *Server) saveRouter(w http.ResponseWriter, r *http.Request, u *User) {
	var in routerInput
	if !readJSON(w, r, &in) {
		return
	}
	rt := &Router{}
	if id := pathID(r, "id"); id > 0 {
		var err error
		if rt, err = s.st.GetRouter(id); err != nil {
			serverErr(w, err)
			return
		}
	}
	if err := s.applyRouter(rt, in); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	if err := s.st.SaveRouter(rt); err != nil {
		serverErr(w, err)
		return
	}
	if err := s.hub.Reload(rt.ID); err != nil {
		serverErr(w, err)
		return
	}
	writeJSON(w, 200, rt)
}

func (s *Server) deleteRouter(w http.ResponseWriter, r *http.Request, u *User) {
	id := pathID(r, "id")
	if err := s.st.DeleteRouter(id); err != nil {
		serverErr(w, err)
		return
	}
	s.hub.Reload(id)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// testRouter connects with the submitted settings (or the stored password)
// and reports identity, version and interface names for the WAN picker.
func (s *Server) testRouter(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		routerInput
		ID int64 `json:"id"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	rt := &Router{}
	if in.ID > 0 {
		if old, err := s.st.GetRouter(in.ID); err == nil {
			rt = old
		}
	}
	in.Mode = "pull"
	in.Name = "test"
	if err := s.applyRouter(rt, in.routerInput); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	pass, err := s.box.Open(rt.secret)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	src := newRestSource(rt, pass)
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	res, err := src.Fetch(ctx, "/system/resource", resourceProps)
	if err != nil {
		errJSON(w, 502, err.Error())
		return
	}
	out := map[string]any{"ok": true}
	if len(res) > 0 {
		out["version"], out["board"] = res[0]["version"], res[0]["board-name"]
	}
	if id, err := src.Fetch(ctx, "/system/identity", nil); err == nil && len(id) > 0 {
		out["identity"] = id[0]["name"]
	}
	names := []string{}
	if ifs, err := src.Fetch(ctx, "/interface", []string{"name", "type"}); err == nil {
		for _, i := range ifs {
			if !strings.HasSuffix(i["type"], "-in") {
				names = append(names, i["name"])
			}
		}
	}
	out["interfaces"] = names
	if _, err := src.Ping(ctx, "127.0.0.1", ""); err != nil {
		out["pingWarning"] = "Không chạy được lệnh ping (user cần quyền test): " + err.Error()
	}
	writeJSON(w, 200, out)
}

func (s *Server) pushScript(w http.ResponseWriter, r *http.Request, u *User) {
	rt, err := s.st.GetRouter(pathID(r, "id"))
	if err != nil {
		serverErr(w, err)
		return
	}
	if rt.Mode != "push" {
		errJSON(w, 400, "router không ở chế độ push")
		return
	}
	base := s.settings.Get().PushBase
	if base == "" {
		scheme := "http"
		if s.isHTTPS(r) {
			scheme = "https"
		}
		host := r.Host
		if s.cfg.TrustProxy && r.Header.Get("X-Forwarded-Host") != "" {
			host = r.Header.Get("X-Forwarded-Host")
		}
		base = scheme + "://" + host
	}
	if strings.Contains(base, "localhost") || strings.Contains(base, "127.0.0.1") {
		errJSON(w, 400, "Router không gọi được localhost. Mở NetPulse bằng IP LAN hoặc đặt 'Địa chỉ cho router' trong Cài đặt.")
		return
	}
	token := randomToken(32)
	if err := s.st.SetPushToken(rt.ID, sha256Hex(token)); err != nil {
		serverErr(w, err)
		return
	}
	s.hub.Reload(rt.ID)
	script := installerScript(rt.Name, base+"/api/push/"+strconv.FormatInt(rt.ID, 10), token, strings.HasPrefix(base, "https://"), rt.Interval)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="netpulse-%d.rsc"`, rt.ID))
	io.WriteString(w, script)
}

func (s *Server) push(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	rs, err := s.hub.PushAuth(pathID(r, "id"), token)
	if err != nil {
		errJSON(w, 401, err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var m pushMsg
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		errJSON(w, 400, "JSON không hợp lệ")
		return
	}
	reply, err := s.hub.Ingest(rs, &m)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(marshalReply(reply))
}

// ---------- monitors & topology ----------

var monitorKinds = map[string]bool{"device": true, "switch": true, "ap": true, "camera": true, "server": true, "printer": true, "phone": true, "virtual": true}

func (s *Server) saveMonitor(w http.ResponseWriter, r *http.Request, u *User) {
	var in Monitor
	if !readJSON(w, r, &in) {
		return
	}
	m := &Monitor{}
	if mid := pathID(r, "mid"); mid > 0 {
		var err error
		if m, err = s.st.GetMonitor(mid); err != nil {
			serverErr(w, err)
			return
		}
	} else {
		m.RouterID = pathID(r, "id")
		if _, err := s.st.GetRouter(m.RouterID); err != nil {
			serverErr(w, err)
			return
		}
	}
	in.Name, in.IP = strings.TrimSpace(in.Name), strings.TrimSpace(in.IP)
	if n := len([]rune(in.Name)); n < 1 || n > 64 {
		errJSON(w, 400, "tên thiết bị 1–64 ký tự")
		return
	}
	if !monitorKinds[in.Kind] {
		errJSON(w, 400, "loại thiết bị không hợp lệ")
		return
	}
	if in.Kind != "virtual" && net.ParseIP(in.IP) == nil {
		errJSON(w, 400, "IP không hợp lệ")
		return
	}
	if in.Kind == "virtual" {
		in.IP = ""
	}
	if !oneOf(in.RepeatMin, 0, 15, 30, 60, 120) {
		errJSON(w, 400, "chu kỳ nhắc lại không hợp lệ")
		return
	}
	if in.ParentID != nil {
		// parent must exist on the same router and must not create a loop
		cur := *in.ParentID
		for depth := 0; ; depth++ {
			if cur == m.ID || depth > 200 {
				errJSON(w, 400, "nối thiết bị tạo thành vòng lặp")
				return
			}
			p, err := s.st.GetMonitor(cur)
			if err != nil || p.RouterID != m.RouterID {
				errJSON(w, 400, "thiết bị cha không hợp lệ")
				return
			}
			if p.ParentID == nil {
				break
			}
			cur = *p.ParentID
		}
	}
	m.Name, m.IP, m.Kind, m.ParentID, m.Enabled, m.Notify, m.RepeatMin = in.Name, in.IP, in.Kind, in.ParentID, in.Enabled, in.Notify, in.RepeatMin
	if err := s.st.SaveMonitor(m); err != nil {
		serverErr(w, err)
		return
	}
	s.hub.ReloadMonitors(m.RouterID)
	writeJSON(w, 200, m)
}

func (s *Server) deleteMonitor(w http.ResponseWriter, r *http.Request, u *User) {
	m, err := s.st.GetMonitor(pathID(r, "mid"))
	if err != nil {
		serverErr(w, err)
		return
	}
	if err := s.st.DeleteMonitor(m); err != nil {
		serverErr(w, err)
		return
	}
	s.hub.ReloadMonitors(m.RouterID)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) saveLayout(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Nodes []NodePos `json:"nodes"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if len(in.Nodes) > 2000 {
		errJSON(w, 400, "quá nhiều nút")
		return
	}
	for _, n := range in.Nodes {
		if math.IsNaN(n.X) || math.IsNaN(n.Y) || math.Abs(n.X) > 1e5 || math.Abs(n.Y) > 1e5 {
			errJSON(w, 400, "toạ độ không hợp lệ")
			return
		}
	}
	id := pathID(r, "id")
	if err := s.st.SaveLayout(id, in.Nodes); err != nil {
		serverErr(w, err)
		return
	}
	s.hub.Reload(id)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- users ----------

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, u *User) {
	users, err := s.st.ListUsers()
	if err != nil {
		serverErr(w, err)
		return
	}
	if users == nil {
		users = []User{}
	}
	writeJSON(w, 200, users)
}

func (s *Server) saveUser(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Username string  `json:"username"`
		Password string  `json:"password"`
		Role     string  `json:"role"`
		Disabled bool    `json:"disabled"`
		Routers  []int64 `json:"routers"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Role != "admin" && in.Role != "viewer" {
		errJSON(w, 400, "vai trò không hợp lệ")
		return
	}
	if in.Password != "" {
		if err := validPassword(in.Password); err != nil {
			errJSON(w, 400, err.Error())
			return
		}
	}
	id := pathID(r, "uid")
	if id == 0 {
		if err := validUsername(in.Username); err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		if in.Password == "" {
			errJSON(w, 400, "tài khoản mới cần mật khẩu")
			return
		}
		var err error
		if id, err = s.st.CreateUser(in.Username, hashPassword(in.Password), in.Role); err != nil {
			errJSON(w, 400, err.Error())
			return
		}
	} else {
		target, err := s.st.UserByID(id)
		if err != nil {
			serverErr(w, err)
			return
		}
		if target.Admin() && (in.Role != "admin" || in.Disabled) {
			if n, _ := s.st.CountAdmins(); n <= 1 {
				errJSON(w, 400, "phải còn ít nhất một quản trị viên đang hoạt động")
				return
			}
			if target.ID == u.ID {
				errJSON(w, 400, "không thể tự hạ quyền hoặc khoá chính mình")
				return
			}
		}
		if in.Password != "" {
			if err := s.st.SetPassword(id, hashPassword(in.Password)); err != nil {
				serverErr(w, err)
				return
			}
			s.st.DeleteUserSessions(id, "")
		}
	}
	if err := s.st.UpdateUser(id, in.Role, in.Disabled, in.Routers); err != nil {
		serverErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, u *User) {
	id := pathID(r, "uid")
	if id == u.ID {
		errJSON(w, 400, "không thể xoá chính mình")
		return
	}
	target, err := s.st.UserByID(id)
	if err != nil {
		serverErr(w, err)
		return
	}
	if target.Admin() {
		if n, _ := s.st.CountAdmins(); n <= 1 {
			errJSON(w, 400, "phải còn ít nhất một quản trị viên")
			return
		}
	}
	if err := s.st.DeleteUser(id); err != nil {
		serverErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- settings & backup ----------

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request, u *User) {
	cur := s.settings.Get()
	hasToken := cur.TGToken != ""
	cur.TGToken = ""
	writeJSON(w, 200, map[string]any{"settings": cur, "hasToken": hasToken, "tokenOk": s.settings.Token() != "" || !hasToken,
		"telegramError": s.n.LastError(), "version": Version, "dataDir": s.cfg.DataDir})
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request, u *User) {
	var in SettingsInput
	if !readJSON(w, r, &in) {
		return
	}
	if err := s.settings.Update(in); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	s.getSettings(w, r, u)
}

func (s *Server) testTelegram(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Chat string `json:"chat"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	chat := strings.TrimSpace(in.Chat)
	if chat == "" {
		chat = s.settings.Get().AdminChat
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.n.Test(ctx, chat); err != nil {
		errJSON(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) backup(w http.ResponseWriter, r *http.Request, u *User) {
	tmp := filepath.Join(s.cfg.DataDir, "backup-"+randomToken(6)+".db")
	defer os.Remove(tmp)
	if err := s.st.Backup(tmp); err != nil {
		serverErr(w, err)
		return
	}
	f, err := os.Open(tmp)
	if err != nil {
		serverErr(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="netpulse-`+time.Now().Format("20060102-1504")+`.db"`)
	io.Copy(w, f)
}
