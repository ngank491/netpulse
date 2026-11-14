package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Row is one RouterOS item with every value normalised to a string, so data
// from the REST API (pull) and from :serialize (push) look identical.
type Row map[string]string

type PingResult struct {
	Sent     int
	Received int
	RTT      float64 // milliseconds, -1 when unknown
}

// Source is anything that can read a router: the REST API or the demo simulator.
type Source interface {
	Fetch(ctx context.Context, path string, props []string) ([]Row, error)
	Ping(ctx context.Context, addr, iface string) (PingResult, error)
}

var errNotSupported = errors.New("router không có mục này")

// section describes one read-only table we keep from the router.
type section struct {
	Name  string
	Path  string // REST path (may include a query filter)
	Props []string
}

var ifaceProps = []string{"name", "type", "running", "disabled", "comment", "mac-address", "rx-byte", "tx-byte", "actual-mtu"}

var resourceProps = []string{"uptime", "version", "board-name", "architecture-name", "cpu-load", "cpu-count",
	"free-memory", "total-memory", "free-hdd-space", "total-hdd-space"}

var sections = []section{
	{"address", "/ip/address", []string{"address", "network", "interface", "disabled", "dynamic", "invalid", "comment"}},
	{"lease", "/ip/dhcp-server/lease", []string{"address", "active-address", "mac-address", "active-mac-address", "host-name", "server", "status", "last-seen", "expires-after", "dynamic", "disabled", "comment"}},
	{"arp", "/ip/arp", []string{"address", "mac-address", "interface", "status", "dynamic", "complete", "invalid", "comment"}},
	{"bridge_host", "/interface/bridge/host", []string{"mac-address", "on-interface", "bridge", "local", "external", "dynamic"}},
	{"dns_static", "/ip/dns/static", []string{"name", "address", "type", "disabled"}},
	{"route", "/ip/route?dst-address=0.0.0.0/0", []string{"dst-address", "gateway", "immediate-gw", "distance", "active", "disabled", "dynamic", "routing-table", "comment"}},
	{"pppoe_client", "/interface/pppoe-client", []string{"name", "interface", "running", "disabled", "add-default-route"}},
	{"dhcp_client", "/ip/dhcp-client", []string{"interface", "status", "address", "gateway", "disabled", "add-default-route"}},
	{"ppp_active", "/ppp/active", []string{"name", "service", "caller-id", "address", "uptime"}},
	{"wan_list", "/interface/list/member?list=WAN", []string{"list", "interface", "disabled"}},
	{"neighbor", "/ip/neighbor", []string{"interface", "address", "mac-address", "identity", "platform", "board", "version"}},
	{"health", "/system/health", []string{"name", "value", "type"}},
	{"identity", "/system/identity", []string{"name"}},
}

func sectionByName(name string) *section {
	for i := range sections {
		if sections[i].Name == name {
			return &sections[i]
		}
	}
	return nil
}

const maxSectionRows = 5000

// cleanRows keeps only the allowed fields; protects against oversized pushes
// and ensures secrets (e.g. PPP passwords) can never be stored.
func cleanRows(rows []Row, props []string) []Row {
	if len(rows) > maxSectionRows {
		rows = rows[:maxSectionRows]
	}
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		c := Row{}
		for _, p := range props {
			if v, ok := r[p]; ok {
				if len(v) > 256 {
					v = v[:256]
				}
				c[p] = v
			}
		}
		out = append(out, c)
	}
	return out
}

// ---------- REST API client ----------

type restSource struct {
	base, user, pass string
	hc               *http.Client
}

func newRestSource(r *Router, pass string) *restSource {
	scheme, port := "http", r.Port
	if r.TLS {
		scheme = "https"
		if port == 0 {
			port = 443
		}
	} else if port == 0 {
		port = 80
	}
	tr := &http.Transport{
		// RouterOS usually runs a self-signed certificate; verification is opt-in.
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: !r.VerifyTLS, MinVersion: tls.VersionTLS12},
		DialContext:           (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
	}
	return &restSource{
		base: scheme + "://" + net.JoinHostPort(r.Host, strconv.Itoa(port)) + "/rest",
		user: r.Username, pass: pass,
		hc: &http.Client{Transport: tr, Timeout: 45 * time.Second},
	}
}

func (c *restSource) do(ctx context.Context, method, path string, body any) (any, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.user, c.pass)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, friendlyNetErr(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, friendlyNetErr(err)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
			Detail  string `json:"detail"`
		}
		json.Unmarshal(data, &e)
		detail := strings.TrimSpace(e.Message + ": " + e.Detail)
		switch {
		case resp.StatusCode == 401:
			return nil, errors.New("sai user hoặc mật khẩu router (401)")
		case resp.StatusCode == 403:
			return nil, errors.New("user router thiếu quyền (cần read, test, api, rest-api)")
		case resp.StatusCode == 404 || (resp.StatusCode == 400 && method == http.MethodGet):
			return nil, errNotSupported
		default:
			return nil, fmt.Errorf("router báo lỗi %d %s", resp.StatusCode, strings.Trim(detail, ": "))
		}
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, errors.New("router trả dữ liệu không phải JSON (cổng có đúng là REST API www/www-ssl?)")
	}
	return v, nil
}

func friendlyNetErr(err error) error {
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return err
	case errors.As(err, &ne) && ne.Timeout():
		return errors.New("router không phản hồi (timeout)")
	case strings.Contains(err.Error(), "connection refused"):
		return errors.New("router từ chối kết nối (dịch vụ www/www-ssl chưa bật hoặc sai cổng)")
	case strings.Contains(err.Error(), "no route to host"), strings.Contains(err.Error(), "network is unreachable"):
		return errors.New("không có đường tới router")
	}
	// Drop the long request URL from the message; keep the cause.
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "handshake failure"), strings.Contains(msg, "internal error"):
		return errors.New("router từ chối HTTPS: www-ssl chưa có chứng chỉ dùng được (kiểm tra /certificate đã sign và /ip service www-ssl certificate=...)")
	case strings.Contains(msg, "server gave HTTP response"):
		return errors.New("cổng này là HTTP, không phải HTTPS: bỏ chọn “Dùng HTTPS” hoặc đổi cổng")
	case strings.Contains(msg, "x509:"):
		return fmt.Errorf("chứng chỉ router không hợp lệ (bỏ chọn “Kiểm tra chứng chỉ” nếu dùng chứng chỉ tự ký): %v", err)
	case strings.Contains(msg, "tls:"):
		return fmt.Errorf("lỗi HTTPS: %v", err)
	}
	return err
}

func (c *restSource) Fetch(ctx context.Context, path string, props []string) ([]Row, error) {
	if len(props) > 0 {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		path += sep + ".proplist=" + url.QueryEscape(strings.Join(props, ","))
	}
	v, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	return toRows(v), nil
}

func (c *restSource) Ping(ctx context.Context, addr, iface string) (PingResult, error) {
	body := map[string]string{"address": addr, "count": "2", "interval": "300ms"}
	if iface != "" {
		body["interface"] = iface
	}
	v, err := c.do(ctx, http.MethodPost, "/ping", body)
	if err != nil {
		return PingResult{}, err
	}
	return parsePing(toRows(v), 2), nil
}

func toRows(v any) []Row {
	switch t := v.(type) {
	case []any:
		out := make([]Row, 0, len(t))
		for _, x := range t {
			if m, ok := x.(map[string]any); ok {
				out = append(out, toRow(m))
			}
		}
		return out
	case map[string]any:
		return []Row{toRow(t)}
	}
	return nil
}

func toRow(m map[string]any) Row {
	r := make(Row, len(m))
	for k, v := range m {
		r[k] = scalar(v)
	}
	return r
}

func scalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// parsePing understands the REST /ping output: one row per packet plus
// running sent/received counters.
func parsePing(rows []Row, count int) PingResult {
	res := PingResult{Sent: count, RTT: -1}
	replies, summary := 0, false
	var rttSum float64
	for _, r := range rows {
		if v, ok := r["received"]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				res.Received, summary = n, true
			}
			if n, err := strconv.Atoi(r["sent"]); err == nil && n > 0 {
				res.Sent = n
			}
		}
		if t := r["time"]; t != "" && r["status"] == "" {
			replies++
			rttSum += rosSeconds(t) * 1000
		}
		if a := r["avg-rtt"]; a != "" {
			res.RTT = rosSeconds(a) * 1000
		}
	}
	if !summary {
		res.Received = replies
	}
	if res.RTT < 0 && replies > 0 {
		res.RTT = rttSum / float64(replies)
	}
	return res
}

var durPart = regexp.MustCompile(`(\d+(?:\.\d+)?)(ms|us|w|d|h|m|s)`)
var clockPart = regexp.MustCompile(`(\d+):(\d+):(\d+(?:\.\d+)?)`)

// rosSeconds parses RouterOS durations: "1w2d3h4m5s", "2d03:04:05", "10ms", "1ms500us".
func rosSeconds(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	total := 0.0
	if m := clockPart.FindStringSubmatch(s); m != nil {
		h, _ := strconv.ParseFloat(m[1], 64)
		mi, _ := strconv.ParseFloat(m[2], 64)
		se, _ := strconv.ParseFloat(m[3], 64)
		total += h*3600 + mi*60 + se
		s = strings.Replace(s, m[0], "", 1)
	}
	for _, m := range durPart.FindAllStringSubmatch(s, -1) {
		n, _ := strconv.ParseFloat(m[1], 64)
		total += n * map[string]float64{"w": 604800, "d": 86400, "h": 3600, "m": 60, "s": 1, "ms": 0.001, "us": 0.000001}[m[2]]
	}
	return total
}

func truthy(v string) bool { return v == "true" || v == "yes" }

func parseUint(v string) (uint64, bool) {
	n, err := strconv.ParseUint(v, 10, 64)
	return n, err == nil
}

func parseFloat(v string) (float64, bool) {
	f, err := strconv.ParseFloat(v, 64)
	return f, err == nil
}
