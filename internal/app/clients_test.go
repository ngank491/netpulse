package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestVendorOf(t *testing.T) {
	if v, r := vendorOf("C4:AD:34:C6:04:C0"); v != "Routerboard.com" || r {
		t.Fatalf("mikrotik = %q %v", v, r)
	}
	if v, r := vendorOf("DC:A6:32:00:00:01"); v != "Raspberry Pi" || r {
		t.Fatalf("rpi = %q %v", v, r)
	}
	if _, r := vendorOf("BE:3D:31:00:00:01"); !r {
		t.Fatal("locally administered MAC must be flagged random")
	}
	if normMAC("aa-bb-cc-dd-ee-ff") != "AA:BB:CC:DD:EE:FF" || normMAC("junk") != "" {
		t.Fatal("normMAC")
	}
}

func TestParseNBStatAndDNSName(t *testing.T) {
	// header(12) + name(34) + type/class/ttl/rdlen(10) + count(1) + 2 entries
	b := make([]byte, 12)
	b = append(b, 0x20)
	b = append(b, []byte(strings.Repeat("A", 32))...)
	b = append(b, 0, 0, 0x21, 0, 1, 0, 0, 0, 0, 0, 37)
	b = append(b, 2)
	entry := func(name string, suffix byte, flags uint16) []byte {
		e := []byte(name + strings.Repeat(" ", 15-len(name)))
		return append(e, suffix, byte(flags>>8), byte(flags))
	}
	b = append(b, entry("WORKGROUP", 0x00, 0x8400)...) // group name: skip
	b = append(b, entry("PC-KETOAN", 0x00, 0x0400)...)
	if n := parseNBStat(b); n != "PC-KETOAN" {
		t.Fatalf("nbstat = %q", n)
	}
	// compressed DNS name: "iphone.local" then pointer to it
	msg := []byte{6, 'i', 'p', 'h', 'o', 'n', 'e', 5, 'l', 'o', 'c', 'a', 'l', 0, 0xC0, 0x00}
	if n, next, ok := dnsName(msg, 14); !ok || n != "iphone.local" || next != 16 {
		t.Fatalf("dnsName = %q %d %v", n, next, ok)
	}
	if cleanHostName("Lan-iPhone.local.", "192.168.1.5") != "Lan-iPhone" || cleanHostName("192-168-1-5.isp.vn", "192.168.1.5") != "" {
		t.Fatal("cleanHostName")
	}
}

func newClientHub(t *testing.T) (*Hub, *RState, *Store) {
	dir := t.TempDir()
	st, err := OpenStore(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := LoadBox(filepath.Join(dir, "key"))
	settings, _ := NewSettingsStore(st, box)
	h := NewHub(st, box, settings, false)
	h.resolver.lookup = func(context.Context, string) (string, string) { return "", "" }
	r := &Router{Name: "R", Mode: "push", Enabled: true, WANs: []WAN{{Iface: "pppoe-out1", Label: "VNPT"}}}
	if err := st.SaveRouter(r); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); h.Wait() })
	if err := h.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return h, h.get(r.ID), st
}

func TestClientPresenceAndNames(t *testing.T) {
	h, rs, st := newClientHub(t)
	t0 := nowMs()
	h.observeSection(rs, "lease", []Row{
		{"address": "192.168.1.20", "mac-address": "DC:A6:32:00:00:01", "host-name": "pi", "status": "bound"},
		{"address": "192.168.1.21", "mac-address": "48:EA:63:00:00:02", "status": "bound"},
	}, t0)
	h.observeSection(rs, "arp", []Row{
		{"address": "192.168.1.20", "mac-address": "DC:A6:32:00:00:01", "status": "reachable", "interface": "bridge"},
		{"address": "192.168.1.21", "mac-address": "48:EA:63:00:00:02", "status": "stale", "interface": "bridge"},
		{"address": "192.168.1.30", "mac-address": "C4:AD:34:00:00:03", "status": "reachable", "interface": "bridge"},
	}, t0)
	h.observeSection(rs, "bridge_host", []Row{{"mac-address": "48:EA:63:00:00:02", "on-interface": "ether3"}}, t0)
	h.observeSection(rs, "dns_static", []Row{{"name": "router-tang2", "address": "192.168.1.30"}}, t0)
	h.updateClients(rs, t0)

	c := map[string]Client{}
	rs.mu.RLock()
	list := rs.clientList()
	rs.mu.RUnlock()
	for _, x := range list {
		c[x.IP] = x
	}
	if c["192.168.1.20"].Name != "pi" || c["192.168.1.20"].NameSrc != "dhcp" || !c["192.168.1.20"].Online {
		t.Fatalf("pi = %+v", c["192.168.1.20"])
	}
	if c21 := c["192.168.1.21"]; !c21.Online || c21.Port != "ether3" || c21.Vendor != "Zhejiang Uniview" || c21.Name != "" {
		t.Fatalf("camera via bridge host = %+v", c21)
	}
	if c["192.168.1.30"].Name != "router-tang2" || c["192.168.1.30"].NameSrc != "dns" {
		t.Fatalf("dns static name = %+v", c["192.168.1.30"])
	}
	evs, _ := st.ListEvents(nil, 10)
	if len(evs) != 0 {
		t.Fatalf("first discovery must not create 'new device' events: %+v", evs)
	}

	// pi goes away: ARP fails → offline, since = last seen, previous session kept
	t1 := t0 + 120_000
	h.observeSection(rs, "arp", []Row{{"address": "192.168.1.20", "mac-address": "DC:A6:32:00:00:01", "status": "failed"}}, t1)
	h.observeSection(rs, "bridge_host", nil, t1)
	h.updateClients(rs, t1)
	pi := clientOf(rs, "DC:A6:32:00:00:01")
	if pi.Online || pi.Since != t0 || pi.LastSession != 0 {
		t.Fatalf("pi offline = %+v", pi)
	}
	// camera has no traffic for > 5 minutes → offline by idle timeout
	h.updateClients(rs, t0+clientIdle+60_000)
	if cam := clientOf(rs, "48:EA:63:00:00:02"); cam.Online {
		t.Fatalf("idle camera still online: %+v", cam)
	}

	// a brand-new device now raises an event
	t2 := t0 + clientIdle + 120_000
	h.observeSection(rs, "arp", []Row{{"address": "192.168.1.99", "mac-address": "BE:3D:31:00:00:09", "status": "reachable"}}, t2)
	h.updateClients(rs, t2)
	evs, _ = st.ListEvents(nil, 10)
	if len(evs) != 1 || !strings.Contains(evs[0].Msg, "Thiết bị mới vào mạng: thiết bị dùng MAC ngẫu nhiên – 192.168.1.99") {
		t.Fatalf("new device event = %+v", evs)
	}

	// manual name wins and survives a reload from the database
	if err := h.SetAlias(rs.id, "48:ea:63:00:00:02", "Camera cổng"); err != nil {
		t.Fatal(err)
	}
	if cam := clientOf(rs, "48:EA:63:00:00:02"); cam.Name != "Camera cổng" || cam.NameSrc != "alias" {
		t.Fatalf("alias = %+v", cam)
	}
	rs.mu.RLock()
	cfg := rs.cfg
	rs.mu.RUnlock()
	rs2 := newRState(cfg, nil)
	if err := h.loadClients(rs2); err != nil || len(rs2.Clients) != 4 || rs2.aliases["48:EA:63:00:00:02"] != "Camera cổng" || !rs2.clientsReady {
		t.Fatalf("reload: %v clients=%d aliases=%v", err, len(rs2.Clients), rs2.aliases)
	}
}

func clientOf(rs *RState, mac string) Client {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	if c := rs.Clients[mac]; c != nil {
		return *c
	}
	return Client{}
}

func TestRouterLifecycleEvents(t *testing.T) {
	h, rs, st := newClientHub(t)
	t0 := nowMs()
	ifs := func(running string) []Row {
		return []Row{{"name": "ether2", "type": "ether", "running": running, "comment": "Switch tầng 1", "rx-byte": "1", "tx-byte": "1"}}
	}
	h.observeFast(rs, Row{"uptime": "1d", "version": "7.18 (stable)", "board-name": "hEX"}, ifs("true"), t0)
	h.observeFast(rs, Row{"uptime": "1d00:00:15", "version": "7.18 (stable)"}, ifs("false"), t0+15_000)
	h.observeFast(rs, Row{"uptime": "1d00:00:30", "version": "7.18 (stable)"}, ifs("false"), t0+30_000)
	// reboot + upgrade: uptime back to 1 minute
	h.observeFast(rs, Row{"uptime": "1m", "version": "7.19 (stable)"}, ifs("true"), t0+600_000)
	h.observeSection(rs, "address", []Row{{"address": "113.1.1.1/32", "interface": "pppoe-out1"}}, t0)
	h.observeSection(rs, "address", []Row{{"address": "113.2.2.2/32", "interface": "pppoe-out1"}}, t0+1000)

	evs, _ := st.ListEvents(nil, 20)
	var msgs []string
	for i := len(evs) - 1; i >= 0; i-- {
		msgs = append(msgs, evs[i].Msg)
	}
	all := strings.Join(msgs, "\n")
	for _, want := range []string{
		"Bắt đầu giám sát router (hEX · RouterOS 7.18 (stable))",
		"Cổng ether2 (Switch tầng 1): mất link",
		"Router vừa khởi động lại",
		"RouterOS đổi phiên bản: 7.18 (stable) → 7.19 (stable)",
		"Cổng ether2 (Switch tầng 1): có link trở lại",
		"IP public của WAN VNPT (pppoe-out1) đổi: 113.1.1.1 → 113.2.2.2",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing event %q in:\n%s", want, all)
		}
	}
	if v, _ := h.View(rs.id); v.WANs[0].PublicIP != "113.2.2.2" {
		t.Fatalf("public ip = %q", v.WANs[0].PublicIP)
	}
}

// fakeBot imitates the Telegram-style bot API used by Zalo and Telegram.
type fakeBot struct {
	mu   sync.Mutex
	sent []map[string]any
	path []string
}

func (f *fakeBot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.path = append(f.path, r.URL.Path)
	switch {
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		f.sent = append(f.sent, body)
		io.WriteString(w, `{"ok":true,"result":{"message_id":"1","date":1}}`)
	case strings.HasSuffix(r.URL.Path, "/getUpdates"):
		// Zalo: a single update object, no update_id
		io.WriteString(w, `{"ok":true,"result":{"event_name":"message.text.received","message":{"message_id":"m1","text":"id","chat":{"id":"zl-123","chat_type":"PRIVATE"},"from":{"display_name":"Anh Lan"}}}}`)
	default:
		w.WriteHeader(404)
		io.WriteString(w, `{"ok":false,"error_code":404,"description":"not found"}`)
	}
}

func TestZaloChannel(t *testing.T) {
	dir := t.TempDir()
	st, _ := OpenStore(filepath.Join(dir, "db"))
	defer st.Close()
	box, _ := LoadBox(filepath.Join(dir, "key"))
	settings, _ := NewSettingsStore(st, box)
	tok := "1234567:abcdEFGH-xyz"
	if err := settings.Update(SettingsInput{ZaloToken: &tok, ZaloAdmin: "zl-admin", SummaryAt: "08:00", RepeatMin: 30, Timezone: "Asia/Ho_Chi_Minh", RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	bot := &fakeBot{}
	srv := httptest.NewServer(bot)
	defer srv.Close()
	n := NewNotifier(st, settings)
	n.base["zalo"] = srv.URL

	n.Alert(Targets{Zalo: "zl-group"}, "🔴 [R] WAN mất Internet")
	n.Alert(Targets{Zalo: "zl-group"}, "🟢 [R] WAN có Internet")
	n.flush(context.Background())
	bot.mu.Lock()
	if len(bot.sent) != 2 || bot.path[0] != "/bot"+tok+"/sendMessage" {
		t.Fatalf("sent=%v path=%v", bot.sent, bot.path)
	}
	if bot.sent[0]["chat_id"] != "zl-admin" || bot.sent[0]["text"] != "🔴 [R] WAN mất Internet\n\n🟢 [R] WAN có Internet" || bot.sent[1]["chat_id"] != "zl-group" {
		t.Fatalf("payload = %v", bot.sent)
	}
	bot.mu.Unlock()
	if left, _ := st.DueOutbox(nowMs()+1, 10); len(left) != 0 {
		t.Fatalf("outbox not drained: %v", left)
	}
	// Telegram has no token: its messages are not sent but also not lost
	n.Send("telegram", "999", "x")
	n.flush(context.Background())
	if left, _ := st.DueOutbox(nowMs()+1, 10); len(left) != 1 || left[0].Channel != "telegram" {
		t.Fatalf("telegram message must wait for a token: %v", left)
	}

	// bot loop: a single-object getUpdates result is understood, the chat is
	// remembered and "id" is answered once (no duplicate replies)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	go n.runBot(ctx, "zalo")
	waitFor(t, "recent zalo chat", func() bool {
		r := n.Recent()
		return len(r) == 1 && r[0].ChatID == "zl-123" && r[0].Name == "Anh Lan"
	})
	time.Sleep(4 * time.Second) // let it poll again: same message must not be answered twice
	n.flush(context.Background())
	bot.mu.Lock()
	defer bot.mu.Unlock()
	replies := 0
	for _, m := range bot.sent {
		if m["chat_id"] == "zl-123" && strings.Contains(m["text"].(string), "Chat ID của cuộc trò chuyện này: zl-123") {
			replies++
		}
	}
	if replies != 1 {
		t.Fatalf("expected exactly one reply, got %d: %v", replies, bot.sent)
	}
}
