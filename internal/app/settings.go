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
	SummaryOn     bool   `json:"summaryOn"`
	SummaryAt     string `json:"summaryAt"`
	RepeatMin     int    `json:"repeatMin"` // reminders for WAN / router outages
	Timezone      string `json:"timezone"`
	RetentionDays int    `json:"retentionDays"`
	PushBase      string `json:"pushBase"`
}

func defaultSettings() Settings {
	return Settings{SummaryOn: true, SummaryAt: "08:00", RepeatMin: 30, Timezone: "Asia/Ho_Chi_Minh", RetentionDays: 30}
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

// Token returns the decrypted Telegram bot token ("" when unset).
func (s *SettingsStore) Token() string {
	t, err := s.box.Open(s.Get().TGToken)
	if err != nil {
		return ""
	}
	return t
}

var (
	reChat  = regexp.MustCompile(`^(-?\d{1,20}|@[A-Za-z0-9_]{4,64})$`)
	reToken = regexp.MustCompile(`^\d{5,15}:[A-Za-z0-9_-]{30,64}$`)
	reHHMM  = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)
)

// SettingsInput is what the admin form sends. Token: nil = keep, "" = remove.
type SettingsInput struct {
	TGToken       *string `json:"tgToken"`
	AdminChat     string  `json:"adminChat"`
	SummaryOn     bool    `json:"summaryOn"`
	SummaryAt     string  `json:"summaryAt"`
	RepeatMin     int     `json:"repeatMin"`
	Timezone      string  `json:"timezone"`
	RetentionDays int     `json:"retentionDays"`
	PushBase      string  `json:"pushBase"`
}

func (s *SettingsStore) Update(in SettingsInput) error {
	in.AdminChat = strings.TrimSpace(in.AdminChat)
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
