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

func TestRuleChecksOnDemoConfig(t *testing.T) {
	snap := collectAudit(context.Background(), newDemoSource(1))
	fs := ruleChecks(snap, perfStats{}, []string{"pppoe-vnpt"})
	titles := ""
	for _, f := range fs {
		titles += f.Severity + ": " + f.Title + "\n"
	}
	for _, want := range []string{
		"critical: Firewall không chặn truy cập vào router từ Internet",
		"high: Dịch vụ Telnet đang bật",
		"high: DNS của router trả lời cả truy vấn từ Internet",
		"high: Mở cổng Remote Desktop (3389) từ Internet",
		"lần đăng nhập sai trong log gần đây",
		"medium: Tài khoản mặc định \"admin\"",
		"medium: UPnP đang bật",
		"medium: MAC-WinBox cho phép trên mọi interface",
		"medium: RouterOS chưa cập nhật bản mới nhất (7.18.2 → 7.20.4)",
	} {
		if !strings.Contains(titles, want) {
			t.Errorf("missing finding %q in:\n%s", want, titles)
		}
	}
	// the only fasttrack rule in the demo is RouterOS's dynamic dummy counter rule,
	// so the check must still report that FastTrack is not enabled
	if !strings.Contains(titles, "low: Chưa bật FastTrack") {
		t.Errorf("expected FastTrack finding in:\n%s", titles)
	}
	if fs[0].Severity != "critical" {
		t.Fatalf("findings must be sorted by severity: %+v", fs[0])
	}
	sec, _ := scores(fs)
	if sec >= 50 {
		t.Fatalf("insecure demo must score low, got %d", sec)
	}
}

func TestRedactAndAnonymize(t *testing.T) {
	r := redactRow(Row{"name": "backup", "password": "x", "source": `/tool fetch url="ftp://a" user=bk password=Secret123 dst-path=b.rsc`}, 600)
	if _, ok := r["password"]; ok || strings.Contains(r["source"], "Secret123") || !strings.Contains(r["source"], "password=***") {
		t.Fatalf("redaction failed: %v", r)
	}
	a := newAnonymizer()
	in := "login failure from 45.141.80.17 and 45.141.80.17 to 192.168.88.1 mac DC:A6:32:00:10:00 dns 8.8.8.8 cgnat 100.64.1.2"
	m := a.Mask(in)
	if strings.Contains(m, "45.141.80.17") || strings.Contains(m, "DC:A6:32") || !strings.Contains(m, "PUBLIC-IP-1 and PUBLIC-IP-1") ||
		!strings.Contains(m, "192.168.88.1") || !strings.Contains(m, "8.8.8.8") || !strings.Contains(m, "100.64.1.2") || !strings.Contains(m, "MAC-1") {
		t.Fatalf("mask = %q", m)
	}
	if a.Unmask(m) != in {
		t.Fatalf("unmask = %q", a.Unmask(m))
	}
}

// fakeClaude imitates the Messages API: first asks for /ip/service, then submits.
type fakeClaude struct {
	mu     sync.Mutex
	bodies []string
	betas  []string
	turn   int
}

func (f *fakeClaude) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(b))
	f.betas = append(f.betas, r.Header.Get("anthropic-beta"))
	f.turn++
	turn := f.turn
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	var content string
	stop := "tool_use"
	switch turn {
	case 1:
		content = `[{"type":"text","text":"Tôi sẽ xem dịch vụ và log."},
			{"type":"tool_use","id":"tu_1","name":"get_router_data","input":{"path":"/ip/service"}},
			{"type":"tool_use","id":"tu_2","name":"get_router_data","input":{"path":"/log"}},
			{"type":"tool_use","id":"tu_3","name":"get_history","input":{"kind":"events","hours":24,"interface":""}}]`
	default:
		// find the placeholder the agent was given for the attacker IP and echo it back
		ph := "PUBLIC-IP-1"
		content = `[{"type":"tool_use","id":"tu_9","name":"submit_report","input":{"security_score":35,"performance_score":88,
			"summary":"Router bị dò mật khẩu từ ` + ph + `.",
			"findings":[{"severity":"low","area":"performance","title":"Ít quan trọng","evidence":"x","fix":"y","commands":[]},
			            {"severity":"critical","area":"security","title":"Chặn WinBox từ ` + ph + `","evidence":"24 lần","fix":"chặn","commands":["/ip firewall address-list add list=chan address=` + ph + `"]}],
			"report_markdown":"## Tổng quan\nNguồn tấn công ` + ph + `.\n"}}]`
	}
	io.WriteString(w, `{"id":"msg_`+string(rune('0'+turn))+`","type":"message","role":"assistant","model":"claude-opus-5-5","content":`+content+
		`,"stop_reason":"`+stop+`","stop_sequence":null,"usage":{"input_tokens":1000,"output_tokens":200,"cache_read_input_tokens":500,"cache_creation_input_tokens":0}}`)
}

func TestAgentEndToEnd(t *testing.T) {
	dir := t.TempDir()
	st, _ := OpenStore(filepath.Join(dir, "db"))
	defer st.Close()
	box, _ := LoadBox(filepath.Join(dir, "key"))
	settings, _ := NewSettingsStore(st, box)
	key := "sk-ant-test-0123456789abcdefghij"
	if err := settings.Update(SettingsInput{AIKey: &key, AIModel: "claude-opus-5-5", AIConsent: true, AIAnonymize: true,
		SummaryAt: "08:00", RepeatMin: 30, Timezone: "Asia/Ho_Chi_Minh", RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	h := NewHub(st, box, settings, true)
	r := &Router{Name: "Demo", Mode: "demo", Enabled: true, Interval: 300, WANTarget: "8.8.8.8"}
	st.SaveRouter(r)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); h.Wait() }()
	if err := h.Start(ctx); err != nil {
		t.Fatal(err)
	}
	fc := &fakeClaude{}
	srv := httptest.NewServer(fc)
	defer srv.Close()
	a := NewAuditor(h, st, settings, nil)
	a.ctx, a.apiBase = ctx, srv.URL

	id, err := a.Start(r.ID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Start(r.ID, "manual"); err == nil {
		t.Fatal("second concurrent audit must be refused")
	}
	waitFor(t, "audit finished", func() bool { return a.Running(r.ID) == nil })
	rep, err := st.GetAudit(id)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != "done" || rep.Source != "ai" || rep.Error != "" {
		t.Fatalf("report status=%s source=%s err=%s note=%s", rep.Status, rep.Source, rep.Error, rep.Note)
	}
	if rep.SecScore != 35 || rep.Findings[0].Severity != "critical" || !strings.Contains(rep.Findings[0].Title, "45.141.") {
		t.Fatalf("findings not sorted/unmasked: %+v", rep.Findings)
	}
	if !strings.Contains(rep.Markdown, "45.141.") || !strings.Contains(rep.Summary, "45.141.") {
		t.Fatalf("report not unmasked: %q / %q", rep.Markdown, rep.Summary)
	}
	if len(rep.Steps) < 3 || rep.Usage == nil || rep.Usage.Turns != 2 || rep.Usage.CostUSD <= 0 || len(rep.RuleFindings) == 0 {
		t.Fatalf("steps=%v usage=%+v rules=%d", rep.Steps, rep.Usage, len(rep.RuleFindings))
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.bodies) != 2 {
		t.Fatalf("calls = %d", len(fc.bodies))
	}
	for i, body := range fc.bodies {
		if strings.Contains(body, "45.141.") || strings.Contains(body, "DC:A6:32") {
			t.Fatalf("request %d leaked a public IP or MAC", i)
		}
		if !strings.Contains(fc.betas[i], "server-side-fallback-2026-07-01") {
			t.Fatalf("missing fallback beta header: %q", fc.betas[i])
		}
	}
	var req map[string]any
	json.Unmarshal([]byte(fc.bodies[1]), &req)
	if req["model"] != "claude-opus-5-5" || req["fallbacks"] != "default" {
		t.Fatalf("model/fallbacks = %v / %v", req["model"], req["fallbacks"])
	}
	if oc, _ := req["output_config"].(map[string]any); oc["effort"] != "high" {
		t.Fatalf("effort = %v", req["output_config"])
	}
	tools, _ := req["tools"].([]any)
	if len(tools) != 4 || tools[0].(map[string]any)["strict"] != true {
		t.Fatalf("tools = %v", tools)
	}
	if !strings.Contains(fc.bodies[1], `"tool_use_id":"tu_2"`) || !strings.Contains(fc.bodies[1], "PUBLIC-IP-1") {
		t.Fatal("second request must carry the masked tool results")
	}
	_ = time.Second
}

func TestAuditWithoutAI(t *testing.T) {
	dir := t.TempDir()
	st, _ := OpenStore(filepath.Join(dir, "db"))
	defer st.Close()
	box, _ := LoadBox(filepath.Join(dir, "key"))
	settings, _ := NewSettingsStore(st, box)
	h := NewHub(st, box, settings, true)
	r := &Router{Name: "Demo", Mode: "demo", Enabled: true, Interval: 300, WANTarget: "8.8.8.8"}
	st.SaveRouter(r)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); h.Wait() }()
	h.Start(ctx)
	a := NewAuditor(h, st, settings, nil)
	a.ctx = ctx
	id, err := a.Start(r.ID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "audit finished", func() bool { return a.Running(r.ID) == nil })
	rep, _ := st.GetAudit(id)
	if rep.Status != "done" || rep.Source != "rules" || !strings.Contains(rep.Note, "Chưa cấu hình trợ lý AI") || !strings.Contains(rep.Markdown, "## Bảo mật") {
		t.Fatalf("rules-only report: %+v", rep)
	}
	list, _ := st.ListAudits(r.ID, 10)
	if len(list) != 1 || list[0].Markdown != "" {
		t.Fatalf("list = %+v", list)
	}
}
