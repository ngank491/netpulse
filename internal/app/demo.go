package app

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"
)

// demoSource simulates a RouterOS device so the UI can be tried without hardware.
type demoSource struct {
	mu      sync.Mutex
	started time.Time
	bytes   map[string][2]uint64
	last    time.Time
}

func newDemoSource(seed int64) *demoSource {
	return &demoSource{started: time.Now().Add(-time.Duration(3+seed) * 24 * time.Hour), bytes: map[string][2]uint64{}}
}

var demoIfaces = []struct {
	name, typ string
	rx, tx    float64 // average bps
}{
	{"ether1", "ether", 0, 0}, {"pppoe-vnpt", "pppoe-out", 48e6, 9e6}, {"ether2", "ether", 22e6, 4e6},
	{"ether3", "ether", 3e6, 25e6}, {"ether4", "ether", 1.2e6, 3e6}, {"ether5", "ether", 0, 0},
	{"bridge-lan", "bridge", 4e6, 30e6}, {"vlan10-cam", "vlan", 6e6, 0.4e6}, {"wlan1", "wlan", 2e6, 8e6},
}

func (d *demoSource) Fetch(ctx context.Context, path string, _ []string) ([]Row, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	switch strings.SplitN(path, "?", 2)[0] {
	case "/system/resource":
		up := now.Sub(d.started)
		return []Row{{"uptime": fmt.Sprintf("%dd%02d:%02d:%02d", int(up.Hours())/24, int(up.Hours())%24, int(up.Minutes())%60, int(up.Seconds())%60),
			"version": "7.18.2 (stable)", "board-name": "RB5009UG+S+", "architecture-name": "arm64",
			"cpu-load": strconv.Itoa(4 + rand.IntN(18)), "cpu-count": "4", "total-memory": "1073741824",
			"free-memory": strconv.Itoa(640000000 + rand.IntN(40000000)), "total-hdd-space": "1073741824", "free-hdd-space": "960000000"}}, nil
	case "/interface":
		dt := 15.0
		if !d.last.IsZero() {
			dt = now.Sub(d.last).Seconds()
		}
		d.last = now
		wave := 0.6 + 0.4*math.Sin(float64(now.Unix())/600)
		var rows []Row
		for _, it := range demoIfaces {
			b := d.bytes[it.name]
			b[0] += uint64(it.rx * wave * (0.8 + 0.4*rand.Float64()) * dt / 8)
			b[1] += uint64(it.tx * wave * (0.8 + 0.4*rand.Float64()) * dt / 8)
			d.bytes[it.name] = b
			running := it.name != "ether5"
			rows = append(rows, Row{"name": it.name, "type": it.typ, "running": strconv.FormatBool(running), "disabled": "false",
				"rx-byte": strconv.FormatUint(b[0], 10), "tx-byte": strconv.FormatUint(b[1], 10), "mac-address": "48:A9:8A:00:00:0" + it.name[len(it.name)-1:]})
		}
		return rows, nil
	case "/ip/address":
		return []Row{{"address": "192.168.88.1/24", "network": "192.168.88.0", "interface": "bridge-lan", "dynamic": "false"},
			{"address": "10.10.10.1/24", "network": "10.10.10.0", "interface": "vlan10-cam", "dynamic": "false"},
			{"address": "113.161.20.45/32", "network": "113.161.1.1", "interface": "pppoe-vnpt", "dynamic": "true"}}, nil
	case "/ip/dhcp-server/lease":
		var rows []Row
		names := []string{"PC-KETOAN", "LAPTOP-GD", "iPhone-Lan", "Galaxy-S24", "MAYIN-HP", "NAS-Synology", "AP-Tang2", "TV-PhongHop"}
		for i, n := range names {
			rows = append(rows, Row{"address": fmt.Sprintf("192.168.88.%d", 20+i), "active-address": fmt.Sprintf("192.168.88.%d", 20+i),
				"mac-address": fmt.Sprintf("DC:A6:32:00:10:%02X", i), "host-name": n, "server": "dhcp-lan", "status": "bound", "last-seen": fmt.Sprintf("%ds", rand.IntN(50))})
		}
		return rows, nil
	case "/ip/arp":
		return []Row{{"address": "192.168.88.20", "mac-address": "DC:A6:32:00:10:00", "interface": "bridge-lan", "status": "reachable"},
			{"address": "192.168.88.10", "mac-address": "DC:A6:32:00:20:01", "interface": "bridge-lan", "status": "reachable"},
			{"address": "10.10.10.11", "mac-address": "BC:32:5F:00:00:11", "interface": "vlan10-cam", "status": "stale"}}, nil
	case "/ip/route":
		return []Row{{"dst-address": "0.0.0.0/0", "gateway": "pppoe-vnpt", "immediate-gw": "pppoe-vnpt", "distance": "1", "active": "true"},
			{"dst-address": "0.0.0.0/0", "gateway": "192.168.1.1", "immediate-gw": "192.168.1.1%ether1", "distance": "2", "active": "false"}}, nil
	case "/interface/pppoe-client":
		return []Row{{"name": "pppoe-vnpt", "interface": "ether2", "running": "true", "disabled": "false", "add-default-route": "true"}}, nil
	case "/ip/dhcp-client":
		return []Row{{"interface": "ether1", "status": "bound", "address": "192.168.1.50/24", "gateway": "192.168.1.1", "add-default-route": "yes"}}, nil
	case "/ppp/active", "/system/health":
		return nil, errNotSupported
	case "/interface/bridge/host":
		return []Row{{"mac-address": "DC:A6:32:00:10:00", "on-interface": "ether3", "bridge": "bridge-lan"},
			{"mac-address": "DC:A6:32:00:20:01", "on-interface": "ether4", "bridge": "bridge-lan"}}, nil
	case "/ip/dns/static":
		return []Row{{"name": "nas.vanphong.lan", "address": "192.168.88.10"}}, nil
	case "/interface/list/member":
		return []Row{{"list": "WAN", "interface": "ether1"}, {"list": "WAN", "interface": "pppoe-vnpt"}}, nil
	case "/ip/neighbor":
		return []Row{{"interface": "bridge-lan", "address": "192.168.88.2", "identity": "SW-CoreTang1", "platform": "MikroTik", "board": "CRS326-24G-2S+"}}, nil
	case "/system/identity":
		return []Row{{"name": "Demo-VanPhong"}}, nil
	}
	return nil, errNotSupported
}

func (d *demoSource) Ping(ctx context.Context, addr, iface string) (PingResult, error) {
	select {
	case <-ctx.Done():
		return PingResult{}, ctx.Err()
	case <-time.After(time.Duration(50+rand.IntN(200)) * time.Millisecond):
	}
	// .250 is always dead; one address flaps every ~10 minutes to show alerts.
	if strings.HasSuffix(addr, ".250") || (strings.HasSuffix(addr, ".13") && time.Now().Minute()%10 < 3) {
		return PingResult{Sent: 2, Received: 0, RTT: -1}, nil
	}
	return PingResult{Sent: 2, Received: 2, RTT: 0.4 + rand.Float64()*8}, nil
}

// seedDemo creates a demo router with a small topology when none exists.
func seedDemo(st *Store) error {
	rs, err := st.ListRouters()
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.Mode == "demo" {
			return nil
		}
	}
	r := &Router{Name: "Văn phòng (demo)", Mode: "demo", Interval: 10, Enabled: true, WANTarget: "8.8.8.8",
		WANs: []WAN{{Iface: "pppoe-vnpt", Label: "VNPT"}, {Iface: "ether1", Label: "Viettel"}}}
	if err := st.SaveRouter(r); err != nil {
		return err
	}
	add := func(name, ip, kind string, parent *int64) *int64 {
		m := &Monitor{RouterID: r.ID, Name: name, IP: ip, Kind: kind, ParentID: parent, Enabled: true, Notify: true}
		if err == nil {
			err = st.SaveMonitor(m)
		}
		return &m.ID
	}
	sw := add("Switch tầng 1", "192.168.88.2", "switch", nil)
	add("NAS Synology", "192.168.88.10", "server", sw)
	ap := add("AP tầng 2", "192.168.88.3", "ap", sw)
	add("Máy in HP", "192.168.88.13", "device", ap)
	cam := add("Switch camera", "", "virtual", sw)
	add("Camera cổng", "10.10.10.11", "camera", cam)
	add("Camera kho", "10.10.10.250", "camera", cam)
	return err
}
