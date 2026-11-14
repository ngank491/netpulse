package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- read-only configuration snapshot for audits ----------

// auditPath is one RouterOS menu read for the audit. Props is an allowlist of
// fields: anything else (keys, passwords, PSKs) is never requested.
type auditPath struct {
	Path  string
	Title string
	Props []string
	Max   int  // keep at most this many rows (the newest for logs)
	Tail  bool // keep the last rows instead of the first
}

var auditPaths = []auditPath{
	{"/system/resource", "Tài nguyên hệ thống", resourceProps, 1, false},
	{"/system/routerboard", "RouterBOARD / firmware", []string{"routerboard", "model", "current-firmware", "upgrade-firmware", "firmware-type"}, 1, false},
	{"/system/package/update", "Kênh cập nhật RouterOS", []string{"channel", "installed-version", "latest-version", "status"}, 1, false},
	{"/system/clock", "Đồng hồ", []string{"time-zone-name", "time-zone-autodetect"}, 1, false},
	{"/system/ntp/client", "NTP client", []string{"enabled", "mode", "servers", "status"}, 1, false},
	{"/system/health", "Cảm biến", []string{"name", "value", "type"}, 50, false},
	{"/system/scheduler", "Scheduler", []string{"name", "on-event", "interval", "start-time", "policy", "disabled", "run-count", "owner", "comment"}, 100, false},
	{"/system/script", "Script", []string{"name", "owner", "policy", "run-count", "last-started", "source", "comment"}, 100, false},
	{"/user", "Tài khoản", []string{"name", "group", "address", "disabled", "last-logged-in", "comment"}, 100, false},
	{"/user/group", "Nhóm quyền", []string{"name", "policy"}, 50, false},
	{"/user/active", "Phiên đăng nhập hiện tại", []string{"name", "address", "via", "when"}, 50, false},
	{"/ip/service", "Dịch vụ quản trị", []string{"name", "port", "address", "disabled", "certificate", "invalid"}, 20, false},
	{"/ip/ssh", "SSH", []string{"strong-crypto", "forwarding-enabled", "always-allow-password-login", "host-key-type"}, 1, false},
	{"/ip/dns", "DNS", []string{"allow-remote-requests", "servers", "dynamic-servers", "use-doh-server", "cache-size"}, 1, false},
	{"/ip/socks", "SOCKS proxy", []string{"enabled", "port", "version"}, 1, false},
	{"/ip/proxy", "Web proxy", []string{"enabled", "port"}, 1, false},
	{"/ip/upnp", "UPnP", []string{"enabled", "allow-disable-external-interface"}, 1, false},
	{"/ip/cloud", "IP Cloud / DDNS", []string{"ddns-enabled", "update-time", "public-address", "status"}, 1, false},
	{"/ip/settings", "Cài đặt IP", []string{"ip-forward", "rp-filter", "tcp-syncookies", "allow-fast-path"}, 1, false},
	{"/ip/firewall/filter", "Firewall filter", []string{"chain", "action", "protocol", "dst-port", "src-address", "dst-address", "src-address-list", "dst-address-list", "in-interface", "in-interface-list", "out-interface-list", "connection-state", "connection-nat-state", "jump-target", "disabled", "dynamic", "log", "comment", "packets"}, 300, false},
	{"/ip/firewall/nat", "Firewall NAT", []string{"chain", "action", "protocol", "dst-port", "src-address", "dst-address", "in-interface", "in-interface-list", "out-interface", "out-interface-list", "to-addresses", "to-ports", "disabled", "comment"}, 200, false},
	{"/ip/firewall/raw", "Firewall raw", []string{"chain", "action", "protocol", "dst-port", "src-address", "src-address-list", "in-interface", "in-interface-list", "disabled", "comment"}, 100, false},
	{"/ip/firewall/connection/tracking", "Connection tracking", []string{"enabled", "total-entries", "max-entries", "tcp-established-timeout", "udp-timeout"}, 1, false},
	{"/interface", "Interface (bộ đếm lỗi)", []string{"name", "type", "running", "disabled", "rx-error", "tx-error", "rx-drop", "tx-drop", "rx-packet", "tx-packet", "link-downs", "comment"}, 300, false},
	{"/interface/list", "Interface list", []string{"name", "include", "exclude"}, 50, false},
	{"/interface/list/member", "Thành viên interface list", []string{"list", "interface", "disabled"}, 200, false},
	{"/interface/wireless", "Wi-Fi (wireless)", []string{"name", "ssid", "mode", "security-profile", "wps-mode", "disabled"}, 30, false},
	{"/interface/wireless/security-profiles", "Wi-Fi security profile", []string{"name", "mode", "authentication-types", "unicast-ciphers"}, 30, false},
	{"/interface/wifi", "Wi-Fi (wifi)", []string{"name", "configuration.ssid", "security.authentication-types", "security.wps", "disabled"}, 30, false},
	{"/tool/mac-server", "MAC Telnet", []string{"allowed-interface-list"}, 1, false},
	{"/tool/mac-server/mac-winbox", "MAC Winbox", []string{"allowed-interface-list"}, 1, false},
	{"/tool/bandwidth-server", "Bandwidth test server", []string{"enabled", "authenticate"}, 1, false},
	{"/tool/romon", "RoMON", []string{"enabled"}, 1, false},
	{"/ip/neighbor/discovery-settings", "Neighbor discovery", []string{"discover-interface-list", "protocol"}, 1, false},
	{"/snmp", "SNMP", []string{"enabled", "trap-version", "contact", "location"}, 1, false},
	{"/snmp/community", "SNMP community", []string{"name", "addresses", "security", "read-access", "write-access", "disabled"}, 20, false},
	{"/certificate", "Chứng chỉ", []string{"name", "common-name", "invalid-after", "expires-after", "trusted", "expired"}, 50, false},
	{"/queue/simple", "Simple queue", []string{"name", "target", "max-limit", "disabled"}, 300, false},
	{"/queue/tree", "Queue tree", []string{"name", "parent", "max-limit", "disabled"}, 200, false},
	{"/ip/dhcp-server", "DHCP server", []string{"name", "interface", "address-pool", "lease-time", "disabled"}, 30, false},
	{"/log", "Log hệ thống (mới nhất)", []string{"time", "topics", "message"}, 300, true},
}

type auditSection struct {
	Title       string `json:"title"`
	Rows        []Row  `json:"rows"`
	Total       int    `json:"total"`
	Unsupported bool   `json:"unsupported,omitempty"`
	Error       string `json:"error,omitempty"`
}

type auditSnapshot struct {
	At       int64                    `json:"at"`
	Live     bool                     `json:"live"` // read from the router now (REST); false = only data NetPulse already had
	Sections map[string]*auditSection `json:"sections"`
}

var (
	reSecretKey = regexp.MustCompile(`(?i)(password|passwd|passphrase|secret|private-key|preshared|pre-shared|psk|wpa-pre|wpa2-pre|auth-key|token)`)
	reSecretVal = regexp.MustCompile(`(?i)((?:password|passwd|passphrase|secret|token|psk|key|user)\s*[=:]\s*)("[^"]*"|[^\s;"]+)`)
)

// redactRow drops any secret-looking field and masks inline credentials (e.g. in script sources).
func redactRow(r Row, maxVal int) Row {
	out := Row{}
	for k, v := range r {
		if reSecretKey.MatchString(k) {
			continue
		}
		v = reSecretVal.ReplaceAllString(v, "${1}***")
		if len(v) > maxVal {
			v = truncateUTF8(v, maxVal) + "…"
		}
		out[k] = v
	}
	return out
}

// collectAudit reads every audit path (read-only GET) from the router.
func collectAudit(ctx context.Context, src Source) *auditSnapshot {
	snap := &auditSnapshot{At: nowMs(), Live: true, Sections: map[string]*auditSection{}}
	for _, p := range auditPaths {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		rows, err := src.Fetch(cctx, p.Path, p.Props)
		cancel()
		sec := &auditSection{Title: p.Title}
		switch {
		case errors.Is(err, errNotSupported):
			sec.Unsupported = true
		case err != nil:
			sec.Error = err.Error()
		default:
			sec.Total = len(rows)
			if len(rows) > p.Max {
				if p.Tail {
					rows = rows[len(rows)-p.Max:]
				} else {
					rows = rows[:p.Max]
				}
			}
			maxVal := 300
			if p.Path == "/system/script" || p.Path == "/system/scheduler" {
				maxVal = 600
			}
			for _, r := range rows {
				sec.Rows = append(sec.Rows, redactRow(r, maxVal))
			}
		}
		snap.Sections[p.Path] = sec
		if ctx.Err() != nil {
			break
		}
	}
	return snap
}

// snapshotFromState builds a reduced snapshot for push routers (no REST access).
func snapshotFromState(rs *RState) *auditSnapshot {
	snap := &auditSnapshot{At: nowMs(), Sections: map[string]*auditSection{}}
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	add := func(path, title string, rows []Row) {
		snap.Sections[path] = &auditSection{Title: title, Rows: rows, Total: len(rows)}
	}
	if rs.Resource != nil {
		add("/system/resource", "Tài nguyên hệ thống", []Row{rs.Resource})
	}
	var ifs []Row
	for _, i := range rs.Ifaces {
		ifs = append(ifs, Row{"name": i.Name, "type": i.Type, "running": strconv.FormatBool(i.Running), "disabled": strconv.FormatBool(i.Disabled)})
	}
	add("/interface", "Interface", ifs)
	add("/system/health", "Cảm biến", rs.Sections["health"])
	add("/ip/address", "Địa chỉ IP", rs.Sections["address"])
	add("/ip/route", "Route mặc định", rs.Sections["route"])
	return snap
}

func (s *auditSnapshot) rows(path string) []Row {
	if sec := s.Sections[path]; sec != nil {
		return sec.Rows
	}
	return nil
}

func (s *auditSnapshot) one(path string) Row {
	if r := s.rows(path); len(r) > 0 {
		return r[0]
	}
	return nil
}

func (s *auditSnapshot) has(path string) bool {
	sec := s.Sections[path]
	return sec != nil && !sec.Unsupported && sec.Error == ""
}

// ---------- performance & reliability stats from NetPulse's own history ----------

type statSummary struct {
	Avg float64 `json:"avg"`
	P95 float64 `json:"p95"`
	Max float64 `json:"max"`
	N   int     `json:"samples"`
}

func summarize(vals []float64) statSummary {
	if len(vals) == 0 {
		return statSummary{}
	}
	s := append([]float64{}, vals...)
	sort.Float64s(s)
	sum := 0.0
	for _, v := range s {
		sum += v
	}
	r := func(f float64) float64 { return math.Round(f*10) / 10 }
	return statSummary{Avg: r(sum / float64(len(s))), P95: r(s[int(float64(len(s)-1)*0.95)]), Max: r(s[len(s)-1]), N: len(s)}
}

type perfStats struct {
	Hours    int                       `json:"hours"`
	CPU      statSummary               `json:"cpuPercent"`
	Mem      statSummary               `json:"memPercent"`
	Ifaces   map[string][2]statSummary `json:"interfacesBps,omitempty"` // [rx, tx]
	Warnings int                       `json:"warningEvents"`
	ByTopic  map[string]int            `json:"warningsByTopic"`
	Devices  []map[string]any          `json:"deviceAvailability,omitempty"`
}

func (h *Hub) perfStats(routerID int64, hours int, iface string) (perfStats, error) {
	out := perfStats{Hours: hours, ByTopic: map[string]int{}}
	since := nowMs() - int64(hours)*3600_000
	samples, err := h.st.Samples(routerID, since)
	if err != nil {
		return out, err
	}
	var cpu, mem []float64
	rx, tx := map[string][]float64{}, map[string][]float64{}
	for _, s := range samples {
		if s.CPU.Valid {
			cpu = append(cpu, s.CPU.Float64)
		}
		if s.Mem.Valid {
			mem = append(mem, s.Mem.Float64)
		}
		var d map[string][2]float64
		if json.Unmarshal([]byte(s.Data), &d) == nil {
			for name, v := range d {
				if iface == "" || iface == name {
					rx[name] = append(rx[name], v[0])
					tx[name] = append(tx[name], v[1])
				}
			}
		}
	}
	out.CPU, out.Mem = summarize(cpu), summarize(mem)
	out.Ifaces = map[string][2]statSummary{}
	type peak struct {
		name string
		p95  float64
	}
	var peaks []peak
	for name := range rx {
		out.Ifaces[name] = [2]statSummary{summarize(rx[name]), summarize(tx[name])}
		peaks = append(peaks, peak{name, out.Ifaces[name][0].P95 + out.Ifaces[name][1].P95})
	}
	if iface == "" && len(peaks) > 8 { // keep the 8 busiest interfaces
		sort.Slice(peaks, func(i, j int) bool { return peaks[i].p95 > peaks[j].p95 })
		for _, p := range peaks[8:] {
			delete(out.Ifaces, p.name)
		}
	}
	evs, err := h.st.ListEvents([]int64{routerID}, 2000)
	if err != nil {
		return out, err
	}
	for _, e := range evs {
		if e.At < since || e.Level != "warning" {
			continue
		}
		out.Warnings++
		topic := "khác"
		switch {
		case strings.Contains(e.Msg, "WAN"):
			topic = "WAN"
		case strings.HasPrefix(e.Msg, "Cổng"):
			topic = "cổng"
		case strings.Contains(e.Msg, "router") || strings.Contains(e.Msg, "Router"):
			topic = "router"
		case strings.Contains(e.Msg, "mất kết nối"):
			topic = "thiết bị"
		}
		out.ByTopic[topic]++
	}
	avail, err := h.st.Availability(routerID, since)
	if err == nil {
		mons, _ := h.st.ListMonitors(routerID)
		for _, m := range mons {
			if a, ok := avail[m.ID]; ok {
				out.Devices = append(out.Devices, map[string]any{"name": m.Name, "ip": m.IP, "availabilityPercent": math.Round(a*10000) / 100})
			}
		}
	}
	return out, nil
}

// ---------- built-in rule checks ----------

type Finding struct {
	Severity string   `json:"severity"` // critical | high | medium | low | info
	Area     string   `json:"area"`     // security | performance | reliability
	Title    string   `json:"title"`
	Evidence string   `json:"evidence"`
	Fix      string   `json:"fix"`
	Commands []string `json:"commands"`
}

var severityRank = map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3, "info": 4}
var severityWeight = map[string]int{"critical": 30, "high": 15, "medium": 7, "low": 3, "info": 0}

// scores turns finding penalties into 0-100 with diminishing returns:
// one critical issue ≈ 67, a pile of serious issues approaches 0 without hitting it at once.
func scores(fs []Finding) (sec, perf int) {
	var ps, pp float64
	for _, f := range fs {
		if f.Area == "security" {
			ps += float64(severityWeight[f.Severity])
		} else {
			pp += float64(severityWeight[f.Severity])
		}
	}
	return int(math.Round(100 * 60 / (60 + ps))), int(math.Round(100 * 60 / (60 + pp)))
}

func on(v string) bool { return v == "true" || v == "yes" }

func ruleChecks(snap *auditSnapshot, perf perfStats, wans []string) []Finding {
	var fs []Finding
	add := func(sev, area, title, evidence, fix string, cmds ...string) {
		fs = append(fs, Finding{sev, area, title, evidence, fix, cmds})
	}
	filter := snap.rows("/ip/firewall/filter")
	active := func(r Row) bool { return !on(r["disabled"]) && !on(r["dynamic"]) }

	// --- firewall input protection from the Internet
	wanSet := map[string]bool{}
	for _, w := range wans {
		wanSet[w] = true
	}
	inputDropsWAN := false
	for _, r := range filter {
		if !active(r) || r["chain"] != "input" || (r["action"] != "drop" && r["action"] != "reject") {
			continue
		}
		catchAll := r["protocol"] == "" && r["dst-port"] == "" && r["src-address"] == "" && r["src-address-list"] == "" && r["connection-state"] == "" && r["in-interface"] == "" && r["in-interface-list"] == ""
		fromWAN := r["in-interface-list"] == "WAN" || r["in-interface-list"] == "!LAN" || wanSet[r["in-interface"]]
		if catchAll || (fromWAN && r["protocol"] == "" && r["dst-port"] == "") {
			inputDropsWAN = true
		}
	}
	if snap.has("/ip/firewall/filter") && !inputDropsWAN {
		add("critical", "security", "Firewall không chặn truy cập vào router từ Internet",
			fmt.Sprintf("Chain input có %d rule nhưng không có rule drop chung cho lưu lượng từ WAN.", countChain(filter, "input")),
			"Thêm rule cho phép kết nối đã thiết lập, cho phép LAN, rồi chặn mọi thứ còn lại từ WAN. Bật Safe Mode (Ctrl+X) trước khi sửa firewall để không tự khoá mình.",
			`/ip firewall filter add chain=input action=accept connection-state=established,related,untracked comment="accept established"`,
			`/ip firewall filter add chain=input action=drop connection-state=invalid comment="drop invalid"`,
			`/ip firewall filter add chain=input action=accept protocol=icmp comment="accept ICMP"`,
			`/ip firewall filter add chain=input action=drop in-interface-list=!LAN comment="drop all not coming from LAN"`)
	}
	// --- management services
	for _, s := range snap.rows("/ip/service") {
		if on(s["disabled"]) {
			continue
		}
		name, open := s["name"], s["address"] == ""
		switch name {
		case "telnet":
			add("high", "security", "Dịch vụ Telnet đang bật (mật khẩu truyền không mã hoá)", "/ip service telnet port "+s["port"]+", address="+orAll(s["address"]),
				"Tắt Telnet, dùng SSH hoặc WinBox.", "/ip service disable telnet")
		case "ftp":
			add("medium", "security", "Dịch vụ FTP đang bật", "/ip service ftp port "+s["port"]+", address="+orAll(s["address"]),
				"Tắt FTP nếu không dùng để chép file.", "/ip service disable ftp")
		case "api":
			add("medium", "security", "API không mã hoá (cổng 8728) đang bật", "address="+orAll(s["address"]),
				"Tắt API thường; nếu cần dùng api-ssl và giới hạn địa chỉ.", "/ip service disable api")
		case "www":
			sev := "medium"
			if !open {
				sev = "low"
			}
			add(sev, "security", "WebFig qua HTTP (không mã hoá) đang bật", "address="+orAll(s["address"]),
				"Dùng www-ssl thay cho www, hoặc ít nhất giới hạn địa chỉ quản trị.", "/ip service set www address=192.168.88.0/24")
		}
		if open && (name == "winbox" || name == "ssh" || name == "www-ssl" || name == "api-ssl" || name == "www" || name == "api") {
			sev := "medium"
			if !inputDropsWAN {
				sev = "high"
			}
			add(sev, "security", "Dịch vụ quản trị "+name+" cho phép mọi địa chỉ", "/ip service "+name+" (port "+s["port"]+") address trống",
				"Giới hạn địa chỉ được phép quản trị (mạng LAN/VPN của quản trị viên).", "/ip service set "+name+" address=192.168.88.0/24,10.0.0.0/24")
		}
	}
	// --- users
	for _, u := range snap.rows("/user") {
		if on(u["disabled"]) {
			continue
		}
		if u["name"] == "admin" {
			add("medium", "security", "Tài khoản mặc định \"admin\" vẫn đang dùng", "user admin, group="+u["group"],
				"Tạo tài khoản quản trị tên khác với mật khẩu mạnh, đăng nhập thử, rồi tắt admin.",
				`/user add name=quantri group=full password="<MatKhauManh>"`, "/user disable admin")
		}
		if u["group"] == "full" && u["address"] == "" && u["name"] != "admin" {
			add("low", "security", "Tài khoản toàn quyền \""+u["name"]+"\" đăng nhập được từ mọi địa chỉ", "group=full, address trống",
				"Giới hạn địa chỉ đăng nhập của tài khoản quản trị.", "/user set "+u["name"]+" address=192.168.88.0/24")
		}
	}
	// --- DNS open resolver
	if dns := snap.one("/ip/dns"); on(dns["allow-remote-requests"]) && !inputDropsWAN {
		add("high", "security", "DNS của router trả lời cả truy vấn từ Internet (open resolver)", "allow-remote-requests=yes và firewall input không chặn WAN",
			"Chặn DNS từ WAN (hoặc tắt allow-remote-requests nếu LAN không dùng router làm DNS).",
			"/ip firewall filter add chain=input in-interface-list=WAN protocol=udp dst-port=53 action=drop comment=\"block DNS from WAN\"",
			"/ip firewall filter add chain=input in-interface-list=WAN protocol=tcp dst-port=53 action=drop comment=\"block DNS from WAN\"")
	}
	// --- risky features
	if on(snap.one("/ip/socks")["enabled"]) {
		add("critical", "security", "SOCKS proxy đang bật", "Thường do mã độc bật lên để biến router thành proxy",
			"Tắt SOCKS, kiểm tra scheduler/script lạ và đổi mật khẩu mọi tài khoản.", "/ip socks set enabled=no")
	}
	if on(snap.one("/ip/proxy")["enabled"]) {
		add("medium", "security", "Web proxy đang bật", "port "+snap.one("/ip/proxy")["port"],
			"Tắt nếu không cố ý dùng; nếu dùng, chặn truy cập proxy từ WAN.", "/ip proxy set enabled=no")
	}
	if on(snap.one("/ip/upnp")["enabled"]) {
		add("medium", "security", "UPnP đang bật", "Thiết bị trong LAN có thể tự mở cổng ra Internet",
			"Tắt UPnP nếu không cần (game console, camera P2P…).", "/ip upnp set enabled=no")
	}
	if l := snap.one("/tool/mac-server/mac-winbox")["allowed-interface-list"]; l == "all" {
		add("medium", "security", "MAC-WinBox cho phép trên mọi interface", "allowed-interface-list=all",
			"Chỉ cho phép MAC-WinBox trên LAN.", "/tool mac-server mac-winbox set allowed-interface-list=LAN")
	}
	if l := snap.one("/tool/mac-server")["allowed-interface-list"]; l == "all" {
		add("low", "security", "MAC-Telnet cho phép trên mọi interface", "allowed-interface-list=all",
			"Tắt hoặc chỉ cho phép trên LAN.", "/tool mac-server set allowed-interface-list=none")
	}
	if on(snap.one("/tool/bandwidth-server")["enabled"]) {
		add("low", "security", "Bandwidth-test server đang bật", "Có thể bị lợi dụng để làm nghẽn đường truyền",
			"Tắt khi không đo tốc độ.", "/tool bandwidth-server set enabled=no")
	}
	if on(snap.one("/tool/romon")["enabled"]) {
		add("low", "security", "RoMON đang bật", "", "Tắt nếu không quản trị router qua RoMON.", "/tool romon set enabled=no")
	}
	if l := snap.one("/ip/neighbor/discovery-settings")["discover-interface-list"]; l == "all" {
		add("low", "security", "Neighbor discovery quảng bá trên mọi interface (cả WAN)", "discover-interface-list=all",
			"Chỉ quảng bá trong LAN.", "/ip neighbor discovery-settings set discover-interface-list=LAN")
	}
	if on(snap.one("/snmp")["enabled"]) {
		for _, c := range snap.rows("/snmp/community") {
			if !on(c["disabled"]) && c["name"] == "public" && (c["addresses"] == "" || c["addresses"] == "::/0" || strings.Contains(c["addresses"], "0.0.0.0/0")) {
				add("medium", "security", "SNMP dùng community mặc định \"public\" cho mọi địa chỉ", "addresses="+orAll(c["addresses"]),
					"Đổi tên community và giới hạn địa chỉ máy giám sát.", "/snmp community set [find name=public] addresses=192.168.88.10/32")
			}
		}
	}
	if s := snap.one("/ip/ssh"); s != nil && s["strong-crypto"] == "false" {
		add("low", "security", "SSH chưa bật strong-crypto", "strong-crypto=no", "Bật mã hoá mạnh cho SSH.", "/ip ssh set strong-crypto=yes")
	}
	// --- versions & firmware
	if u := snap.one("/system/package/update"); u != nil {
		inst, latest := u["installed-version"], u["latest-version"]
		if inst != "" && latest != "" && versionLess(inst, latest) {
			add("medium", "security", "RouterOS chưa cập nhật bản mới nhất ("+inst+" → "+latest+")", "kênh "+u["channel"],
				"Sao lưu cấu hình, rồi cập nhật vào giờ thấp điểm.", "/system backup save name=truoc-cap-nhat", "/system package update install")
		}
	}
	if rb := snap.one("/system/routerboard"); rb != nil && rb["current-firmware"] != "" && rb["upgrade-firmware"] != "" && rb["current-firmware"] != rb["upgrade-firmware"] {
		add("low", "reliability", "Firmware RouterBOARD chưa khớp RouterOS", rb["current-firmware"]+" → "+rb["upgrade-firmware"],
			"Nâng firmware rồi khởi động lại router.", "/system routerboard upgrade", "/system reboot")
	}
	if n := snap.one("/system/ntp/client"); n != nil && n["enabled"] == "false" {
		add("low", "reliability", "NTP client đang tắt (giờ router có thể sai, log khó đối chiếu)", "enabled=no",
			"Bật NTP.", "/system ntp client set enabled=yes servers=time.google.com,pool.ntp.org")
	}
	if on(snap.one("/ip/cloud")["ddns-enabled"]) {
		add("info", "security", "IP Cloud DDNS đang bật", "Tên miền *.sn.mynetname.net trỏ về IP public của router",
			"Chỉ để bật khi cần truy cập từ xa; đảm bảo firewall input chặn WAN.")
	}
	// --- port forwards of risky services
	risky := map[string]string{"21": "FTP", "22": "SSH", "23": "Telnet", "139": "NetBIOS", "445": "SMB", "3389": "Remote Desktop", "5900": "VNC", "8291": "WinBox", "3306": "MySQL", "1433": "SQL Server"}
	for _, r := range snap.rows("/ip/firewall/nat") {
		if !active(r) || r["chain"] != "dstnat" || r["action"] != "dst-nat" {
			continue
		}
		port := firstNonEmpty(r["to-ports"], r["dst-port"])
		for _, p := range strings.Split(r["dst-port"]+","+port, ",") {
			if svc, ok := risky[strings.TrimSpace(p)]; ok && r["src-address"] == "" && r["src-address-list"] == "" {
				add("high", "security", "Mở cổng "+svc+" ("+strings.TrimSpace(p)+") từ Internet vào "+r["to-addresses"],
					"NAT dstnat dst-port="+r["dst-port"]+" → "+r["to-addresses"]+":"+r["to-ports"]+" "+r["comment"],
					"Không mở trực tiếp các dịch vụ này ra Internet. Dùng VPN (WireGuard), hoặc ít nhất giới hạn src-address.",
					"/ip firewall nat disable [find where chain=dstnat dst-port=\""+r["dst-port"]+"\"]")
				break
			}
		}
	}
	// --- scripts / schedulers that download and run code (common MikroTik malware pattern)
	suspicious := regexp.MustCompile(`(?i)(/tool fetch|fetch url=).*(/import|\[:parse)|(/import|:parse).*fetch|/ip socks|/ip proxy set enabled=yes|dst-path=.*\.rsc`)
	for _, sec := range []string{"/system/scheduler", "/system/script"} {
		for _, r := range snap.rows(sec) {
			body := firstNonEmpty(r["on-event"], r["source"])
			if r["name"] != "netpulse" && suspicious.MatchString(body) {
				add("high", "security", "Script/scheduler \""+r["name"]+"\" tải và chạy mã từ bên ngoài", truncateUTF8(body, 200),
					"Kiểm tra nguồn gốc. Nếu không do bạn tạo: xoá, đổi mật khẩu mọi tài khoản, cập nhật RouterOS.",
					"/system scheduler print detail", "/system script print detail")
			}
		}
	}
	// --- brute force attempts in the log
	reFail := regexp.MustCompile(`login failure for user (\S+) from (\S+) via (\S+)`)
	fails, srcs := 0, map[string]int{}
	for _, l := range snap.rows("/log") {
		if m := reFail.FindStringSubmatch(l["message"]); m != nil {
			fails++
			srcs[m[2]+" ("+m[3]+")"]++
		}
	}
	if fails >= 10 {
		add("high", "security", fmt.Sprintf("%d lần đăng nhập sai trong log gần đây (dấu hiệu dò mật khẩu)", fails), topCounts(srcs, 5),
			"Giới hạn địa chỉ quản trị (/ip service address=…), chặn WAN ở chain input, đổi mật khẩu mạnh.",
			`/ip firewall filter add chain=input in-interface-list=WAN protocol=tcp dst-port=8291,22 action=drop comment="block admin from WAN"`)
	}
	// --- Wi-Fi security
	for _, p := range snap.rows("/interface/wireless/security-profiles") {
		if p["mode"] == "none" {
			for _, w := range snap.rows("/interface/wireless") {
				if w["security-profile"] == p["name"] && !on(w["disabled"]) && w["mode"] != "station" {
					add("medium", "security", "Wi-Fi \""+w["ssid"]+"\" không đặt mật khẩu", "security-profile "+p["name"]+" mode=none",
						"Đặt WPA2/WPA3 cho mạng này (trừ khi là mạng khách có cô lập).", "/interface wireless security-profiles set "+p["name"]+" mode=dynamic-keys authentication-types=wpa2-psk")
				}
			}
		}
		legacy := false
		for _, t := range strings.Split(p["authentication-types"], ",") {
			legacy = legacy || t == "wpa-psk" || t == "wpa-eap"
		}
		if legacy {
			add("low", "security", "Security profile \""+p["name"]+"\" còn cho phép WPA (đời cũ)", "authentication-types="+p["authentication-types"],
				"Chỉ dùng WPA2/WPA3.", "/interface wireless security-profiles set "+p["name"]+" authentication-types=wpa2-psk")
		}
	}
	for _, w := range snap.rows("/interface/wireless") {
		if !on(w["disabled"]) && w["wps-mode"] != "" && w["wps-mode"] != "disabled" {
			add("medium", "security", "WPS đang bật trên "+w["name"], "wps-mode="+w["wps-mode"], "Tắt WPS (dễ bị dò PIN).", "/interface wireless set "+w["name"]+" wps-mode=disabled")
		}
	}
	// --- certificates
	for _, c := range snap.rows("/certificate") {
		if on(c["expired"]) {
			add("low", "reliability", "Chứng chỉ \""+c["name"]+"\" đã hết hạn", "invalid-after="+c["invalid-after"], "Tạo/ký lại chứng chỉ và gán lại cho dịch vụ dùng nó.")
		} else if d := rosSeconds(c["expires-after"]); c["expires-after"] != "" && d > 0 && d < 30*86400 {
			add("low", "reliability", "Chứng chỉ \""+c["name"]+"\" sắp hết hạn", "còn "+humanDur(int64(d*1000)), "Gia hạn chứng chỉ trước khi hết hạn.")
		}
	}

	// --- performance
	hasFasttrack := false
	for _, r := range filter {
		if active(r) && r["chain"] == "forward" && r["action"] == "fasttrack-connection" {
			hasFasttrack = true
		}
	}
	queues := len(snap.rows("/queue/simple"))
	if snap.has("/ip/firewall/filter") && !hasFasttrack && queues == 0 {
		add("low", "performance", "Chưa bật FastTrack", "Chain forward không có rule fasttrack-connection",
			"FastTrack giảm mạnh tải CPU cho lưu lượng đã thiết lập (không dùng nếu có queue/hotspot cần đếm lưu lượng).",
			`/ip firewall filter add chain=forward action=fasttrack-connection connection-state=established,related hw-offload=yes comment="fasttrack" place-before=0`,
			`/ip firewall filter add chain=forward action=accept connection-state=established,related,untracked place-before=1`)
	}
	if queues > 300 {
		add("low", "performance", fmt.Sprintf("%d simple queue", snap.Sections["/queue/simple"].Total), "Nhiều simple queue làm tăng tải CPU", "Cân nhắc queue tree + PCQ cho số lượng lớn.")
	}
	if ct := snap.one("/ip/firewall/connection/tracking"); ct != nil {
		tot, _ := parseFloat(ct["total-entries"])
		mx, _ := parseFloat(ct["max-entries"])
		if mx > 0 && tot/mx > 0.8 {
			add("medium", "performance", "Bảng connection tracking gần đầy", fmt.Sprintf("%.0f/%.0f kết nối", tot, mx),
				"Tìm máy tạo nhiều kết nối (Firewall → Connections), giảm timeout, hoặc nâng RAM/thiết bị.", "/ip firewall connection tracking set tcp-established-timeout=1h")
		}
	}
	for _, i := range snap.rows("/interface") {
		if on(i["disabled"]) {
			continue
		}
		errs := num(i["rx-error"]) + num(i["tx-error"])
		pk := num(i["rx-packet"]) + num(i["tx-packet"])
		if errs > 100 && pk > 0 && errs/pk > 0.0005 {
			add("medium", "performance", "Cổng "+i["name"]+" có nhiều lỗi gói tin", fmt.Sprintf("%.0f lỗi / %.0f gói (%.3f%%)", errs, pk, errs/pk*100),
				"Kiểm tra cáp, đầu bấm, module SFP và chế độ tốc độ/duplex của thiết bị hai đầu.", "/interface ethernet monitor "+i["name"]+" once")
		}
		if ld := num(i["link-downs"]); ld >= 20 {
			add("low", "reliability", fmt.Sprintf("Cổng %s mất link %.0f lần", i["name"], ld), "link-downs="+i["link-downs"],
				"Thường do cáp lỏng, nguồn thiết bị đầu kia không ổn định hoặc lỗi tự thoả thuận tốc độ.")
		}
	}
	for _, s := range snap.rows("/system/health") {
		if strings.Contains(s["name"], "temperature") {
			if t, ok := parseFloat(s["value"]); ok && t >= 75 {
				add("medium", "reliability", fmt.Sprintf("Nhiệt độ cao: %s = %.0f°C", s["name"], t), "", "Kiểm tra thông gió, quạt, nhiệt độ phòng máy.")
			}
		}
	}
	if res := snap.one("/system/resource"); res != nil {
		free, ok1 := parseFloat(res["free-hdd-space"])
		total, ok2 := parseFloat(res["total-hdd-space"])
		if ok1 && ok2 && total > 0 && free/total < 0.1 {
			add("low", "reliability", "Bộ nhớ lưu trữ sắp đầy", fmt.Sprintf("còn %.0f%%", free/total*100), "Xoá file backup/log/gói cũ trong Files.")
		}
	}
	if perf.CPU.N > 30 && perf.CPU.P95 >= 80 {
		add("medium", "performance", fmt.Sprintf("CPU cao kéo dài (P95 %.0f%%, tối đa %.0f%% trong %d giờ)", perf.CPU.P95, perf.CPU.Max, perf.Hours), fmt.Sprintf("trung bình %.0f%%", perf.CPU.Avg),
			"Xem Tools → Profile để biết tiến trình tốn CPU; bật FastTrack; giảm rule firewall/mangle không cần thiết.", "/tool profile duration=10s")
	}
	if perf.Mem.N > 30 && perf.Mem.Max >= 90 {
		add("medium", "performance", fmt.Sprintf("RAM gần đầy (tối đa %.0f%%)", perf.Mem.Max), fmt.Sprintf("trung bình %.0f%%", perf.Mem.Avg),
			"Kiểm tra address-list lớn, connection tracking, gói không dùng.")
	}
	if n := perf.ByTopic["WAN"]; n >= 6 {
		add("medium", "reliability", fmt.Sprintf("WAN chập chờn: %d cảnh báo trong %d giờ", n, perf.Hours), "Xem trang Sự kiện để biết thời điểm", "Liên hệ nhà mạng, kiểm tra modem/ONT và cáp quang.")
	}
	for _, d := range perf.Devices {
		if a, _ := d["availabilityPercent"].(float64); a < 98 {
			add("low", "reliability", fmt.Sprintf("Thiết bị %v (%v) chỉ sẵn sàng %.1f%%", d["name"], d["ip"], a), "", "Kiểm tra nguồn, cáp và cổng switch của thiết bị.")
		}
	}
	sort.SliceStable(fs, func(i, j int) bool { return severityRank[fs[i].Severity] < severityRank[fs[j].Severity] })
	return fs
}

func countChain(rows []Row, chain string) (n int) {
	for _, r := range rows {
		if r["chain"] == chain && !on(r["disabled"]) {
			n++
		}
	}
	return
}

func orAll(a string) string {
	if a == "" {
		return "(mọi địa chỉ)"
	}
	return a
}

func num(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func topCounts(m map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	var l []kv
	for k, v := range m {
		l = append(l, kv{k, v})
	}
	sort.Slice(l, func(i, j int) bool { return l[i].v > l[j].v })
	var parts []string
	for i, x := range l {
		if i == n {
			break
		}
		parts = append(parts, fmt.Sprintf("%s: %d lần", x.k, x.v))
	}
	return strings.Join(parts, "; ")
}

// versionLess compares RouterOS versions like "7.18.2" < "7.19".
func versionLess(a, b string) bool {
	pa, pb := strings.Split(strings.Fields(a + " ")[0], "."), strings.Split(strings.Fields(b + " ")[0], ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(strings.TrimLeft(pa[i], "rcbeta"))
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(strings.TrimLeft(pb[i], "rcbeta"))
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// ---------- anonymiser: public IPs and MACs never leave the server ----------

type anonymizer struct {
	fwd map[string]string
	rev map[string]string
	n   map[string]int
}

var (
	reIPv4 = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	reMAC  = regexp.MustCompile(`\b[0-9A-Fa-f]{2}(?::[0-9A-Fa-f]{2}){5}\b`)
)

func newAnonymizer() *anonymizer {
	return &anonymizer{fwd: map[string]string{}, rev: map[string]string{}, n: map[string]int{}}
}

func (a *anonymizer) token(kind, v string) string {
	if t, ok := a.fwd[v]; ok {
		return t
	}
	a.n[kind]++
	t := fmt.Sprintf("%s-%d", kind, a.n[kind])
	a.fwd[v], a.rev[t] = t, v
	return t
}

func (a *anonymizer) Mask(s string) string {
	if a == nil {
		return s
	}
	s = reIPv4.ReplaceAllStringFunc(s, func(m string) string {
		ip, err := netip.ParseAddr(m)
		if err != nil || ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
			return m
		}
		if b := ip.As4(); b[0] == 100 && b[1]&0xC0 == 64 { // CGNAT 100.64/10 is not public either
			return m
		}
		if m == "8.8.8.8" || m == "1.1.1.1" || m == "8.8.4.4" || m == "1.0.0.1" {
			return m // well-known resolvers carry no private information
		}
		return a.token("PUBLIC-IP", m)
	})
	return reMAC.ReplaceAllStringFunc(s, func(m string) string { return a.token("MAC", strings.ToUpper(m)) })
}

func (a *anonymizer) Unmask(s string) string {
	if a == nil || len(a.rev) == 0 {
		return s
	}
	keys := make([]string, 0, len(a.rev))
	for k := range a.rev {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) }) // PUBLIC-IP-12 before PUBLIC-IP-1
	for _, k := range keys {
		s = strings.ReplaceAll(s, k, a.rev[k])
	}
	return s
}
