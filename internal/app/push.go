package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ---------- push ingestion (router runs a script and POSTs to us) ----------

// flexInt accepts 2, "2" or missing (→ -1) because RouterOS serialisation
// varies between versions.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = -1
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		*f = -1
		return nil
	}
	*f = flexInt(n)
	return nil
}

type pushMsg struct {
	V        int              `json:"v"`
	Kind     string           `json:"kind"`
	Identity string           `json:"identity"`
	Resource map[string]any   `json:"resource"`
	Ifaces   []map[string]any `json:"ifaces"`
	Ping     []struct {
		ID flexInt `json:"id"`
		R  flexInt `json:"r"`
	} `json:"ping"`
	Wan []struct {
		If string  `json:"if"`
		R  flexInt `json:"r"`
	} `json:"wan"`
	Name  string           `json:"name"`
	Gen   any              `json:"gen"`
	Part  flexInt          `json:"part"`
	Final bool             `json:"final"`
	Rows  []map[string]any `json:"rows"`
}

type pushBuf struct {
	gen  string
	next int64
	rows []Row
}

// PushAuth returns the router state when the bearer token matches.
func (h *Hub) PushAuth(id int64, token string) (*RState, error) {
	rs := h.get(id)
	if rs == nil || token == "" {
		return nil, errors.New("unauthorized")
	}
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	if !equalHash(rs.cfg.pushToken, sha256Hex(token)) {
		return nil, errors.New("unauthorized")
	}
	if rs.cfg.Mode != "push" || !rs.cfg.Enabled {
		return nil, errors.New("router không ở chế độ push hoặc đang tắt")
	}
	return rs, nil
}

func mapRows(in []map[string]any) []Row {
	out := make([]Row, 0, len(in))
	for _, m := range in {
		out = append(out, toRow(m))
	}
	return out
}

func (h *Hub) Ingest(rs *RState, m *pushMsg) (any, error) {
	now := nowMs()
	switch m.Kind {
	case "fast":
		rs.mu.Lock()
		tooSoon := now-rs.lastFast < 8000
		if !tooSoon {
			rs.lastFast = now
		}
		rs.mu.Unlock()
		if !tooSoon {
			h.observeFast(rs, toRow(m.Resource), mapRows(m.Ifaces), now)
			if m.Identity != "" {
				h.observeSection(rs, "identity", []Row{{"name": m.Identity}}, now)
			}
		}
		return h.pushReply(rs, now), nil
	case "probe":
		mons, wans := map[int64]probeOut{}, map[string]probeOut{}
		for _, p := range m.Ping {
			mons[int64(p.ID)] = probeResult(int(p.R))
		}
		for _, w := range m.Wan {
			wans[w.If] = probeResult(int(w.R))
		}
		h.observeProbes(rs, mons, wans, now)
		return map[string]bool{"ok": true}, nil
	case "section":
		return map[string]bool{"ok": true}, h.pushSection(rs, m, now)
	}
	return nil, fmt.Errorf("kind %q không hợp lệ", m.Kind)
}

func probeResult(r int) probeOut {
	if r < 0 {
		return probeOut{err: errors.New("router không chạy được lệnh ping")}
	}
	return probeOut{res: PingResult{Sent: 2, Received: r, RTT: -1}}
}

func (h *Hub) pushReply(rs *RState, now int64) map[string]any {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	ping := []map[string]any{}
	for _, m := range rs.monitors {
		if m.Pingable() && len(ping) < 30 {
			ping = append(ping, map[string]any{"id": m.ID, "ip": m.IP})
		}
	}
	wans := []string{}
	for _, w := range rs.cfg.WANs {
		if len(wans) < 8 {
			wans = append(wans, w.Iface)
		}
	}
	secs := []string{}
	if now-rs.lastPushSections >= 55000 {
		rs.lastPushSections = now
		for _, s := range sections {
			if s.Name != "identity" {
				secs = append(secs, s.Name)
			}
		}
	}
	return map[string]any{"ping": ping, "wan": wans, "target": rs.cfg.WANTarget, "sections": secs}
}

func (h *Hub) pushSection(rs *RState, m *pushMsg, now int64) error {
	if sectionByName(m.Name) == nil || m.Name == "identity" {
		return fmt.Errorf("mục %q không hợp lệ", m.Name)
	}
	gen := scalar(m.Gen)
	rows := mapRows(m.Rows)
	rs.mu.Lock()
	b := rs.push[m.Name]
	if m.Part == 0 || b == nil || b.gen != gen {
		if m.Part != 0 {
			rs.mu.Unlock()
			return errors.New("thiếu phần đầu của dữ liệu")
		}
		b = &pushBuf{gen: gen}
		rs.push[m.Name] = b
	}
	if int64(m.Part) != b.next {
		delete(rs.push, m.Name)
		rs.mu.Unlock()
		return errors.New("dữ liệu đến sai thứ tự")
	}
	b.next++
	if len(b.rows) < maxSectionRows {
		b.rows = append(b.rows, rows...)
	}
	if !m.Final {
		rs.mu.Unlock()
		return nil
	}
	all := b.rows
	delete(rs.push, m.Name)
	rs.mu.Unlock()
	h.observeSection(rs, m.Name, all, now)
	switch m.Name {
	case "lease", "arp", "bridge_host", "dns_static", "neighbor":
		h.updateClients(rs, now) // presence is time-based, so partial rounds are fine
	}
	return nil
}

// ---------- RouterOS installer script ----------

// rosQuote makes a RouterOS string literal.
func rosQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "\r", "", "\n", `\n`, "\t", `\t`, "?", `\?`)
	return `"` + r.Replace(s) + `"`
}

var pushFields = map[string][]string{}

func init() {
	for _, s := range sections {
		pushFields[s.Name] = s.Props
	}
}

var pushCommands = []struct{ name, cmd string }{
	{"address", "/ip address print as-value"},
	{"lease", "/ip dhcp-server lease print as-value"},
	{"arp", "/ip arp print as-value"},
	{"bridge_host", "/interface bridge host print as-value"},
	{"dns_static", "/ip dns static print as-value"},
	{"route", "/ip route print as-value where dst-address=0.0.0.0/0"},
	{"pppoe_client", "/interface pppoe-client print as-value"},
	{"dhcp_client", "/ip dhcp-client print as-value"},
	{"ppp_active", "/ppp active print as-value"},
	{"wan_list", "/interface list member print as-value where list=WAN"},
	{"neighbor", "/ip neighbor print as-value"},
	{"health", "/system health print as-value"},
}

func rosList(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = rosQuote(s)
	}
	return "{" + strings.Join(q, ";") + "}"
}

// collectorScript is the body of the "netpulse" script on the router.
func collectorScript(url, token string, https bool) string {
	tlsOpt := ""
	if https {
		tlsOpt = " check-certificate=no"
	}
	var b strings.Builder
	b.WriteString(`# NetPulse collector (read-only). Managed by NetPulse - do not edit.
:if ([:len [/system script job find where script="netpulse"]] > 1) do={ :error "NetPulse: previous run still active" }
:local npUrl ` + rosQuote(url) + `
:local npHdr ` + rosQuote("Content-Type: application/json,Authorization: Bearer "+token) + `
:local npPost do={
  :local body [:serialize to=json value=$data options=json.no-string-conversion]
  :local res [/tool fetch url=$url http-method=post http-header-field=$hdr http-data=$body output=user as-value duration=20s` + tlsOpt + `]
  :return [:deserialize from=json value=($res->"data")]
}
:local npSend do={
  :local chunk [:toarray ""]
  :local part 0
  :foreach row in=$rows do={
    :local clean [:toarray ""]
    :foreach k in=$fields do={
      :if ([:typeof ($row->$k)] != "nothing") do={ :set ($clean->$k) ($row->$k) }
    }
    :set ($chunk->[:len $chunk]) $clean
    :if ([:len $chunk] >= 40) do={
      :local pkt {"v"=1;"kind"="section";"name"=$name;"gen"=$gen;"part"=$part;"final"=false;"rows"=$chunk}
      $post url=$url hdr=$hdr data=$pkt
      :set part ($part + 1)
      :set chunk [:toarray ""]
    }
  }
  :local last {"v"=1;"kind"="section";"name"=$name;"gen"=$gen;"part"=$part;"final"=true;"rows"=$chunk}
  $post url=$url hdr=$hdr data=$last
}
:local res [:toarray ""]
:foreach k in=` + rosList(resourceProps) + ` do={
  :do { :set ($res->$k) [/system resource get $k] } on-error={}
}
:local ifs [:toarray ""]
:foreach r in=[/interface print stats as-value] do={
  :local x {"name"=($r->"name");"rx-byte"=($r->"rx-byte");"tx-byte"=($r->"tx-byte")}
  :do { :set ($x->"type") [/interface get ($r->".id") type] } on-error={}
  :if (!([:tostr ($x->"type")] ~ "-in\$")) do={
    :do { :set ($x->"running") [/interface get ($r->".id") running] } on-error={}
    :do { :set ($x->"disabled") [/interface get ($r->".id") disabled] } on-error={}
    :do { :set ($x->"comment") [/interface get ($r->".id") comment] } on-error={}
    :set ($ifs->[:len $ifs]) $x
  }
}
:local reply [:toarray ""]
:local fast {"v"=1;"kind"="fast";"identity"=[/system identity get name];"resource"=$res;"ifaces"=$ifs}
:onerror e in={
  :set reply [$npPost url=$npUrl hdr=$npHdr data=$fast]
} do={
  :log warning ("NetPulse: upload failed: " . $e)
  :error "NetPulse: upload failed"
}
:local pr [:toarray ""]
:foreach t in=($reply->"ping") do={
  :local got -1
  :local ip [:toip ($t->"ip")]
  :if ([:typeof $ip] = "ip") do={
    :do { :set got [/ping address=$ip count=2 interval=300ms] } on-error={ :set got -1 }
  }
  :local one {"id"=($t->"id");"r"=$got}
  :set ($pr->[:len $pr]) $one
}
:local wr [:toarray ""]
:local target [:toip ($reply->"target")]
:foreach w in=($reply->"wan") do={
  :local got -1
  :if ([:typeof $target] = "ip") do={
    :do { :set got [/ping address=$target interface=$w count=2 interval=300ms] } on-error={ :set got -1 }
  }
  :local one {"if"=$w;"r"=$got}
  :set ($wr->[:len $wr]) $one
}
:if (([:len $pr] + [:len $wr]) > 0) do={
  :local probe {"v"=1;"kind"="probe";"ping"=$pr;"wan"=$wr}
  :do { $npPost url=$npUrl hdr=$npHdr data=$probe } on-error={ :log warning "NetPulse: probe upload failed" }
}
:local gen ([/system resource get uptime] . "")
:foreach s in=($reply->"sections") do={
`)
	for _, pc := range pushCommands {
		fmt.Fprintf(&b, "  :if ($s = %s) do={\n    :do {\n      :local rows [%s]\n      :local fields %s\n      $npSend post=$npPost url=$npUrl hdr=$npHdr name=$s gen=$gen rows=$rows fields=$fields\n    } on-error={ :log warning \"NetPulse: section %s failed\" }\n  }\n",
			rosQuote(pc.name), pc.cmd, rosList(pushFields[pc.name]), pc.name)
	}
	b.WriteString("}\n")
	return b.String()
}

// installerScript wraps the collector into an idempotent .rsc for /import.
func installerScript(routerName, url, token string, https bool, interval int) string {
	src := collectorScript(url, token, https)
	return `# NetPulse installer for "` + strings.NewReplacer("\n", " ", "\r", " ").Replace(routerName) + `" - RouterOS 7.17+
# Contains a private key: do not share this file. Generating a new file revokes the old key.
# Creates only the script "netpulse" and scheduler "netpulse"; nothing else is changed.
{
:local src ` + rosQuote(src) + `
:local s [/system script find where name="netpulse"]
:if ([:len $s] = 0) do={
  /system script add name="netpulse" policy=read,test comment="NetPulse collector" source=$src
} else={
  /system script set $s policy=read,test source=$src
}
:local j [/system scheduler find where name="netpulse"]
:if ([:len $j] = 0) do={
  /system scheduler add name="netpulse" interval=` + strconv.Itoa(clampInt(interval, 20, 300)) + `s start-time=startup policy=read,test on-event="/system script run netpulse" comment="NetPulse collector"
} else={
  /system scheduler set $j interval=` + strconv.Itoa(clampInt(interval, 20, 300)) + `s policy=read,test on-event="/system script run netpulse" disabled=no
}
:put "NetPulse installed. First upload starting..."
/system script run netpulse
:put "Done. Check the NetPulse dashboard."
}
`
}

const uninstallScript = `/system scheduler remove [find where name="netpulse"]
/system script remove [find where name="netpulse"]`

// marshalReply keeps replies compact for the router's parser.
func marshalReply(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
