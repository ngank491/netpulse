package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRosSeconds(t *testing.T) {
	cases := map[string]float64{
		"1w2d3h4m5s": 604800 + 2*86400 + 3*3600 + 4*60 + 5,
		"2d03:04:05": 2*86400 + 3*3600 + 4*60 + 5,
		"03:04:05":   3*3600 + 4*60 + 5,
		"10ms":       0.010,
		"1ms500us":   0.0015,
		"5m":         300,
		"":           0,
	}
	for in, want := range cases {
		if got := rosSeconds(in); (got-want) > 1e-9 || (want-got) > 1e-9 {
			t.Errorf("rosSeconds(%q)=%v want %v", in, got, want)
		}
	}
}

func TestParsePing(t *testing.T) {
	rows := []Row{
		{"host": "8.8.8.8", "seq": "0", "time": "12ms", "sent": "1", "received": "1"},
		{"host": "8.8.8.8", "seq": "1", "status": "timeout", "sent": "2", "received": "1", "avg-rtt": "12ms"},
	}
	r := parsePing(rows, 2)
	if r.Sent != 2 || r.Received != 1 || r.RTT != 12 {
		t.Fatalf("got %+v", r)
	}
	r = parsePing([]Row{{"time": "4ms"}, {"time": "6ms"}}, 2)
	if r.Received != 2 || r.RTT != 5 {
		t.Fatalf("no-summary got %+v", r)
	}
}

func TestProbeHysteresis(t *testing.T) {
	p := newProbe()
	if tr := p.step(false, false, 1, -1, 2); tr != "" {
		t.Fatal("one failure must not alert")
	}
	if tr := p.step(false, true, 2, -1, 2); tr != "" || p.Fails != 1 {
		t.Fatal("errored check must not count")
	}
	if tr := p.step(false, false, 3, -1, 2); tr != "down" {
		t.Fatal("two failures must go down")
	}
	if tr := p.step(false, false, 4, -1, 2); tr != "" {
		t.Fatal("already down must not re-alert")
	}
	if tr := p.step(true, false, 5, 3, 2); tr != "up" || p.Since != 5 {
		t.Fatal("success must recover")
	}
	q := newProbe()
	if tr := q.step(true, false, 1, 1, 2); tr != "" || q.Status != "up" {
		t.Fatal("unknown->up is silent")
	}
}

func TestDetectWANs(t *testing.T) {
	got := detectWANs(map[string][]Row{
		"pppoe_client": {{"name": "pppoe-out1", "interface": "ether1"}},
		"dhcp_client":  {{"interface": "ether2", "add-default-route": "yes"}, {"interface": "bridge", "add-default-route": "no"}},
		"route":        {{"immediate-gw": "10.0.0.1%ether1"}, {"gateway": "pppoe-out1"}},
		"wan_list":     {{"interface": "lte1"}},
	})
	want := "lte1,pppoe-out1,ether2"
	if strings.Join(got, ",") != want {
		t.Fatalf("got %v want %s", got, want)
	}
}

func TestPasswordAndBox(t *testing.T) {
	h := hashPassword("correct horse battery")
	if !verifyPassword(h, "correct horse battery") || verifyPassword(h, "wrong") {
		t.Fatal("password verify")
	}
	box, err := LoadBox(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	sealed := box.Seal("secret")
	if out, err := box.Open(sealed); err != nil || out != "secret" {
		t.Fatal("box roundtrip", err)
	}
	if _, err := box.Open(sealed[:len(sealed)-2] + "AA"); err == nil {
		t.Fatal("tampered ciphertext must fail")
	}
}

func TestAccumRates(t *testing.T) {
	var a accum
	r1 := 8e6
	ifs := []Iface{{Name: "ether1", RxBps: &r1, TxBps: &r1}}
	if s := a.add(60_000, nil, nil, ifs); s != nil {
		t.Fatal("first sample should not flush")
	}
	a.add(90_000, nil, nil, ifs)
	s := a.add(120_000, nil, nil, ifs)
	if s == nil || s.at != 60_000 || !strings.Contains(string(s.data), `"ether1":[8000000,8000000]`) {
		t.Fatalf("flush got %+v", s)
	}
}

// ---------- end-to-end: fake RouterOS REST API + real HTTP server ----------

type fakeROS struct {
	mu      sync.Mutex
	rx      uint64
	running bool
	dead    map[string]bool
	pings   atomic.Int64
}

func (f *fakeROS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if u, p, ok := r.BasicAuth(); !ok || u != "ro" || p != "pw" {
		w.WriteHeader(401)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/rest/system/resource":
		io.WriteString(w, `{"uptime":"1d02:03:04","cpu-load":"7","total-memory":"1000","free-memory":"250","version":"7.18"}`)
	case r.URL.Path == "/rest/interface":
		f.rx += 1_000_000
		json.NewEncoder(w).Encode([]map[string]string{
			{"name": "ether1", "type": "ether", "running": map[bool]string{true: "true", false: "false"}[f.running], "disabled": "false", "rx-byte": itoa(f.rx), "tx-byte": "100"},
			{"name": "<pppoe-u1>", "type": "pppoe-in", "running": "true", "rx-byte": "1", "tx-byte": "1"},
		})
	case r.URL.Path == "/rest/ping":
		f.pings.Add(1)
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		recv := "2"
		if f.dead[body["address"]] {
			recv = "0"
		}
		io.WriteString(w, `[{"seq":"1","sent":"2","received":"`+recv+`","avg-rtt":"3ms"}]`)
	case r.URL.Path == "/rest/system/identity":
		io.WriteString(w, `{"name":"TestRouter"}`)
	case r.URL.Path == "/rest/ppp/active":
		io.WriteString(w, `[{"name":"u1","service":"pppoe","password":"leak"}]`)
	default:
		w.WriteHeader(404)
		io.WriteString(w, `{"error":404,"message":"Not Found"}`)
	}
}

func itoa(n uint64) string { b, _ := json.Marshal(n); return string(b) }

type harness struct {
	t   *testing.T
	srv *Server
	hub *Hub
	st  *Store
	web *httptest.Server
	c   *http.Client
}

func newHarness(t *testing.T) *harness {
	dir := t.TempDir()
	st, err := OpenStore(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := LoadBox(filepath.Join(dir, "key"))
	settings, _ := NewSettingsStore(st, box)
	cfg := &Config{DataDir: dir}
	srv := &Server{cfg: cfg, st: st, box: box, settings: settings, lim: newLimiter(), pwSem: make(chan struct{}, 2), setupCode: "ABCD-EFGH"}
	hub := NewHub(st, box, settings, false)
	n := NewNotifier(st, settings)
	hub.notify, n.hub, srv.hub, srv.n = n, hub, hub, n
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); hub.Wait() })
	if err := hub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(srv.Handler())
	t.Cleanup(web.Close)
	jar, _ := cookiejar.New(nil)
	return &harness{t, srv, hub, st, web, &http.Client{Jar: jar}}
}

func (h *harness) call(method, path string, body any, out any) int {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, h.web.URL+path, rd)
	req.Header.Set("X-Requested-With", "netpulse")
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.c.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func waitFor(t *testing.T, what string, cond func() bool) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestSetupLoginAndCSRF(t *testing.T) {
	h := newHarness(t)
	if code := h.call("GET", "/api/overview", nil, nil); code != 401 {
		t.Fatalf("anon overview = %d", code)
	}
	if code := h.call("POST", "/api/setup", map[string]string{"code": "WRONG", "username": "admin", "password": "0123456789"}, nil); code != 403 {
		t.Fatalf("bad setup code = %d", code)
	}
	codeFile := filepath.Join(h.srv.cfg.DataDir, setupCodeFile)
	writeFileAtomic(codeFile, []byte("ABCD-EFGH\n"), 0o600)
	if code := h.call("POST", "/api/setup", map[string]string{"code": "abcd-efgh", "username": "admin", "password": "0123456789"}, nil); code != 200 {
		t.Fatalf("setup = %d", code)
	}
	if _, err := os.Stat(codeFile); !os.IsNotExist(err) {
		t.Fatal("setup code file must be removed after setup")
	}
	if code := h.call("POST", "/api/setup", map[string]string{"code": "abcd-efgh", "username": "x2", "password": "0123456789"}, nil); code != 409 {
		t.Fatalf("second setup = %d", code)
	}
	// mutating request without the anti-CSRF header is refused
	req, _ := http.NewRequest("POST", h.web.URL+"/api/admin/routers", strings.NewReader(`{}`))
	resp, _ := h.c.Do(req)
	if resp.StatusCode != 403 {
		t.Fatalf("csrf = %d", resp.StatusCode)
	}
	// viewer cannot reach admin APIs or routers not granted
	var created map[string]int64
	h.call("POST", "/api/admin/users", map[string]any{"username": "bob", "password": "viewerpass1", "role": "viewer", "routers": []int64{}}, &created)
	h.call("POST", "/api/logout", nil, nil)
	if code := h.call("POST", "/api/login", map[string]string{"username": "bob", "password": "viewerpass1"}, nil); code != 200 {
		t.Fatalf("viewer login = %d", code)
	}
	if code := h.call("GET", "/api/admin/routers", nil, nil); code != 403 {
		t.Fatalf("viewer admin = %d", code)
	}
	// brute force lockout
	h.call("POST", "/api/logout", nil, nil)
	for i := 0; i < 5; i++ {
		h.call("POST", "/api/login", map[string]string{"username": "admin", "password": "nope"}, nil)
	}
	if code := h.call("POST", "/api/login", map[string]string{"username": "admin", "password": "0123456789"}, nil); code != 429 {
		t.Fatalf("lockout = %d", code)
	}
}

func TestPullEndToEnd(t *testing.T) {
	ros := &fakeROS{running: true, dead: map[string]bool{"10.0.0.9": true}}
	rosSrv := httptest.NewServer(ros)
	defer rosSrv.Close()
	u, _ := url.Parse(rosSrv.URL)
	host, port, _ := strings.Cut(u.Host, ":")

	h := newHarness(t)
	h.call("POST", "/api/setup", map[string]string{"code": "ABCD-EFGH", "username": "admin", "password": "0123456789"}, nil)
	var rt Router
	code := h.call("POST", "/api/admin/routers", map[string]any{"name": "R1", "mode": "pull", "host": host, "port": atoi(port, 0), "tls": false,
		"username": "ro", "password": "pw", "interval": 5, "wans": []WAN{{Iface: "ether1", Label: "FPT"}}, "enabled": true}, &rt)
	if code != 200 || rt.ID == 0 {
		t.Fatalf("create router = %d", code)
	}
	var test map[string]any
	if code := h.call("POST", "/api/admin/routers/test", map[string]any{"id": rt.ID, "host": host, "port": atoi(port, 0), "username": "ro"}, &test); code != 200 || test["identity"] != "TestRouter" {
		t.Fatalf("test connection = %d %v", code, test)
	}
	h.call("POST", "/api/admin/routers/"+itoa(uint64(rt.ID))+"/monitors", map[string]any{"name": "Cam", "ip": "10.0.0.9", "kind": "camera", "enabled": true, "notify": true}, nil)
	h.hub.ReloadMonitors(rt.ID)

	var det struct{ Router RouterView }
	waitFor(t, "router up", func() bool {
		h.call("GET", "/api/routers/"+itoa(uint64(rt.ID)), nil, &det)
		return det.Router.Status == "up" && det.Router.Identity == "TestRouter"
	})
	if len(det.Router.Ifaces) != 1 {
		t.Fatalf("pppoe-in interfaces must be hidden: %+v", det.Router.Ifaces)
	}
	if _, leaked := det.Router.Sections["ppp_active"][0]["password"]; leaked {
		t.Fatal("password field must be stripped")
	}
	if det.Router.Mem == nil || *det.Router.Mem != 75 {
		t.Fatalf("mem = %v", det.Router.Mem)
	}

	// force two probe rounds and check the camera goes down with an event
	rs := h.hub.get(rt.ID)
	src := newRestSource(&rt, "pw")
	src.base = rosSrv.URL + "/rest"
	h.hub.runProbes(context.Background(), rs, src)
	h.hub.runProbes(context.Background(), rs, src)
	h.call("GET", "/api/routers/"+itoa(uint64(rt.ID)), nil, &det)
	var cam MonitorView
	for _, m := range det.Router.Monitors {
		if m.Name == "Cam" {
			cam = m
		}
	}
	if cam.State == nil || cam.State.Status != "down" {
		t.Fatalf("camera state = %+v", cam.State)
	}
	evs, _ := h.st.ListEvents(nil, 10)
	found := false
	for _, e := range evs {
		found = found || strings.Contains(e.Msg, "Cam (10.0.0.9) mất kết nối")
	}
	if !found {
		t.Fatalf("missing down event: %+v", evs)
	}
	// WAN link loss needs two consecutive samples
	ros.mu.Lock()
	ros.running = false
	ros.mu.Unlock()
	h.hub.observeFast(rs, Row{"uptime": "1d02:03:10"}, []Row{{"name": "ether1", "running": "false", "rx-byte": "1", "tx-byte": "1"}}, nowMs())
	if rs.WANs["ether1"].Link.Status == "down" {
		t.Fatal("one sample must not mark link down")
	}
	h.hub.observeFast(rs, Row{"uptime": "1d02:03:20"}, []Row{{"name": "ether1", "running": "false", "rx-byte": "1", "tx-byte": "1"}}, nowMs())
	if rs.WANs["ether1"].Link.Status != "down" {
		t.Fatal("two samples must mark link down")
	}
	// Telegram alert was queued even though no bot is configured yet (kept up to 24h)
	h.srv.settings.Update(SettingsInput{AdminChat: "123", SummaryAt: "08:00", RepeatMin: 30, Timezone: "Asia/Ho_Chi_Minh", RetentionDays: 30})
	h.hub.observeFast(rs, Row{"uptime": "1d02:03:30"}, []Row{{"name": "ether1", "running": "true", "rx-byte": "1", "tx-byte": "1"}}, nowMs())
	msgs, _ := h.st.DueOutbox(nowMs()+1, 10)
	if len(msgs) == 0 || !strings.Contains(msgs[len(msgs)-1].Text, "[R1] WAN FPT (ether1): link phục hồi") {
		t.Fatalf("outbox = %+v", msgs)
	}
}

func TestPushIngest(t *testing.T) {
	h := newHarness(t)
	h.call("POST", "/api/setup", map[string]string{"code": "ABCD-EFGH", "username": "admin", "password": "0123456789"}, nil)
	var rt Router
	h.call("POST", "/api/admin/routers", map[string]any{"name": "Remote", "mode": "push", "interval": 30, "wans": []WAN{{Iface: "pppoe-out1"}}, "enabled": true}, &rt)
	h.call("POST", "/api/admin/routers/"+itoa(uint64(rt.ID))+"/monitors", map[string]any{"name": "NAS", "ip": "192.168.1.5", "kind": "server", "enabled": true, "notify": true}, nil)

	// localhost is refused (a router can't reach it), so set the address routers should use
	if err := h.srv.settings.Update(SettingsInput{SummaryAt: "08:00", RepeatMin: 30, Timezone: "Asia/Ho_Chi_Minh", RetentionDays: 30, PushBase: "http://10.0.0.2:8080"}); err != nil {
		t.Fatal(err)
	}
	// download installer (rotates token) and extract the token from it
	req, _ := http.NewRequest("POST", h.web.URL+"/api/admin/routers/"+itoa(uint64(rt.ID))+"/push-script", nil)
	req.Header.Set("X-Requested-With", "netpulse")
	resp, err := h.c.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("push-script: %v %d", err, resp.StatusCode)
	}
	script, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	i := strings.Index(string(script), "Bearer ")
	token := string(script[i+7 : i+7+64])
	if !strings.Contains(string(script), "/system scheduler add name=\"netpulse\"") {
		t.Fatal("installer missing scheduler")
	}

	post := func(tok string, body string) (int, map[string]any) {
		r, _ := http.NewRequest("POST", h.web.URL+"/api/push/"+itoa(uint64(rt.ID)), strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := post("bad", `{"kind":"fast"}`); code != 401 {
		t.Fatalf("bad token = %d", code)
	}
	code, reply := post(token, `{"v":1,"kind":"fast","identity":"HN-01","resource":{"uptime":"1w","cpu-load":3,"total-memory":100,"free-memory":50},
		"ifaces":[{"name":"pppoe-out1","type":"pppoe-out","running":true,"disabled":false,"rx-byte":10,"tx-byte":20}]}`)
	if code != 200 || len(reply["ping"].([]any)) != 1 || reply["target"] != "8.8.8.8" || len(reply["sections"].([]any)) == 0 {
		t.Fatalf("fast reply %d %v", code, reply)
	}
	// probes: NAS fails twice → down
	for range 2 {
		post(token, `{"v":1,"kind":"probe","ping":[{"id":`+itoa(uint64(reply["ping"].([]any)[0].(map[string]any)["id"].(float64)))+`,"r":0}],"wan":[{"if":"pppoe-out1","r":2}]}`)
	}
	// chunked section
	post(token, `{"v":1,"kind":"section","name":"lease","gen":"g1","part":0,"final":false,"rows":[{"address":"192.168.1.5","host-name":"nas"}]}`)
	post(token, `{"v":1,"kind":"section","name":"lease","gen":"g1","part":1,"final":true,"rows":[{"address":"192.168.1.6","host-name":"pc"}]}`)

	v, _ := h.hub.View(rt.ID)
	if v.Status != "up" || v.Identity != "HN-01" || v.MonitorsDown != 1 || len(v.Sections["lease"]) != 2 {
		t.Fatalf("view after push: status=%s id=%s down=%d leases=%d", v.Status, v.Identity, v.MonitorsDown, len(v.Sections["lease"]))
	}
	if v.WANs[0].Inet.Status != "up" {
		t.Fatalf("wan inet = %s", v.WANs[0].Inet.Status)
	}
	// watchdog marks a silent push router down
	rs := h.hub.get(rt.ID)
	rs.mu.Lock()
	rs.LastSeen = nowMs() - 200_000
	rs.mu.Unlock()
	h.hub.tick(nowMs())
	if v, _ := h.hub.View(rt.ID); v.Status != "down" {
		t.Fatalf("watchdog status = %s", v.Status)
	}
}

func TestInstallerQuoting(t *testing.T) {
	s := installerScript(`Văn "phòng"`, "http://10.0.0.2:8080/api/push/1", strings.Repeat("a", 64), false, 30)
	// the embedded source must be one quoted literal: no raw newline between the quotes
	start := strings.Index(s, ":local src \"")
	end := strings.Index(s[start:], "\"\n:local s [")
	if start < 0 || end < 0 || strings.Contains(s[start:start+end], "\n") {
		t.Fatal("collector source is not a single-line string literal")
	}
	lit := s[start+len(":local src \"") : start+end]
	for i := range lit {
		if lit[i] == '$' && (i == 0 || lit[i-1] != '\\') {
			t.Fatalf("unescaped $ at %d: %q", i, lit[max(0, i-20):i+5])
		}
	}
	if !strings.Contains(s, "http://10.0.0.2:8080/api/push/1") {
		t.Fatal("url missing")
	}
}
