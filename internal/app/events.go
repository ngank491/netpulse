package app

import (
	"net/netip"
	"strings"
)

// Extra router events. All functions here run with rs.mu held.

// lifecycleEvents: first contact, reboots and RouterOS upgrades.
func (h *Hub) lifecycleEvents(rs *RState, eff *effects, res Row, uptime float64, prevSeen, now int64) {
	ver := strings.TrimSpace(res["version"])
	switch {
	case prevSeen == 0:
		desc := strings.Trim(strings.Join([]string{res["board-name"], "RouterOS " + ver}, " · "), " ·")
		eff.add(now, "info", "▶", "Bắt đầu giám sát router ("+desc+")", false)
		rs.dirty = true
	default:
		if rs.Uptime > 0 && uptime > 0 {
			// expected uptime if the router had kept running since we last saw it
			expected := rs.Uptime + float64(now-prevSeen)/1000
			if uptime+120 < expected {
				eff.add(now, "warning", "🔄", "Router vừa khởi động lại (đã chạy "+humanDur(int64(uptime*1000))+")", true)
				rs.dirty = true
			}
		}
		if ver != "" && rs.Version != "" && ver != rs.Version {
			eff.add(now, "info", "⬆️", "RouterOS đổi phiên bản: "+rs.Version+" → "+ver, true)
			rs.dirty = true
		}
	}
	if uptime > 0 {
		rs.Uptime = uptime
	}
	if ver != "" {
		rs.Version = ver
	}
}

var trackedPortPrefixes = []string{"ether", "sfp", "combo", "wlan", "wifi", "lte", "vlan", "bridge"}

func trackedPort(typ string) bool {
	for _, p := range trackedPortPrefixes {
		if strings.HasPrefix(typ, p) {
			return true
		}
	}
	return false
}

// portEvents logs link up/down of physical ports, VLANs and bridges.
// WAN interfaces are excluded: they have their own (notifying) events.
func (h *Hub) portEvents(rs *RState, eff *effects, list []Iface, now int64) {
	wan := map[string]bool{}
	for _, w := range rs.cfg.WANs {
		wan[w.Iface] = true
	}
	present := map[string]bool{}
	for _, it := range list {
		if wan[it.Name] || it.Disabled || !trackedPort(it.Type) {
			continue
		}
		present[it.Name] = true
		st := rs.IfLinks[it.Name]
		if st == nil {
			st = newProbe()
			rs.IfLinks[it.Name] = st
		}
		label := it.Name
		if it.Comment != "" {
			label += " (" + it.Comment + ")"
		}
		prev, ds := st.Status, st.Since
		switch st.step(it.Running, false, now, -1, 2) {
		case "down":
			if prev == "up" { // ports that were never up stay quiet
				eff.add(now, "warning", "🔌", "Cổng "+label+": mất link", false)
			}
			rs.dirty = true
		case "up":
			eff.add(now, "info", "🔌", "Cổng "+label+": có link trở lại sau "+humanDur(now-ds), false)
			rs.dirty = true
		}
	}
	for name := range rs.IfLinks {
		if !present[name] {
			delete(rs.IfLinks, name)
		}
	}
}

// wanPublicIPs maps WAN interfaces (configured or detected) to their public IPv4/IPv6.
func wanPublicIPs(rs *RState) map[string]string {
	wans := map[string]bool{}
	for _, w := range rs.cfg.WANs {
		wans[w.Iface] = true
	}
	for _, n := range detectWANs(rs.Sections) {
		wans[n] = true
	}
	out := map[string]string{}
	for _, a := range rs.Sections["address"] {
		if truthy(a["disabled"]) || truthy(a["invalid"]) || !wans[a["interface"]] {
			continue
		}
		s := strings.SplitN(a["address"], "/", 2)[0]
		ip, err := netip.ParseAddr(s)
		if err != nil || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			continue
		}
		out[a["interface"]] = s
	}
	return out
}

func (h *Hub) wanIPEvents(rs *RState, eff *effects, now int64) {
	for iface, ip := range wanPublicIPs(rs) {
		old := rs.WanIPs[iface]
		if old == ip {
			continue
		}
		if old != "" {
			eff.add(now, "info", "🌐", "IP public của WAN "+rs.wanLabel(iface)+" đổi: "+old+" → "+ip, false)
		}
		rs.WanIPs[iface] = ip
		rs.dirty = true
	}
}
