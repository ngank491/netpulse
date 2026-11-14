package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- audit reports ----------

type AIUsage struct {
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CacheRead    int64   `json:"cacheRead"`
	CacheWrite   int64   `json:"cacheWrite"`
	Turns        int     `json:"turns"`
	CostUSD      float64 `json:"costUsd"`
	CostKnown    bool    `json:"costKnown"`
}

type AuditReport struct {
	ID           int64    `json:"id"`
	RouterID     int64    `json:"routerId"`
	StartedAt    int64    `json:"startedAt"`
	FinishedAt   int64    `json:"finishedAt"`
	Status       string   `json:"status"` // running | done | error
	Source       string   `json:"source"` // ai | rules
	Model        string   `json:"model,omitempty"`
	Provider     string   `json:"provider,omitempty"` // name of the AI account that wrote the report
	Attempts     []string `json:"attempts,omitempty"` // AI accounts that failed before
	providerID   string
	Trigger      string    `json:"trigger"` // manual | weekly
	SecScore     int       `json:"secScore"`
	PerfScore    int       `json:"perfScore"`
	Summary      string    `json:"summary"`
	Findings     []Finding `json:"findings"`
	RuleFindings []Finding `json:"ruleFindings"`
	Markdown     string    `json:"markdown"`
	Steps        []string  `json:"steps"`
	Usage        *AIUsage  `json:"usage,omitempty"`
	Live         bool      `json:"live"`
	Note         string    `json:"note,omitempty"`
	Error        string    `json:"error,omitempty"`
	Progress     string    `json:"progress,omitempty"`
}

// Auditor runs security/performance audits, one at a time per router.
type Auditor struct {
	h        *Hub
	st       *Store
	settings *SettingsStore
	n        *Notifier
	ctx      context.Context

	mu      sync.Mutex
	running map[int64]*AuditReport // by router
	sem     chan struct{}
	apiBase string // tests
}

func NewAuditor(h *Hub, st *Store, settings *SettingsStore, n *Notifier) *Auditor {
	return &Auditor{h: h, st: st, settings: settings, n: n, running: map[int64]*AuditReport{}, sem: make(chan struct{}, 2)}
}

// Running returns the in-progress report of a router, if any.
func (a *Auditor) Running(routerID int64) *AuditReport {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r := a.running[routerID]; r != nil {
		cp := *r
		return &cp
	}
	return nil
}

func (a *Auditor) setProgress(r *AuditReport, msg string) {
	a.mu.Lock()
	r.Progress = msg
	a.mu.Unlock()
}

// Start launches an audit in the background and returns its id.
func (a *Auditor) Start(routerID int64, trigger, providerID string) (int64, error) {
	if a.ctx == nil {
		return 0, errors.New("auditor chưa khởi động")
	}
	rs := a.h.get(routerID)
	if rs == nil {
		return 0, ErrNotFound
	}
	busy := errors.New("đang có một lần kiểm tra chạy cho router này")
	if a.Running(routerID) != nil {
		return 0, busy
	}
	rep := &AuditReport{RouterID: routerID, StartedAt: nowMs(), Status: "running", Trigger: trigger, Progress: "Đang chờ lượt…", providerID: providerID}
	id, err := a.st.SaveAudit(rep) // sets rep.ID before anyone else can see rep
	if err != nil {
		return 0, err
	}
	a.mu.Lock()
	if a.running[routerID] != nil {
		a.mu.Unlock()
		a.st.DeleteAudit(id)
		return 0, busy
	}
	a.running[routerID] = rep
	a.mu.Unlock()
	go a.run(rs, rep)
	return id, nil
}

// set mutates the shared report under the lock (Running() copies it concurrently).
func (a *Auditor) set(rep *AuditReport, f func(r *AuditReport)) {
	a.mu.Lock()
	f(rep)
	a.mu.Unlock()
}

func (a *Auditor) run(rs *RState, rep *AuditReport) {
	defer func() {
		if v := recover(); v != nil {
			log.Printf("audit panic: %v", v)
			a.set(rep, func(r *AuditReport) { r.Status, r.Error = "error", fmt.Sprint(v) })
		}
		a.mu.Lock()
		rep.FinishedAt, rep.Progress = nowMs(), ""
		if rep.Status == "running" {
			rep.Status = "done"
		}
		final := *rep
		a.mu.Unlock()
		if _, err := a.st.SaveAudit(&final); err != nil {
			log.Printf("lưu báo cáo kiểm tra: %v", err)
		}
		a.mu.Lock()
		delete(a.running, rep.RouterID)
		a.mu.Unlock()
	}()
	fail := func(msg string) { a.set(rep, func(r *AuditReport) { r.Status, r.Error = "error", msg }) }
	select {
	case a.sem <- struct{}{}:
		defer func() { <-a.sem }()
	case <-a.ctx.Done():
		fail("NetPulse đang dừng")
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 15*time.Minute)
	defer cancel()

	rs.mu.RLock()
	cfg := rs.cfg
	rs.mu.RUnlock()

	// 1. read-only snapshot
	a.setProgress(rep, "Đang đọc cấu hình router (chỉ đọc)…")
	var snap *auditSnapshot
	note := ""
	if src, err := a.h.source(&cfg); err == nil && (cfg.Mode == "pull" || cfg.Mode == "demo") {
		snap = collectAudit(ctx, src)
		ok, firstErr := 0, ""
		for _, sec := range snap.Sections {
			if sec.Error == "" && !sec.Unsupported {
				ok++
			} else if firstErr == "" && sec.Error != "" {
				firstErr = sec.Error
			}
		}
		if ok == 0 {
			fail("Không đọc được router: " + firstErr)
			return
		}
	} else {
		snap = snapshotFromState(rs)
		note = "Router ở chế độ push: NetPulse không đọc được cấu hình (firewall, dịch vụ, tài khoản…), chỉ phân tích dữ liệu giám sát. Chuyển sang chế độ REST API để kiểm tra bảo mật đầy đủ."
	}

	// 2. built-in checks
	a.setProgress(rep, "Đang chạy bộ kiểm tra có sẵn…")
	perf, err := a.h.perfStats(cfg.ID, 24, "")
	if err != nil {
		log.Printf("thống kê hiệu suất: %v", err)
	}
	var wans []string
	for _, w := range cfg.WANs {
		wans = append(wans, w.Iface)
	}
	rs.mu.RLock()
	wans = append(wans, detectWANs(rs.Sections)...)
	rs.mu.RUnlock()
	rules := ruleChecks(snap, perf, wans)
	a.set(rep, func(r *AuditReport) {
		r.Live, r.Note = snap.Live, note
		r.RuleFindings, r.Findings, r.Source = rules, rules, "rules"
		r.SecScore, r.PerfScore = scores(rules)
		r.Markdown, r.Summary = rulesMarkdown(r), rulesSummary(r)
	})

	// 3. AI agent (optional): the chosen account first, then others if fallback is on
	s := a.settings.Get()
	order := a.settings.ProviderOrder(rep.providerID)
	switch {
	case len(order) == 0:
		a.set(rep, func(r *AuditReport) {
			r.Note = strings.TrimSpace(r.Note + " Chưa có tài khoản AI: báo cáo chỉ gồm bộ kiểm tra có sẵn.")
		})
		return
	case !s.AIConsent:
		a.set(rep, func(r *AuditReport) {
			r.Note = strings.TrimSpace(r.Note + " Trợ lý AI chưa được phép gửi dữ liệu (Cài đặt → Trợ lý AI).")
		})
		return
	}
	a.mu.Lock()
	base := *rep // the agent reads rule results/scores from a private copy
	a.mu.Unlock()
	var attempts []string
	for _, p := range order {
		if ctx.Err() != nil {
			break
		}
		a.setProgress(rep, p.Name+" đang phân tích…")
		var anon *anonymizer
		if s.AIAnonymize {
			anon = newAnonymizer()
		}
		res, err := a.runAgent(ctx, p, a.settings.ProviderKey(p.ID), rs, cfg, snap, perf, &base, rep, anon)
		if err != nil {
			attempts = append(attempts, p.Name+": "+err.Error())
			a.set(rep, func(r *AuditReport) { r.Attempts = attempts })
			continue
		}
		a.set(rep, func(r *AuditReport) {
			u := res.Usage
			r.Usage, r.Attempts = &u, attempts
			r.Source, r.Model, r.Provider = "ai", res.Model, p.Name
			r.SecScore, r.PerfScore = clampInt(res.SecScore, 0, 100), clampInt(res.PerfScore, 0, 100)
			r.Summary, r.Markdown, r.Findings = res.Summary, res.Markdown, res.Findings
			if r.Findings == nil {
				r.Findings = []Finding{}
			}
		})
		return
	}
	a.set(rep, func(r *AuditReport) {
		r.Attempts = attempts
		r.Error = "Trợ lý AI lỗi (" + strings.Join(attempts, "; ") + ") — đang hiển thị kết quả của bộ kiểm tra có sẵn."
	})
}

// ---------- the agent (any provider) ----------

type aiResult struct {
	Model     string
	SecScore  int
	PerfScore int
	Summary   string
	Markdown  string
	Findings  []Finding
	Usage     AIUsage
}

// Known Claude prices: input, output, cache read — USD per million tokens.
var aiModels = map[string][3]float64{
	"claude-opus-5-5":   {4, 20, 0.20},
	"claude-sonnet-5-5": {2, 10, 0.10},
	"claude-haiku-5-5":  {0.10, 0.50, 0.01},
}

const agentSystem = `You are the network audit agent built into NetPulse, a monitoring tool for MikroTik RouterOS routers. You assess ONE router for security and performance and write a report for the technician who runs it. Write everything the reader sees in Vietnamese.

How you work:
- The router configuration was read once, read-only, by NetPulse. You cannot change anything. Use get_router_data to read the sections you need (the first message lists what exists), get_history for CPU/RAM/traffic/events/devices over time, and get_clients for the client population.
- NetPulse's built-in rule checks have already produced findings. Verify each against the data, drop false positives, adjust severities with reasons, and look for what simple rules miss: firewall rule order and logic (an accept above a drop, rules that never match, missing invalid/established handling), services reachable through NAT, unknown users or groups, schedulers/scripts that fetch or import code, brute-force patterns in the log, outdated RouterOS, interfaces with errors or flapping links, sustained CPU/RAM pressure, queue/fasttrack interactions.
- Ground every finding in data you actually read and quote the evidence (rule numbers or comments, values, counts). If data is missing or a section is unsupported, say so instead of guessing.
- Values such as PUBLIC-IP-3 or MAC-7 are anonymised placeholders. Keep them exactly as written.

Remediation:
- Give exact RouterOS 7 terminal commands, in a safe order. Before any change that can cut off management access (firewall input, services, users, interface lists), tell the reader to enable Safe Mode (Ctrl+X in the terminal) and to add an accept rule for the admin network first.
- Never output passwords or secrets; use placeholders like <MatKhauManh>.
- Prefer few, high-impact actions over long generic checklists.

Finish by calling submit_report exactly once:
- security_score and performance_score from 0 (very bad) to 100 (no issues), consistent with the findings.
- summary: 2-4 sentences for a manager: overall state and the most important action.
- findings: ordered from most to least severe; severity is one of critical, high, medium, low, info; area is one of security, performance, reliability. Each has a short title, the evidence, a fix explanation and the commands (may be empty).
- report_markdown: the full report with these sections: "## Tổng quan", "## Bảo mật", "## Hiệu suất và độ ổn định", "## Việc cần làm theo thứ tự ưu tiên". Put commands in fenced code blocks. Keep it readable for a technician; no filler.`

func objSchema(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

// agentTools are read-only; submit_report ends the run.
func agentTools(paths []string) []llmTool {
	finding := objSchema(map[string]any{
		"severity": map[string]any{"type": "string", "enum": []string{"critical", "high", "medium", "low", "info"}},
		"area":     map[string]any{"type": "string", "enum": []string{"security", "performance", "reliability"}},
		"title":    map[string]any{"type": "string"},
		"evidence": map[string]any{"type": "string"},
		"fix":      map[string]any{"type": "string"},
		"commands": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	}, "severity", "area", "title", "evidence", "fix", "commands")
	return []llmTool{
		{"get_router_data", "Read one section of the router configuration/state that NetPulse collected read-only (e.g. /ip/firewall/filter, /ip/service, /user, /log). Returns the rows as JSON; secrets are already removed.",
			objSchema(map[string]any{"path": map[string]any{"type": "string", "enum": paths, "description": "Section path from the list in the first message"}}, "path")},
		{"get_history", "Statistics NetPulse recorded over time for this router. kind=cpu_mem_interfaces: CPU/RAM and per-interface traffic (bits/s) avg/p95/max; kind=events: the warning/info event log; kind=devices: ping-monitored devices with status and availability. hours: 1-720. interface: optional interface name for cpu_mem_interfaces (empty string = busiest interfaces).",
			objSchema(map[string]any{
				"kind":      map[string]any{"type": "string", "enum": []string{"cpu_mem_interfaces", "events", "devices"}},
				"hours":     map[string]any{"type": "integer"},
				"interface": map[string]any{"type": "string"},
			}, "kind", "hours", "interface")},
		{"get_clients", "Summary of client devices seen on the router: counts online/offline, unnamed and randomised-MAC devices, vendors, and devices first seen in the last 7 days.",
			objSchema(map[string]any{})},
		{"submit_report", "Submit the final audit report. Call exactly once, at the end.",
			objSchema(map[string]any{
				"security_score":    map[string]any{"type": "integer"},
				"performance_score": map[string]any{"type": "integer"},
				"summary":           map[string]any{"type": "string"},
				"findings":          map[string]any{"type": "array", "items": finding},
				"report_markdown":   map[string]any{"type": "string"},
			}, "security_score", "performance_score", "summary", "findings", "report_markdown")},
	}
}

func estimateCost(p AIProvider, model string, u *AIUsage) {
	price := [3]float64{p.PriceIn, p.PriceOut, p.PriceIn}
	if p.Kind == "anthropic" {
		if known, ok := aiModels[model]; ok && p.PriceIn == 0 {
			price = known
		}
	}
	if price[0] == 0 && price[1] == 0 {
		u.CostUSD, u.CostKnown = 0, false
		return
	}
	cacheRead := price[2]
	if p.Kind == "openai" {
		cacheRead = price[0] / 2
	}
	u.CostUSD = (float64(u.InputTokens)*price[0] + float64(u.CacheWrite)*price[0]*1.25 + float64(u.OutputTokens)*price[1] + float64(u.CacheRead)*cacheRead) / 1e6
	u.CostKnown = true
}

func (a *Auditor) runAgent(ctx context.Context, p AIProvider, key string, rs *RState, cfg Router, snap *auditSnapshot, perf perfStats, rep, live *AuditReport, anon *anonymizer) (*aiResult, error) {
	var paths []string
	var index strings.Builder
	for _, ap := range auditPaths {
		sec := snap.Sections[ap.Path]
		if sec == nil {
			continue
		}
		switch {
		case sec.Unsupported:
			fmt.Fprintf(&index, "- %s (%s): không có trên router này\n", ap.Path, ap.Title)
		case sec.Error != "":
			fmt.Fprintf(&index, "- %s (%s): không đọc được (%s)\n", ap.Path, ap.Title, sec.Error)
		default:
			paths = append(paths, ap.Path)
			fmt.Fprintf(&index, "- %s (%s): %d dòng\n", ap.Path, ap.Title, sec.Total)
		}
	}
	for path, sec := range snap.Sections { // push-mode sections not in auditPaths
		if !strings.Contains(index.String(), path+" ") {
			paths = append(paths, path)
			fmt.Fprintf(&index, "- %s (%s): %d dòng\n", path, sec.Title, sec.Total)
		}
	}
	sort.Strings(paths)

	rs.mu.RLock()
	identity, res := rs.Identity, rs.Resource
	rs.mu.RUnlock()
	ruleJSON, _ := json.MarshalIndent(rep.RuleFindings, "", " ")
	perfJSON, _ := json.Marshal(perf)
	var wans []string
	for _, w := range cfg.WANs {
		wans = append(wans, w.Iface+" ("+firstNonEmpty(w.Label, "không nhãn")+")")
	}
	first := fmt.Sprintf(`Router: %q, identity %q, model %s, RouterOS %s, uptime %s. Kết nối: %s.
Cấu hình đọc lúc %s (live=%v).
WAN đang được NetPulse theo dõi: %s.

Các mục dữ liệu có thể đọc bằng get_router_data:
%s
Kết quả bộ kiểm tra có sẵn (điểm bảo mật %d, hiệu suất %d):
%s

Thống kê 24 giờ qua: %s

Hãy kiểm tra router này và gọi submit_report khi xong.`,
		cfg.Name, identity, res["board-name"], res["version"], res["uptime"], cfg.Mode,
		time.UnixMilli(snap.At).In(a.settings.Loc()).Format("15:04 02/01/2006"), snap.Live,
		firstNonEmpty(strings.Join(wans, ", "), "chưa cấu hình"),
		index.String(), rep.SecScore, rep.PerfScore, ruleJSON, perfJSON)

	sess, err := newSession(p, key, agentSystem, anon.Mask(first), agentTools(paths), a.apiBase)
	if err != nil {
		return nil, err
	}
	out := &aiResult{Model: p.Model}
	var results []llmResult
	nudge := ""
	for turn := 0; turn < 18; turn++ {
		t, err := sess.Next(ctx, results, nudge)
		if err != nil {
			return out, err
		}
		out.Usage.InputTokens += t.In
		out.Usage.OutputTokens += t.Out
		out.Usage.CacheRead += t.CacheRead
		out.Usage.CacheWrite += t.CacheWrite
		out.Usage.Turns++
		if t.Model != "" {
			out.Model = t.Model
		}
		estimateCost(p, out.Model, &out.Usage)
		if t.Stop == "refusal" {
			return out, errors.New("mô hình từ chối yêu cầu")
		}
		results, nudge = nil, ""
		submitted := false
		for _, c := range t.Calls {
			if t.Stop == "length" {
				results = append(results, llmResult{c.ID, "INVALID_JSON: output was cut off, call the tool again with shorter content", true})
				continue
			}
			text, isErr, done := a.execTool(c.Name, c.Args, rs, snap, cfg.ID, live, anon, out)
			submitted = submitted || done
			results = append(results, llmResult{c.ID, anon.Mask(text), isErr})
		}
		if submitted {
			return out, nil
		}
		if len(t.Calls) == 0 {
			if strings.TrimSpace(t.Text) != "" { // ended with prose instead of the tool: keep it
				out.Markdown = anon.Unmask(t.Text)
				out.SecScore, out.PerfScore = rep.SecScore, rep.PerfScore
				out.Summary, out.Findings = rep.Summary, rep.RuleFindings
				return out, nil
			}
			return out, fmt.Errorf("mô hình dừng mà chưa nộp báo cáo (%s)", t.Stop)
		}
		if turn >= 14 {
			nudge = "Bạn đã có đủ dữ liệu. Hãy gọi submit_report ngay bây giờ."
		}
	}
	return out, errors.New("quá số bước cho phép mà chưa có báo cáo")
}

// execTool runs one read-only tool. done=true when the final report was accepted.
func (a *Auditor) execTool(name, raw string, rs *RState, snap *auditSnapshot, routerID int64, rep *AuditReport, anon *anonymizer, out *aiResult) (string, bool, bool) {
	addStep := func(s string) {
		a.mu.Lock()
		rep.Steps = append(rep.Steps, s)
		rep.Progress = "Trợ lý AI: " + s
		a.mu.Unlock()
	}
	jsonOut := func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	switch name {
	case "get_router_data":
		var in struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(raw), &in) != nil {
			return "INVALID_JSON", true, false
		}
		sec := snap.Sections[in.Path]
		if sec == nil {
			return "Không có mục " + in.Path, true, false
		}
		addStep("đọc " + sec.Title + " (" + in.Path + ")")
		rows := sec.Rows
		for { // keep each tool result reasonably small
			b := jsonOut(map[string]any{"path": in.Path, "title": sec.Title, "total": sec.Total, "returned": len(rows), "rows": rows, "unsupported": sec.Unsupported, "error": sec.Error})
			if len(b) < 60_000 || len(rows) < 10 {
				return b, false, false
			}
			rows = rows[:len(rows)*2/3]
		}
	case "get_history":
		var in struct {
			Kind      string `json:"kind"`
			Hours     int    `json:"hours"`
			Interface string `json:"interface"`
		}
		if json.Unmarshal([]byte(raw), &in) != nil {
			return "INVALID_JSON", true, false
		}
		in.Hours = clampInt(in.Hours, 1, 720)
		switch in.Kind {
		case "cpu_mem_interfaces":
			addStep(fmt.Sprintf("xem CPU/RAM/lưu lượng %d giờ", in.Hours))
			p, err := a.h.perfStats(routerID, in.Hours, in.Interface)
			if err != nil {
				return err.Error(), true, false
			}
			p.Devices = nil
			return jsonOut(p), false, false
		case "events":
			addStep(fmt.Sprintf("xem sự kiện %d giờ", in.Hours))
			evs, err := a.st.ListEvents([]int64{routerID}, 400)
			if err != nil {
				return err.Error(), true, false
			}
			since := nowMs() - int64(in.Hours)*3600_000
			var list []map[string]string
			for _, e := range evs {
				if e.At >= since {
					list = append(list, map[string]string{"time": time.UnixMilli(e.At).In(a.settings.Loc()).Format("2006-01-02 15:04"), "level": e.Level, "message": e.Msg})
				}
			}
			return jsonOut(map[string]any{"hours": in.Hours, "events": list}), false, false
		case "devices":
			addStep("xem thiết bị theo dõi")
			v, ok := a.h.View(routerID)
			if !ok {
				return "router không còn", true, false
			}
			var list []map[string]any
			for _, m := range v.Monitors {
				item := map[string]any{"name": m.Name, "ip": m.IP, "kind": m.Kind, "enabled": m.Enabled, "availability24h": m.Avail24}
				if m.State != nil {
					item["status"], item["since"] = m.State.Status, time.UnixMilli(m.State.Since).In(a.settings.Loc()).Format("2006-01-02 15:04")
				}
				list = append(list, item)
			}
			return jsonOut(map[string]any{"devices": list, "wans": v.WANs}), false, false
		}
		return "kind không hợp lệ", true, false
	case "get_clients":
		addStep("xem danh sách client")
		rs.mu.RLock()
		list := rs.clientList()
		rs.mu.RUnlock()
		vendors := map[string]int{}
		online, unnamed, random := 0, 0, 0
		var recent []map[string]any
		for _, c := range list {
			if c.Online {
				online++
			}
			if c.Name == "" {
				unnamed++
			}
			if c.RandomMAC {
				random++
			}
			vendors[firstNonEmpty(c.Vendor, "(không rõ)")]++
			if nowMs()-c.FirstSeen < 7*86400_000 && len(recent) < 40 {
				recent = append(recent, map[string]any{"name": c.Name, "vendor": c.Vendor, "ip": c.IP, "mac": c.MAC, "online": c.Online,
					"firstSeen": time.UnixMilli(c.FirstSeen).In(a.settings.Loc()).Format("2006-01-02 15:04")})
			}
		}
		return jsonOut(map[string]any{"total": len(list), "online": online, "unnamed": unnamed, "randomMac": random,
			"vendors": topCounts(vendors, 12), "firstSeenLast7Days": recent}), false, false
	case "submit_report":
		var in struct {
			Sec      int       `json:"security_score"`
			Perf     int       `json:"performance_score"`
			Summary  string    `json:"summary"`
			Findings []Finding `json:"findings"`
			Markdown string    `json:"report_markdown"`
		}
		if json.Unmarshal([]byte(raw), &in) != nil {
			return "INVALID_JSON: gửi lại submit_report với JSON hợp lệ", true, false
		}
		if strings.TrimSpace(in.Markdown) == "" || strings.TrimSpace(in.Summary) == "" {
			return "report_markdown và summary không được để trống", true, false
		}
		for i := range in.Findings {
			f := &in.Findings[i]
			if _, ok := severityRank[f.Severity]; !ok {
				f.Severity = "info"
			}
			f.Title, f.Evidence, f.Fix = anon.Unmask(f.Title), anon.Unmask(f.Evidence), anon.Unmask(f.Fix)
			for j := range f.Commands {
				f.Commands[j] = anon.Unmask(f.Commands[j])
			}
			if f.Commands == nil {
				f.Commands = []string{}
			}
		}
		sort.SliceStable(in.Findings, func(i, j int) bool {
			return severityRank[in.Findings[i].Severity] < severityRank[in.Findings[j].Severity]
		})
		addStep("viết báo cáo")
		out.SecScore, out.PerfScore = in.Sec, in.Perf
		out.Summary, out.Markdown, out.Findings = anon.Unmask(in.Summary), anon.Unmask(in.Markdown), in.Findings
		return "Đã nhận báo cáo.", false, true
	}
	return "công cụ không tồn tại: " + name, true, false
}

// ---------- reports without AI ----------

var severityVi = map[string]string{"critical": "Nghiêm trọng", "high": "Cao", "medium": "Trung bình", "low": "Thấp", "info": "Thông tin"}

func rulesSummary(rep *AuditReport) string {
	counts := map[string]int{}
	for _, f := range rep.RuleFindings {
		counts[f.Severity]++
	}
	if len(rep.RuleFindings) == 0 {
		return "Bộ kiểm tra có sẵn không phát hiện vấn đề nào."
	}
	var parts []string
	for _, s := range []string{"critical", "high", "medium", "low"} {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d mức %s", counts[s], strings.ToLower(severityVi[s])))
		}
	}
	msg := fmt.Sprintf("Bộ kiểm tra có sẵn phát hiện %d vấn đề (%s).", len(rep.RuleFindings), strings.Join(parts, ", "))
	return msg + " Việc nên làm trước: " + rep.RuleFindings[0].Title + "."
}

func rulesMarkdown(rep *AuditReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Tổng quan\n\nĐiểm bảo mật **%d/100**, hiệu suất và độ ổn định **%d/100** (tính theo bộ kiểm tra có sẵn).\n\n%s\n\n", rep.SecScore, rep.PerfScore, rulesSummary(rep))
	groups := []struct{ title, area string }{{"## Bảo mật", "security"}, {"## Hiệu suất và độ ổn định", "performance"}}
	for _, g := range groups {
		b.WriteString(g.title + "\n\n")
		n := 0
		for _, f := range rep.RuleFindings {
			if (g.area == "security") != (f.Area == "security") {
				continue
			}
			n++
			fmt.Fprintf(&b, "### %s – %s\n\n", severityVi[f.Severity], f.Title)
			if f.Evidence != "" {
				fmt.Fprintf(&b, "- Dữ liệu: %s\n", f.Evidence)
			}
			fmt.Fprintf(&b, "- Cách xử lý: %s\n\n", f.Fix)
			if len(f.Commands) > 0 {
				b.WriteString("```\n" + strings.Join(f.Commands, "\n") + "\n```\n\n")
			}
		}
		if n == 0 {
			b.WriteString("Không phát hiện vấn đề.\n\n")
		}
	}
	return b.String()
}

// ---------- weekly schedule ----------

func (a *Auditor) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !a.settings.Get().AutoAudit {
			continue
		}
		local := time.Now().In(a.settings.Loc())
		if local.Weekday() != time.Sunday || local.Hour() < 7 {
			continue
		}
		day := local.Format("2006-01-02")
		if last, _ := a.st.Get("audit_weekly_last"); last == day {
			continue
		}
		a.st.Set("audit_weekly_last", day)
		for _, rs := range a.h.all() {
			rs.mu.RLock()
			enabled, id := rs.cfg.Enabled, rs.id
			rs.mu.RUnlock()
			if !enabled {
				continue
			}
			aid, err := a.Start(id, "weekly", "")
			if err != nil {
				continue
			}
			a.waitAndNotify(ctx, rs, aid)
		}
	}
}

func (a *Auditor) waitAndNotify(ctx context.Context, rs *RState, aid int64) {
	for a.Running(rs.id) != nil {
		if sleepCtx(ctx, 5*time.Second); ctx.Err() != nil {
			return
		}
	}
	rep, err := a.st.GetAudit(aid)
	if err != nil || rep.Status != "done" || a.n == nil {
		return
	}
	rs.mu.RLock()
	cfg := rs.cfg
	rs.mu.RUnlock()
	var b strings.Builder
	fmt.Fprintf(&b, "🛡 Kiểm tra định kỳ [%s]\nBảo mật %d/100 · Hiệu suất %d/100\n%s", cfg.Name, rep.SecScore, rep.PerfScore, rep.Summary)
	n := 0
	for _, f := range rep.Findings {
		if f.Severity == "critical" || f.Severity == "high" || f.Severity == "medium" {
			if n == 5 {
				break
			}
			fmt.Fprintf(&b, "\n• (%s) %s", severityVi[f.Severity], f.Title)
			n++
		}
	}
	b.WriteString("\nXem chi tiết và lệnh khắc phục trong NetPulse → router → Kiểm tra AI.")
	a.n.Alert(Targets{cfg.ChatID, cfg.ZaloChat}, b.String())
}
