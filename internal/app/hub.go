package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProbeState is a small state machine with hysteresis: it only reports
// "down" after `threshold` consecutive failures and ignores checks that
// could not run at all (errors), so a flaky ping never pages anyone.
type ProbeState struct {
	Status    string  `json:"status"` // unknown | up | down | error
	Fails     int     `json:"fails"`
	Since     int64   `json:"since"`
	LastCheck int64   `json:"lastCheck"`
	RTT       float64 `json:"rtt"`
	Alerted   bool    `json:"alerted"`
	LastAlert int64   `json:"lastAlert"`
	Error     string  `json:"error,omitempty"`
}

func newProbe() *ProbeState { return &ProbeState{Status: "unknown", RTT: -1} }

func (p *ProbeState) step(ok, errored bool, now int64, rtt float64, threshold int) string {
	p.LastCheck = now
	if errored {
		if p.Status == "unknown" {
			p.Status = "error"
		}
		return ""
	}
	p.Error = ""
	if ok {
		p.Fails, p.RTT = 0, rtt
		if p.Status != "up" {
			prev := p.Status
			p.Status, p.Since = "up", now
			if prev == "down" {
				return "up"
			}
		}
		return ""
	}
	p.Fails++
	p.RTT = -1
	if p.Fails >= threshold && p.Status != "down" {
		p.Status, p.Since = "down", now
		return "down"
	}
	return ""
}

type WanState struct {
	Link ProbeState `json:"link"`
	Inet ProbeState `json:"inet"`
}

type Iface struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Running  bool     `json:"running"`
	Disabled bool     `json:"disabled"`
	Comment  string   `json:"comment,omitempty"`
	MAC      string   `json:"mac,omitempty"`
	RxBytes  uint64   `json:"rxBytes"`
	TxBytes  uint64   `json:"txBytes"`
	RxBps    *float64 `json:"rxBps"`
	TxBps    *float64 `json:"txBps"`
}

// RState is the live state of one router.
type RState struct {
	id        int64 // immutable
	mu        sync.RWMutex
	cfg       Router
	monitors  []Monitor
	Conn      ProbeState
	LastSeen  int64
	LastErr   string
	Identity  string
	Resource  Row
	Ifaces    []Iface
	Sections  map[string][]Row
	SectionAt map[string]int64
	Probes    map[int64]*ProbeState
	WANs      map[string]*WanState
	IfLinks   map[string]*ProbeState // link state of every port (events)
	WanIPs    map[string]string      // last public IP per WAN interface
	Version   string                 // last RouterOS version seen
	Uptime    float64                // last uptime seen (reboot detection)
	Clients   map[string]*Client     // by MAC

	aliases          map[string]string
	clientsReady     bool
	prevBytes        map[string][2]uint64
	prevAt           int64
	prevUptime       float64
	acc              accum
	push             map[string]*pushBuf
	lastPushSections int64
	lastFast         int64
	dirty            bool
	lastSave         int64

	stop context.CancelFunc
	done chan struct{}
}

type persisted struct {
	Conn      ProbeState             `json:"conn"`
	LastSeen  int64                  `json:"lastSeen"`
	LastErr   string                 `json:"lastErr"`
	Identity  string                 `json:"identity"`
	Resource  Row                    `json:"resource"`
	Ifaces    []Iface                `json:"ifaces"`
	Sections  map[string][]Row       `json:"sections"`
	SectionAt map[string]int64       `json:"sectionAt"`
	Probes    map[int64]*ProbeState  `json:"probes"`
	WANs      map[string]*WanState   `json:"wans"`
	IfLinks   map[string]*ProbeState `json:"ifLinks"`
	WanIPs    map[string]string      `json:"wanIps"`
	Version   string                 `json:"version"`
	Uptime    float64                `json:"uptime"`
}

func newRState(r Router, mons []Monitor) *RState {
	return &RState{id: r.ID, cfg: r, monitors: mons, Conn: *newProbe(), Sections: map[string][]Row{}, SectionAt: map[string]int64{},
		Probes: map[int64]*ProbeState{}, WANs: map[string]*WanState{}, push: map[string]*pushBuf{},
		IfLinks: map[string]*ProbeState{}, WanIPs: map[string]string{}, Clients: map[string]*Client{}, aliases: map[string]string{}}
}

func (rs *RState) restore(b []byte) {
	var p persisted
	if json.Unmarshal(b, &p) != nil {
		return
	}
	rs.Conn, rs.LastSeen, rs.LastErr, rs.Identity, rs.Resource, rs.Ifaces = p.Conn, p.LastSeen, p.LastErr, p.Identity, p.Resource, p.Ifaces
	if p.Sections != nil {
		rs.Sections = p.Sections
	}
	if p.SectionAt != nil {
		rs.SectionAt = p.SectionAt
	}
	if p.Probes != nil {
		rs.Probes = p.Probes
	}
	if p.WANs != nil {
		rs.WANs = p.WANs
	}
	if p.IfLinks != nil {
		rs.IfLinks = p.IfLinks
	}
	if p.WanIPs != nil {
		rs.WanIPs = p.WanIPs
	}
	rs.Version, rs.Uptime = p.Version, p.Uptime
	// Rates from before the restart are meaningless.
	for i := range rs.Ifaces {
		rs.Ifaces[i].RxBps, rs.Ifaces[i].TxBps = nil, nil
	}
	rs.prune()
}

// snapshotLocked must be called with rs.mu held.
func (rs *RState) snapshotLocked() []byte {
	b, _ := json.Marshal(persisted{rs.Conn, rs.LastSeen, rs.LastErr, rs.Identity, rs.Resource, rs.Ifaces, rs.Sections, rs.SectionAt, rs.Probes, rs.WANs,
		rs.IfLinks, rs.WanIPs, rs.Version, rs.Uptime})
	return b
}

// prune drops state for monitors/WANs that are no longer configured.
func (rs *RState) prune() {
	keep := map[int64]bool{}
	for _, m := range rs.monitors {
		keep[m.ID] = true
	}
	for id := range rs.Probes {
		if !keep[id] {
			delete(rs.Probes, id)
		}
	}
	wans := map[string]bool{}
	for _, w := range rs.cfg.WANs {
		wans[w.Iface] = true
	}
	for n := range rs.WANs {
		if !wans[n] {
			delete(rs.WANs, n)
		}
	}
}

func (rs *RState) monitor(id int64) *Monitor {
	for i := range rs.monitors {
		if rs.monitors[i].ID == id {
			return &rs.monitors[i]
		}
	}
	return nil
}

func (rs *RState) probe(id int64) *ProbeState {
	p := rs.Probes[id]
	if p == nil {
		p = newProbe()
		rs.Probes[id] = p
	}
	return p
}

func (rs *RState) wan(name string) *WanState {
	w := rs.WANs[name]
	if w == nil {
		w = &WanState{Link: *newProbe(), Inet: *newProbe()}
		rs.WANs[name] = w
	}
	return w
}

func (rs *RState) wanLabel(iface string) string {
	for _, w := range rs.cfg.WANs {
		if w.Iface == iface && w.Label != "" {
			return w.Label + " (" + iface + ")"
		}
	}
	return iface
}

// ---------- effects produced by an observation, applied outside the lock ----------

type alert struct {
	at     int64
	level  string // info | warning
	icon   string
	msg    string
	event  bool
	notify bool
}

type probeRec struct {
	id int64
	at int64
	ok bool
}

type effects struct {
	alerts []alert
	sample *sampleRow
	probes []probeRec
	state  []byte
}

func (e *effects) add(at int64, level, icon, msg string, notify bool) {
	e.alerts = append(e.alerts, alert{at: at, level: level, icon: icon, msg: msg, event: true, notify: notify})
}

// ---------- hub ----------

type Hub struct {
	st       *Store
	box      *Box
	settings *SettingsStore
	notify   *Notifier
	demo     bool

	resolver *resolver

	ctl sync.Mutex // serialises Reload/Remove
	mu  sync.RWMutex
	rs  map[int64]*RState
	ctx context.Context
	wg  sync.WaitGroup
}

func NewHub(st *Store, box *Box, settings *SettingsStore, demo bool) *Hub {
	return &Hub{st: st, box: box, settings: settings, demo: demo, rs: map[int64]*RState{}, resolver: newResolver()}
}

func (h *Hub) Start(ctx context.Context) error {
	h.ctx = ctx
	routers, err := h.st.ListRouters()
	if err != nil {
		return err
	}
	for _, r := range routers {
		if err := h.load(r, true); err != nil {
			return err
		}

	}
	h.wg.Add(2)
	go func() {
		defer h.wg.Done()
		h.housekeeping(ctx)
	}()
	go func() {
		defer h.wg.Done()
		h.runResolver(ctx)
	}()
	return nil
}

// Wait blocks until all loops stopped, then persists every router's state.
func (h *Hub) Wait() {
	h.wg.Wait()
	h.mu.RLock()
	defer h.mu.RUnlock()
	for id, rs := range h.rs {
		rs.mu.RLock()
		b := rs.snapshotLocked()
		rs.mu.RUnlock()
		if err := h.st.SaveState(id, b); err != nil {
			log.Printf("lưu trạng thái router %d: %v", id, err)
		}
	}
}

func (h *Hub) get(id int64) *RState {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.rs[id]
}

func (h *Hub) all() []*RState {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*RState, 0, len(h.rs))
	names := map[*RState]string{}
	for _, rs := range h.rs {
		rs.mu.RLock()
		names[rs] = strings.ToLower(rs.cfg.Name)
		rs.mu.RUnlock()
		out = append(out, rs)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := names[out[i]], names[out[j]]
		if a == b {
			return out[i].id < out[j].id
		}
		return a < b
	})
	return out
}

func (h *Hub) load(r *Router, onStart bool) error {
	mons, err := h.st.ListMonitors(r.ID)
	if err != nil {
		return err
	}
	rs := newRState(*r, mons)
	if b, err := h.st.LoadState(r.ID); err != nil {
		return err
	} else if b != nil {
		rs.restore(b)
	}
	if err := h.loadClients(rs); err != nil {
		return err
	}
	h.mu.Lock()
	h.rs[r.ID] = rs
	h.mu.Unlock()
	if onStart && r.Enabled && rs.LastSeen > 0 {
		// explains gaps in charts and history after an update or reboot of the server;
		// read before the poll loop starts touching rs
		gap := nowMs() - rs.LastSeen
		msg := "NetPulse " + Version + " khởi động, tiếp tục giám sát"
		if gap > 2*60_000 {
			msg += " (đã gián đoạn " + humanDur(gap) + ")"
		}
		h.st.AddEvent(r.ID, nowMs(), "info", msg)
	}
	h.startLoop(rs)
	return nil
}

// Reload re-reads a router (and its monitors) from the database.
func (h *Hub) Reload(id int64) error {
	h.ctl.Lock()
	defer h.ctl.Unlock()
	r, err := h.st.GetRouter(id)
	if errors.Is(err, ErrNotFound) {
		if rs := h.get(id); rs != nil {
			h.stopLoop(rs)
			h.mu.Lock()
			delete(h.rs, id)
			h.mu.Unlock()
		}
		return nil
	}
	if err != nil {
		return err
	}
	rs := h.get(id)
	if rs == nil {
		return h.load(r, false)
	}
	mons, err := h.st.ListMonitors(id)
	if err != nil {
		return err
	}
	h.stopLoop(rs)
	rs.mu.Lock()
	modeChanged := rs.cfg.Mode != r.Mode || rs.cfg.Host != r.Host
	rs.cfg, rs.monitors = *r, mons
	if modeChanged {
		rs.Conn, rs.LastSeen, rs.LastErr = *newProbe(), 0, ""
		rs.prevAt = 0
	}
	rs.prune()
	rs.dirty = true
	rs.mu.Unlock()
	h.startLoop(rs)
	return nil
}

// ReloadMonitors refreshes only the ping targets (no reconnect).
func (h *Hub) ReloadMonitors(routerID int64) error {
	rs := h.get(routerID)
	if rs == nil {
		return nil
	}
	mons, err := h.st.ListMonitors(routerID)
	if err != nil {
		return err
	}
	rs.mu.Lock()
	rs.monitors = mons
	rs.prune()
	rs.mu.Unlock()
	return nil
}

func (h *Hub) startLoop(rs *RState) {
	rs.mu.RLock()
	cfg := rs.cfg
	rs.mu.RUnlock()
	if !cfg.Enabled || cfg.Mode == "push" || (cfg.Mode == "demo" && !h.demo) {
		return
	}
	src, err := h.source(&cfg)
	if err != nil {
		rs.mu.Lock()
		rs.LastErr = err.Error()
		rs.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	rs.stop, rs.done = cancel, done
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer close(done)
		h.runPull(ctx, rs, src, cfg.Interval)
	}()
}

func (h *Hub) stopLoop(rs *RState) {
	if rs.stop != nil {
		rs.stop()
		<-rs.done
		rs.stop, rs.done = nil, nil
	}
}

func (h *Hub) source(r *Router) (Source, error) {
	switch r.Mode {
	case "demo":
		return newDemoSource(r.ID), nil
	case "pull":
		pass, err := h.box.Open(r.secret)
		if err != nil {
			return nil, err
		}
		return newRestSource(r, pass), nil
	}
	return nil, fmt.Errorf("chế độ %q không hỗ trợ đọc trực tiếp", r.Mode)
}

// ---------- pull loop ----------

func (h *Hub) runPull(ctx context.Context, rs *RState, src Source, intervalS int) {
	interval := time.Duration(clampInt(intervalS, 5, 300)) * time.Second
	var lastSlow, lastProbe time.Time
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		h.pollOnce(ctx, rs, src, &lastSlow, &lastProbe)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (h *Hub) pollOnce(ctx context.Context, rs *RState, src Source, lastSlow, lastProbe *time.Time) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("router %d: lỗi không mong đợi: %v", rs.id, r)
		}
	}()
	fctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	res, err := src.Fetch(fctx, "/system/resource", resourceProps)
	var ifs []Row
	if err == nil {
		ifs, err = src.Fetch(fctx, "/interface", ifaceProps)
	}
	cancel()
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		h.observeFailure(rs, err.Error(), nowMs())
		return
	}
	var resRow Row
	if len(res) > 0 {
		resRow = res[0]
	}
	h.observeFast(rs, resRow, ifs, nowMs())

	if time.Since(*lastSlow) >= 59*time.Second {
		*lastSlow = time.Now()
		for _, sec := range sections {
			sctx, c := context.WithTimeout(ctx, 20*time.Second)
			rows, err := src.Fetch(sctx, sec.Path, sec.Props)
			c()
			if ctx.Err() != nil {
				return
			}
			if err != nil && !errors.Is(err, errNotSupported) {
				continue // keep previous data on transient errors
			}
			h.observeSection(rs, sec.Name, rows, nowMs())
		}
		h.updateClients(rs, nowMs())
	}
	if time.Since(*lastProbe) >= 29*time.Second {
		*lastProbe = time.Now()
		h.runProbes(ctx, rs, src)
	}
}

type probeOut struct {
	res PingResult
	err error
}

func (h *Hub) runProbes(ctx context.Context, rs *RState, src Source) {
	type job struct {
		mon         int64
		wan         string
		addr, iface string
	}
	var jobs []job
	rs.mu.RLock()
	for _, m := range rs.monitors {
		if m.Pingable() {
			jobs = append(jobs, job{mon: m.ID, addr: m.IP})
		}
	}
	for _, w := range rs.cfg.WANs {
		jobs = append(jobs, job{wan: w.Iface, addr: rs.cfg.WANTarget, iface: w.Iface})
	}
	rs.mu.RUnlock()
	if len(jobs) == 0 {
		return
	}
	out := make([]probeOut, len(jobs))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pctx, c := context.WithTimeout(ctx, 15*time.Second)
			defer c()
			res, err := src.Ping(pctx, j.addr, j.iface)
			out[i] = probeOut{res, err}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	mons, wans := map[int64]probeOut{}, map[string]probeOut{}
	for i, j := range jobs {
		if j.wan != "" {
			wans[j.wan] = out[i]
		} else {
			mons[j.mon] = out[i]
		}
	}
	h.observeProbes(rs, mons, wans, nowMs())
}

// ---------- observations (shared by pull, push and demo) ----------

func (h *Hub) finish(rs *RState, eff *effects, now int64) {
	// must hold rs.mu; decides whether to persist state
	if rs.dirty || now-rs.lastSave > 5*60*1000 {
		eff.state = rs.snapshotLocked()
		rs.dirty, rs.lastSave = false, now
	}
}

func (h *Hub) observeFailure(rs *RState, msg string, now int64) {
	var eff effects
	rs.mu.Lock()
	rs.LastErr = msg
	if rs.Conn.step(false, false, now, -1, 3) == "down" {
		eff.add(now, "warning", "🔴", "Mất kết nối tới router: "+msg, true)
		rs.Conn.Alerted, rs.Conn.LastAlert = true, now
		rs.dirty = true
	}
	h.finish(rs, &eff, now)
	cfg := rs.cfg
	rs.mu.Unlock()
	h.apply(cfg, eff)
}

func (h *Hub) observeFast(rs *RState, res Row, ifs []Row, now int64) {
	var eff effects
	rs.mu.Lock()
	prevSeen := rs.LastSeen
	rs.LastSeen, rs.LastErr = now, ""
	downSince := rs.Conn.Since
	if rs.Conn.step(true, false, now, -1, 3) == "up" && rs.Conn.Alerted {
		eff.add(now, "info", "🟢", "Router kết nối trở lại sau "+humanDur(now-downSince), true)
		rs.Conn.Alerted = false
		rs.dirty = true
	}

	uptime := rosSeconds(res["uptime"])
	elapsed := float64(now-rs.prevAt) / 1000
	valid := rs.prevAt > 0 && elapsed > 0.5 && elapsed < 600 && uptime >= rs.prevUptime
	next := make(map[string][2]uint64, len(ifs))
	list := make([]Iface, 0, len(ifs))
	for _, r := range ifs {
		name := r["name"]
		if name == "" || strings.HasSuffix(r["type"], "-in") { // skip per-client dynamic tunnels
			continue
		}
		it := Iface{Name: name, Type: r["type"], Running: truthy(r["running"]), Disabled: truthy(r["disabled"]), Comment: r["comment"], MAC: r["mac-address"]}
		rx, ok1 := parseUint(r["rx-byte"])
		tx, ok2 := parseUint(r["tx-byte"])
		it.RxBytes, it.TxBytes = rx, tx
		if ok1 && ok2 {
			next[name] = [2]uint64{rx, tx}
			if p, ok := rs.prevBytes[name]; ok && valid && rx >= p[0] && tx >= p[1] {
				a, b := float64(rx-p[0])*8/elapsed, float64(tx-p[1])*8/elapsed
				it.RxBps, it.TxBps = &a, &b
			}
		}
		list = append(list, it)
		if len(list) >= 1000 {
			break
		}
	}
	rs.Ifaces, rs.prevBytes, rs.prevAt, rs.prevUptime = list, next, now, uptime
	if res != nil {
		rs.Resource = cleanRows([]Row{res}, resourceProps)[0]
	}
	h.lifecycleEvents(rs, &eff, res, uptime, prevSeen, now)
	h.portEvents(rs, &eff, list, now)

	for _, w := range rs.cfg.WANs {
		var it *Iface
		for i := range list {
			if list[i].Name == w.Iface {
				it = &list[i]
			}
		}
		ws := rs.wan(w.Iface)
		if it == nil {
			ws.Link.step(false, true, now, -1, 2)
			ws.Link.Error = "không thấy interface trên router"
			continue
		}
		ds := ws.Link.Since
		switch ws.Link.step(it.Running && !it.Disabled, false, now, -1, 2) {
		case "down":
			eff.add(now, "warning", "🔴", "WAN "+rs.wanLabel(w.Iface)+": mất link", true)
			ws.Link.Alerted, ws.Link.LastAlert, rs.dirty = true, now, true
		case "up":
			if ws.Link.Alerted {
				eff.add(now, "info", "🟢", "WAN "+rs.wanLabel(w.Iface)+": link phục hồi sau "+humanDur(now-ds), true)
			}
			ws.Link.Alerted, rs.dirty = false, true
		}
	}

	cpu, okc := parseFloat(res["cpu-load"])
	var cpuP, memP *float64
	if okc {
		cpuP = &cpu
	}
	if m, ok := memPct(res); ok {
		memP = &m
	}
	eff.sample = rs.acc.add(now, cpuP, memP, list)
	h.finish(rs, &eff, now)
	cfg := rs.cfg
	rs.mu.Unlock()
	h.apply(cfg, eff)
}

func (h *Hub) observeSection(rs *RState, name string, rows []Row, now int64) {
	sec := sectionByName(name)
	if sec == nil {
		return
	}
	rows = cleanRows(rows, sec.Props)
	rs.mu.Lock()
	rs.Sections[name], rs.SectionAt[name] = rows, now
	if name == "identity" && len(rows) > 0 {
		rs.Identity = rows[0]["name"]
	}
	var eff effects
	if name == "address" {
		h.wanIPEvents(rs, &eff, now)
	}
	h.finish(rs, &eff, now)
	cfg := rs.cfg
	rs.mu.Unlock()
	h.apply(cfg, eff)
}

func (h *Hub) observeProbes(rs *RState, mons map[int64]probeOut, wans map[string]probeOut, now int64) {
	var eff effects
	rs.mu.Lock()
	for id, o := range mons {
		m := rs.monitor(id)
		if m == nil || !m.Pingable() {
			continue
		}
		p := rs.probe(id)
		ok, errd := o.err == nil && o.res.Received > 0, o.err != nil
		if errd {
			p.Error = o.err.Error()
		}
		ds := p.Since
		target := m.Name + " (" + m.IP + ")"
		switch p.step(ok, errd, now, o.res.RTT, 2) {
		case "down":
			eff.add(now, "warning", "🔴", target+" mất kết nối", m.Notify)
			p.Alerted, p.LastAlert, rs.dirty = m.Notify, now, true
		case "up":
			eff.add(now, "info", "🟢", target+" hoạt động trở lại sau "+humanDur(now-ds), p.Alerted)
			p.Alerted, rs.dirty = false, true
		}
		if !errd {
			eff.probes = append(eff.probes, probeRec{id, now, ok})
		}
	}
	for iface, o := range wans {
		known := false
		for _, w := range rs.cfg.WANs {
			known = known || w.Iface == iface
		}
		if !known {
			continue
		}
		ws := rs.wan(iface)
		ok, errd := o.err == nil && o.res.Received > 0, o.err != nil
		if errd {
			ws.Inet.Error = o.err.Error()
		}
		ds := ws.Inet.Since
		switch ws.Inet.step(ok, errd, now, o.res.RTT, 2) {
		case "down":
			eff.add(now, "warning", "🔴", "WAN "+rs.wanLabel(iface)+": mất Internet (ping "+rs.cfg.WANTarget+" thất bại 2 lần)", true)
			ws.Inet.Alerted, ws.Inet.LastAlert, rs.dirty = true, now, true
		case "up":
			eff.add(now, "info", "🟢", "WAN "+rs.wanLabel(iface)+": Internet trở lại sau "+humanDur(now-ds), ws.Inet.Alerted)
			ws.Inet.Alerted, rs.dirty = false, true
		}
	}
	h.finish(rs, &eff, now)
	cfg := rs.cfg
	rs.mu.Unlock()
	h.apply(cfg, eff)
}

func (h *Hub) apply(cfg Router, eff effects) {
	id, name := cfg.ID, cfg.Name
	for _, a := range eff.alerts {
		if a.event {
			if err := h.st.AddEvent(id, a.at, a.level, a.msg); err != nil {
				log.Printf("ghi sự kiện: %v", err)
			}
		}
		if a.notify && h.notify != nil {
			h.notify.Alert(Targets{cfg.ChatID, cfg.ZaloChat}, a.icon+" ["+name+"] "+a.msg)
		}
	}
	if s := eff.sample; s != nil {
		if err := h.st.AddSample(id, s.at, s.cpu, s.mem, s.data); err != nil {
			log.Printf("ghi lịch sử: %v", err)
		}
	}
	for _, p := range eff.probes {
		if err := h.st.AddProbe(p.id, p.at, p.ok); err != nil {
			log.Printf("ghi thống kê ping: %v", err)
			break
		}
	}
	if eff.state != nil {
		if err := h.st.SaveState(id, eff.state); err != nil {
			log.Printf("lưu trạng thái: %v", err)
		}
	}
}

// ---------- housekeeping: reminders, push watchdog, summary, retention ----------

func (h *Hub) housekeeping(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	lastPurge := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := nowMs()
		h.tick(now)
		h.maybeSummary(time.Now())
		if time.Since(lastPurge) > time.Hour {
			lastPurge = time.Now()
			days := h.settings.Get().RetentionDays
			if err := h.st.Purge(now - int64(days)*86400000); err != nil {
				log.Printf("dọn dữ liệu cũ: %v", err)
			}
		}
	}
}

func (h *Hub) tick(now int64) {
	repeat := int64(h.settings.Get().RepeatMin) * 60000
	loc := h.settings.Loc()
	for _, rs := range h.all() {
		var eff effects
		rs.mu.Lock()
		cfg := rs.cfg
		if cfg.Enabled && cfg.Mode == "push" && rs.LastSeen > 0 {
			limit := max(int64(90000), int64(cfg.Interval)*3000)
			if now-rs.LastSeen > limit && rs.Conn.Status != "down" {
				rs.Conn.Status, rs.Conn.Since, rs.Conn.Alerted, rs.Conn.LastAlert = "down", rs.LastSeen, true, now
				rs.prevAt = 0
				eff.add(now, "warning", "🔴", "Router không gửi dữ liệu từ "+clock(rs.LastSeen, loc), true)
				rs.dirty = true
			}
		}
		remind := func(p *ProbeState, every int64, what string) {
			if p.Status == "down" && p.Alerted && every > 0 && now-p.LastAlert >= every {
				p.LastAlert = now
				eff.alerts = append(eff.alerts, alert{at: now, icon: "⏰", notify: true,
					msg: what + " vẫn lỗi từ " + clock(p.Since, loc) + " (" + humanDur(now-p.Since) + ")"})
			}
		}
		if cfg.Enabled {
			remind(&rs.Conn, repeat, "Kết nối router")
			for name, w := range rs.WANs {
				remind(&w.Link, repeat, "Link WAN "+rs.wanLabel(name))
				remind(&w.Inet, repeat, "Internet WAN "+rs.wanLabel(name))
			}
			for id, p := range rs.Probes {
				if m := rs.monitor(id); m != nil && m.Notify && m.Pingable() {
					remind(p, int64(m.RepeatMin)*60000, m.Name+" ("+m.IP+")")
				}
			}
		}
		h.finish(rs, &eff, now)
		rs.mu.Unlock()
		h.apply(cfg, eff)
	}
}

func (h *Hub) maybeSummary(t time.Time) {
	s := h.settings.Get()
	if !s.SummaryOn || h.notify == nil || (s.AdminChat == "" && s.ZaloAdmin == "" && !h.anyRouterChat()) {
		return
	}
	local := t.In(h.settings.Loc())
	if local.Format("15:04") < s.SummaryAt {
		return
	}
	day := local.Format("2006-01-02")
	if last, _ := h.st.Get("summary_last"); last == day {
		return
	}
	if err := h.st.Set("summary_last", day); err != nil {
		return
	}
	// Do not send a stale summary after a long downtime (e.g. started at 23:00).
	if local.Format("15:04") > addMinutes(s.SummaryAt, 30) {
		return
	}
	h.notify.ToAdmins(h.SummaryText(nil))
	for _, rs := range h.all() {
		rs.mu.RLock()
		t, id := Targets{rs.cfg.ChatID, rs.cfg.ZaloChat}, rs.id
		rs.mu.RUnlock()
		for _, c := range channels {
			if chat := t.For(c.Name); chat != "" && chat != h.settings.AdminChat(c.Name) {
				h.notify.Send(c.Name, chat, h.SummaryText(func(x int64) bool { return x == id }))
			}
		}
	}
}

func (h *Hub) anyRouterChat() bool {
	for _, rs := range h.all() {
		rs.mu.RLock()
		ok := rs.cfg.ChatID != "" || rs.cfg.ZaloChat != ""
		rs.mu.RUnlock()
		if ok {
			return true
		}
	}
	return false
}

func addMinutes(hhmm string, n int) string {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return hhmm
	}
	r := t.Add(time.Duration(n) * time.Minute)
	if r.Day() != t.Day() {
		return "23:59"
	}
	return r.Format("15:04")
}

// SummaryText renders a plain-text status report; filter nil = all routers.
func (h *Hub) SummaryText(filter func(int64) bool) string {
	loc := h.settings.Loc()
	now := nowMs()
	var b strings.Builder
	fmt.Fprintf(&b, "📊 NetPulse – tình trạng lúc %s\n", clock(now, loc))
	n := 0
	for _, v := range h.Views(filter, false) {
		n++
		icon := map[string]string{"up": "🟢", "down": "🔴", "disabled": "⚪"}[v.Status]
		if icon == "" {
			icon = "🟡"
		}
		fmt.Fprintf(&b, "\n%s %s", icon, v.Name)
		if v.Status == "up" {
			if v.CPU != nil {
				fmt.Fprintf(&b, " · CPU %.0f%%", *v.CPU)
			}
			if v.Mem != nil {
				fmt.Fprintf(&b, " · RAM %.0f%%", *v.Mem)
			}
		} else if v.Status == "down" {
			fmt.Fprintf(&b, " · mất kết nối từ %s", clock(v.Since, loc))
		}
		upW := 0
		for _, w := range v.WANs {
			if w.Configured && w.Inet.Status != "down" && w.Link.Status != "down" {
				upW++
			}
		}
		if nw := countConfigured(v.WANs); nw > 0 {
			fmt.Fprintf(&b, " · WAN %d/%d", upW, nw)
		}
		if v.MonitorsTotal > 0 {
			fmt.Fprintf(&b, " · thiết bị lỗi %d/%d", v.MonitorsDown, v.MonitorsTotal)
		}
		for _, w := range v.WANs {
			if w.Configured && (w.Inet.Status == "down" || w.Link.Status == "down") {
				fmt.Fprintf(&b, "\n   🔴 WAN %s", w.Iface)
				if w.Label != "" {
					fmt.Fprintf(&b, " – %s", w.Label)
				}
			}
		}
		for _, d := range v.DownList {
			fmt.Fprintf(&b, "\n   🔴 %s", d)
		}
		if c, err := h.st.CountEventsSince(v.ID, now-86400000, "warning"); err == nil && c > 0 {
			fmt.Fprintf(&b, "\n   ⚠ %d cảnh báo trong 24 giờ", c)
		}
	}
	if n == 0 {
		b.WriteString("\nChưa có router nào.")
	}
	return b.String()
}

func countConfigured(ws []WanView) (n int) {
	for _, w := range ws {
		if w.Configured {
			n++
		}
	}
	return
}

// ---------- views for the API ----------

type MonitorView struct {
	Monitor
	State   *ProbeState `json:"state"`
	Avail24 *float64    `json:"avail24"`
}

type WanView struct {
	Iface      string     `json:"iface"`
	Label      string     `json:"label"`
	Configured bool       `json:"configured"`
	Running    *bool      `json:"running"`
	Link       ProbeState `json:"link"`
	Inet       ProbeState `json:"inet"`
	RxBps      *float64   `json:"rxBps"`
	TxBps      *float64   `json:"txBps"`
	PublicIP   string     `json:"publicIp"`
}

type RouterView struct {
	ID            int64            `json:"id"`
	Name          string           `json:"name"`
	Mode          string           `json:"mode"`
	Status        string           `json:"status"` // up | down | waiting | disabled
	Since         int64            `json:"since"`
	LastSeen      int64            `json:"lastSeen"`
	LastErr       string           `json:"lastErr"`
	Identity      string           `json:"identity"`
	Resource      Row              `json:"resource"`
	CPU           *float64         `json:"cpu"`
	Mem           *float64         `json:"mem"`
	Interval      int              `json:"interval"`
	WANTarget     string           `json:"wanTarget"`
	WANs          []WanView        `json:"wans"`
	DetectedWANs  []string         `json:"detectedWans"`
	MonitorsTotal int              `json:"monitorsTotal"`
	MonitorsDown  int              `json:"monitorsDown"`
	DownList      []string         `json:"downList"`
	ClientsTotal  int              `json:"clientsTotal"`
	ClientsOnline int              `json:"clientsOnline"`
	Clients       []Client         `json:"clients"`
	Ifaces        []Iface          `json:"ifaces"`
	Monitors      []MonitorView    `json:"monitors"`
	Sections      map[string][]Row `json:"sections"`
	SectionAt     map[string]int64 `json:"sectionAt"`
	MapX          *float64         `json:"mapX"`
	MapY          *float64         `json:"mapY"`
}

func (h *Hub) Views(filter func(int64) bool, detail bool) []RouterView {
	out := []RouterView{}
	for _, rs := range h.all() {
		if filter != nil && !filter(rs.id) {
			continue
		}
		out = append(out, h.view(rs, detail))
	}
	return out
}

func (h *Hub) View(id int64) (RouterView, bool) {
	rs := h.get(id)
	if rs == nil {
		return RouterView{}, false
	}
	return h.view(rs, true), true
}

func (h *Hub) view(rs *RState, detail bool) RouterView {
	var avail map[int64]float64
	if detail {
		avail, _ = h.st.Availability(rs.id, nowMs()-86400000)
	}
	loc := h.settings.Loc()
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	c := rs.cfg
	v := RouterView{ID: c.ID, Name: c.Name, Mode: c.Mode, Status: rs.Conn.Status, Since: rs.Conn.Since, LastSeen: rs.LastSeen,
		LastErr: rs.LastErr, Identity: rs.Identity, Resource: rs.Resource, Interval: c.Interval, WANTarget: c.WANTarget,
		MapX: c.MapX, MapY: c.MapY, DownList: []string{}}
	switch {
	case !c.Enabled:
		v.Status = "disabled"
	case rs.LastSeen == 0:
		v.Status = "waiting"
	case v.Status != "down":
		v.Status = "up"
	}
	if f, ok := parseFloat(rs.Resource["cpu-load"]); ok {
		v.CPU = &f
	}
	if m, ok := memPct(rs.Resource); ok {
		v.Mem = &m
	}
	ifByName := map[string]*Iface{}
	for i := range rs.Ifaces {
		ifByName[rs.Ifaces[i].Name] = &rs.Ifaces[i]
	}
	v.DetectedWANs = detectWANs(rs.Sections)
	wanView := func(iface, label string, configured bool) WanView {
		w := WanView{Iface: iface, Label: label, Configured: configured, Link: *newProbe(), Inet: *newProbe()}
		if it := ifByName[iface]; it != nil {
			r := it.Running && !it.Disabled
			w.Running, w.RxBps, w.TxBps = &r, it.RxBps, it.TxBps
		}
		if s := rs.WANs[iface]; s != nil && configured {
			w.Link, w.Inet = s.Link, s.Inet
		}
		w.PublicIP = rs.WanIPs[iface]
		return w
	}
	seen := map[string]bool{}
	for _, w := range c.WANs {
		v.WANs = append(v.WANs, wanView(w.Iface, w.Label, true))
		seen[w.Iface] = true
	}
	for _, name := range v.DetectedWANs {
		if !seen[name] {
			v.WANs = append(v.WANs, wanView(name, "", false))
		}
	}
	if v.WANs == nil {
		v.WANs = []WanView{}
	}
	for _, m := range rs.monitors {
		if !m.Pingable() {
			continue
		}
		v.MonitorsTotal++
		if p := rs.Probes[m.ID]; p != nil && p.Status == "down" {
			v.MonitorsDown++
			if len(v.DownList) < 10 {
				v.DownList = append(v.DownList, fmt.Sprintf("%s (%s) từ %s", m.Name, m.IP, clock(p.Since, loc)))
			}
		}
	}
	for _, c := range rs.Clients {
		if c.IP == "" {
			continue
		}
		v.ClientsTotal++
		if c.Online {
			v.ClientsOnline++
		}
	}
	if detail {
		v.Clients = rs.clientList()
		v.Ifaces = append([]Iface{}, rs.Ifaces...)
		v.Sections, v.SectionAt = map[string][]Row{}, map[string]int64{}
		for k, rows := range rs.Sections {
			v.Sections[k] = rows
			v.SectionAt[k] = rs.SectionAt[k]
		}
		v.Monitors = []MonitorView{}
		for _, m := range rs.monitors {
			mv := MonitorView{Monitor: m}
			if p := rs.Probes[m.ID]; p != nil {
				cp := *p
				mv.State = &cp
			}
			if a, ok := avail[m.ID]; ok {
				a = math.Round(a*10000) / 100
				mv.Avail24 = &a
			}
			v.Monitors = append(v.Monitors, mv)
		}
	}
	return v
}

// detectWANs guesses Internet-facing interfaces from router data.
func detectWANs(sec map[string][]Row) []string {
	var order []string
	set := map[string]bool{}
	add := func(n string) {
		if n != "" && !set[n] {
			set[n] = true
			order = append(order, n)
		}
	}
	for _, r := range sec["wan_list"] {
		if !truthy(r["disabled"]) {
			add(r["interface"])
		}
	}
	for _, r := range sec["pppoe_client"] {
		if !truthy(r["disabled"]) {
			add(r["name"])
		}
	}
	for _, r := range sec["dhcp_client"] {
		if !truthy(r["disabled"]) && r["add-default-route"] != "no" {
			add(r["interface"])
		}
	}
	for _, r := range sec["route"] {
		if truthy(r["disabled"]) {
			continue
		}
		for _, gw := range []string{r["immediate-gw"], r["gateway"]} {
			if i := strings.LastIndex(gw, "%"); i >= 0 {
				add(gw[i+1:])
			} else if gw != "" && net.ParseIP(gw) == nil {
				add(gw)
			}
		}
	}
	// A PPPoE client rides on a physical port: the port is transport, not a WAN.
	for _, r := range sec["pppoe_client"] {
		if set[r["name"]] && set[r["interface"]] {
			delete(set, r["interface"])
		}
	}
	out := []string{}
	for _, n := range order {
		if set[n] {
			out = append(out, n)
		}
	}
	return out
}

// ---------- per-minute history ----------

type accum struct {
	minute int64
	n      int
	cpuSum float64
	cpuN   int
	memSum float64
	memN   int
	rx, tx map[string]float64
	cnt    map[string]int
}

type sampleRow struct {
	at       int64
	cpu, mem *float64
	data     []byte
}

func (a *accum) add(now int64, cpu, mem *float64, ifs []Iface) *sampleRow {
	m := now / 60000 * 60000
	var out *sampleRow
	if a.minute != m {
		if a.minute != 0 {
			out = a.flush()
		}
		*a = accum{minute: m, rx: map[string]float64{}, tx: map[string]float64{}, cnt: map[string]int{}}
	}
	a.n++
	if cpu != nil {
		a.cpuSum += *cpu
		a.cpuN++
	}
	if mem != nil {
		a.memSum += *mem
		a.memN++
	}
	for _, it := range ifs {
		if it.RxBps == nil || it.TxBps == nil || it.Disabled {
			continue
		}
		if _, ok := a.cnt[it.Name]; !ok && len(a.cnt) >= 64 {
			continue
		}
		a.rx[it.Name] += *it.RxBps
		a.tx[it.Name] += *it.TxBps
		a.cnt[it.Name]++
	}
	return out
}

func (a *accum) flush() *sampleRow {
	if a.n == 0 {
		return nil
	}
	s := &sampleRow{at: a.minute}
	if a.cpuN > 0 {
		v := math.Round(a.cpuSum/float64(a.cpuN)*10) / 10
		s.cpu = &v
	}
	if a.memN > 0 {
		v := math.Round(a.memSum/float64(a.memN)*10) / 10
		s.mem = &v
	}
	data := make(map[string][2]float64, len(a.cnt))
	for name, c := range a.cnt {
		data[name] = [2]float64{math.Round(a.rx[name] / float64(c)), math.Round(a.tx[name] / float64(c))}
	}
	s.data, _ = json.Marshal(data)
	return s
}

// ---------- helpers ----------

func memPct(res Row) (float64, bool) {
	total, ok1 := parseFloat(res["total-memory"])
	free, ok2 := parseFloat(res["free-memory"])
	if !ok1 || !ok2 || total <= 0 {
		return 0, false
	}
	return math.Round((total-free)/total*1000) / 10, true
}

func clampInt(v, lo, hi int) int {
	return min(max(v, lo), hi)
}

func humanDur(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	s := ms / 1000
	switch {
	case s < 60:
		return fmt.Sprintf("%d giây", s)
	case s < 3600:
		return fmt.Sprintf("%d phút", s/60)
	case s < 86400:
		return fmt.Sprintf("%d giờ %d phút", s/3600, s%3600/60)
	}
	return fmt.Sprintf("%d ngày %d giờ", s/86400, s%86400/3600)
}

func clock(ms int64, loc *time.Location) string {
	if ms == 0 {
		return "?"
	}
	t := time.UnixMilli(ms).In(loc)
	if time.Since(t) < 20*time.Hour {
		return t.Format("15:04")
	}
	return t.Format("15:04 02/01")
}
