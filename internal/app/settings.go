package app

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Settings are global, admin-editable options stored in the kv table.
type Settings struct {
	TGToken       string `json:"tgToken"` // sealed with Box
	AdminChat     string `json:"adminChat"`
	ZaloToken     string `json:"zaloToken"` // sealed with Box
	ZaloAdmin     string `json:"zaloAdmin"`
	NotifyNew     bool   `json:"notifyNew"` // alert when an unknown device joins
	SummaryOn     bool   `json:"summaryOn"`
	SummaryAt     string `json:"summaryAt"`
	RepeatMin     int    `json:"repeatMin"` // reminders for WAN / router outages
	Timezone      string `json:"timezone"`
	RetentionDays int    `json:"retentionDays"`
	PushBase      string `json:"pushBase"`
	AIKey         string `json:"aiKey"` // Anthropic API key, sealed with Box
	AIModel       string `json:"aiModel"`
	AIConsent     bool   `json:"aiConsent"`   // admin agreed to send (redacted) router config to Anthropic
	AIAnonymize   bool   `json:"aiAnonymize"` // replace public IPs/MACs with placeholders
	AutoAudit     bool   `json:"autoAudit"`   // weekly audit on Sunday morning
}

func defaultSettings() Settings {
	return Settings{SummaryOn: true, SummaryAt: "08:00", RepeatMin: 30, Timezone: "Asia/Ho_Chi_Minh", RetentionDays: 30,
		AIModel: "claude-opus-5-5", AIAnonymize: true}
}

type SettingsStore struct {
	st  *Store
	box *Box
	mu  sync.RWMutex
	cur Settings
	loc *time.Location
}

func NewSettingsStore(st *Store, box *Box) (*SettingsStore, error) {
	s := &SettingsStore{st: st, box: box, cur: defaultSettings()}
	if err := st.GetJSON("settings", &s.cur); err != nil {
		return nil, err
	}
	s.loc = loadLoc(s.cur.Timezone)
	return s, nil
}

func loadLoc(name string) *time.Location {
	if l, err := time.LoadLocation(name); err == nil {
		return l
	}
	return time.FixedZone("UTC+7", 7*3600)
}

func (s *SettingsStore) Get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

func (s *SettingsStore) Loc() *time.Location {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loc
}

// Token returns the decrypted bot token of a channel ("" when unset).
func (s *SettingsStore) Token(channel string) string {
	cur := s.Get()
	sealed := cur.TGToken
	if channel == "zalo" {
		sealed = cur.ZaloToken
	}
	t, err := s.box.Open(sealed)
	if err != nil {
		return ""
	}
	return t
}

// AIKey returns the decrypted Anthropic API key ("" when unset).
func (s *SettingsStore) AIKey() string {
	k, err := s.box.Open(s.Get().AIKey)
	if err != nil {
		return ""
	}
	return k
}

// AdminChat returns the admin chat of a channel.
func (s *SettingsStore) AdminChat(channel string) string {
	if channel == "zalo" {
		return s.Get().ZaloAdmin
	}
	return s.Get().AdminChat
}

var (
	reChat  = regexp.MustCompile(`^(-?\d{1,20}|@[A-Za-z0-9_]{4,64})$`)
	reToken = regexp.MustCompile(`^\d{5,15}:[A-Za-z0-9_-]{30,64}$`)
	// Zalo bot tokens look like "12345689:abc-xyz"; chat IDs are opaque strings.
	reZaloToken = regexp.MustCompile(`^\d{3,20}:[A-Za-z0-9_-]{8,200}$`)
	reZaloChat  = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
	reHHMM      = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)
)

// SettingsInput is what the admin form sends. Token: nil = keep, "" = remove.
type SettingsInput struct {
	TGToken       *string `json:"tgToken"`
	AdminChat     string  `json:"adminChat"`
	ZaloToken     *string `json:"zaloToken"`
	ZaloAdmin     string  `json:"zaloAdmin"`
	NotifyNew     bool    `json:"notifyNew"`
	SummaryOn     bool    `json:"summaryOn"`
	SummaryAt     string  `json:"summaryAt"`
	RepeatMin     int     `json:"repeatMin"`
	Timezone      string  `json:"timezone"`
	RetentionDays int     `json:"retentionDays"`
	PushBase      string  `json:"pushBase"`
	AIKey         *string `json:"aiKey"`
	AIModel       string  `json:"aiModel"`
	AIConsent     bool    `json:"aiConsent"`
	AIAnonymize   bool    `json:"aiAnonymize"`
	AutoAudit     bool    `json:"autoAudit"`
}

func (s *SettingsStore) Update(in SettingsInput) error {
	in.AdminChat = strings.TrimSpace(in.AdminChat)
	in.ZaloAdmin = strings.TrimSpace(in.ZaloAdmin)
	if in.ZaloAdmin != "" && !reZaloChat.MatchString(in.ZaloAdmin) {
		return errors.New("Chat ID Zalo không hợp lệ")
	}
	in.PushBase = strings.TrimRight(strings.TrimSpace(in.PushBase), "/")
	if in.AdminChat != "" && !reChat.MatchString(in.AdminChat) {
		return errors.New("Chat ID không hợp lệ")
	}
	if !reHHMM.MatchString(in.SummaryAt) {
		return errors.New("giờ gửi tổng hợp phải dạng HH:MM")
	}
	if !oneOf(in.RepeatMin, 0, 15, 30, 60, 120) {
		return errors.New("chu kỳ nhắc lại không hợp lệ")
	}
	if in.RetentionDays < 1 || in.RetentionDays > 400 {
		return errors.New("số ngày lưu lịch sử phải từ 1 đến 400")
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil || in.Timezone == "" {
		return errors.New("múi giờ không hợp lệ (ví dụ Asia/Ho_Chi_Minh)")
	}
	if in.PushBase != "" {
		u, err := url.Parse(in.PushBase)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
			return errors.New("địa chỉ cho router phải dạng http(s)://IP:PORT")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cur
	if in.TGToken != nil {
		t := strings.TrimSpace(*in.TGToken)
		if t != "" && !reToken.MatchString(t) {
			return errors.New("Bot Token không đúng định dạng (123456:ABC...)")
		}
		next.TGToken = s.box.Seal(t)
	}
	if in.ZaloToken != nil {
		t := strings.TrimSpace(*in.ZaloToken)
		if t != "" && !reZaloToken.MatchString(t) {
			return errors.New("Bot Token Zalo không đúng định dạng (12345:abc...)")
		}
		next.ZaloToken = s.box.Seal(t)
	}
	next.ZaloAdmin, next.NotifyNew = in.ZaloAdmin, in.NotifyNew
	if in.AIKey != nil {
		k := strings.TrimSpace(*in.AIKey)
		if k != "" && (len(k) < 20 || len(k) > 300 || strings.ContainsAny(k, " \t\r\n")) {
			return errors.New("API key Anthropic không đúng định dạng")
		}
		next.AIKey = s.box.Seal(k)
	}
	if in.AIModel == "" {
		in.AIModel = "claude-opus-5-5"
	}
	if _, ok := aiModels[in.AIModel]; !ok {
		return errors.New("mô hình AI không hợp lệ")
	}
	next.AIModel, next.AIConsent, next.AIAnonymize, next.AutoAudit = in.AIModel, in.AIConsent, in.AIAnonymize, in.AutoAudit
	next.AdminChat, next.SummaryOn, next.SummaryAt = in.AdminChat, in.SummaryOn, in.SummaryAt
	next.RepeatMin, next.Timezone, next.RetentionDays, next.PushBase = in.RepeatMin, in.Timezone, in.RetentionDays, in.PushBase
	if err := s.st.SetJSON("settings", next); err != nil {
		return err
	}
	s.cur = next
	s.loc = loadLoc(next.Timezone)
	return nil
}

func oneOf(v int, opts ...int) bool {
	for _, o := range opts {
		if v == o {
			return true
		}
	}
	return false
}
