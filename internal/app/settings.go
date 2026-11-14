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
	TGToken       string       `json:"tgToken"` // sealed with Box
	AdminChat     string       `json:"adminChat"`
	ZaloToken     string       `json:"zaloToken"` // sealed with Box
	ZaloAdmin     string       `json:"zaloAdmin"`
	NotifyNew     bool         `json:"notifyNew"` // alert when an unknown device joins
	SummaryOn     bool         `json:"summaryOn"`
	SummaryAt     string       `json:"summaryAt"`
	RepeatMin     int          `json:"repeatMin"` // reminders for WAN / router outages
	Timezone      string       `json:"timezone"`
	RetentionDays int          `json:"retentionDays"`
	PushBase      string       `json:"pushBase"`
	AIKey         string       `json:"aiKey,omitempty"`   // legacy (v0.3): migrated into AIProviders
	AIModel       string       `json:"aiModel,omitempty"` // legacy (v0.3)
	AIProviders   []AIProvider `json:"aiProviders"`
	AIDefault     string       `json:"aiDefault"`
	AIFallback    bool         `json:"aiFallback"`  // try other accounts when the chosen one fails
	AIConsent     bool         `json:"aiConsent"`   // admin agreed to send (redacted) router config to Anthropic
	AIAnonymize   bool         `json:"aiAnonymize"` // replace public IPs/MACs with placeholders
	AutoAudit     bool         `json:"autoAudit"`   // weekly audit on Sunday morning
}

func defaultSettings() Settings {
	return Settings{SummaryOn: true, SummaryAt: "08:00", RepeatMin: 30, Timezone: "Asia/Ho_Chi_Minh", RetentionDays: 30,
		AIAnonymize: true, AIFallback: true}
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
	if s.cur.AIKey != "" && len(s.cur.AIProviders) == 0 { // v0.3 → multiple accounts
		s.cur.AIProviders = []AIProvider{{ID: "claude", Name: "Claude (Anthropic)", Kind: "anthropic",
			Model: firstNonEmpty(s.cur.AIModel, "claude-opus-5-5"), Key: s.cur.AIKey, Enabled: true}}
		s.cur.AIDefault, s.cur.AIKey, s.cur.AIModel = "claude", "", ""
		if err := st.SetJSON("settings", s.cur); err != nil {
			return nil, err
		}
	}
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

// ---------- AI accounts ----------

// Providers returns the configured AI accounts without their keys.
func (s *SettingsStore) Providers() []AIProvider {
	cur := s.Get()
	out := make([]AIProvider, len(cur.AIProviders))
	for i, p := range cur.AIProviders {
		p.Key = ""
		out[i] = p
	}
	return out
}

func (s *SettingsStore) provider(id string) (AIProvider, bool) {
	for _, p := range s.Get().AIProviders {
		if p.ID == id {
			return p, true
		}
	}
	return AIProvider{}, false
}

func (s *SettingsStore) ProviderKey(id string) string {
	p, ok := s.provider(id)
	if !ok {
		return ""
	}
	k, err := s.box.Open(p.Key)
	if err != nil {
		return ""
	}
	return k
}

// usable: enabled, and has a key unless it is a keyless local server.
func (s *SettingsStore) usable(p AIProvider) bool {
	return p.Enabled && p.Model != "" && (s.ProviderKey(p.ID) != "" || (p.Kind == "openai" && p.Key == ""))
}

// ProviderOrder returns the accounts to try: preferred (or default) first,
// then the other usable accounts if fallback is enabled.
func (s *SettingsStore) ProviderOrder(preferred string) []AIProvider {
	cur := s.Get()
	first := firstNonEmpty(preferred, cur.AIDefault)
	var out []AIProvider
	if p, ok := s.provider(first); ok && s.usable(p) {
		out = append(out, p)
	}
	if len(out) == 0 || cur.AIFallback {
		for _, p := range cur.AIProviders {
			if p.ID != first && s.usable(p) {
				out = append(out, p)
				if !cur.AIFallback {
					break
				}
			}
		}
	}
	return out
}

// AIProviderInput is what the account form sends. Key: nil = keep, "" = none.
type AIProviderInput struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Kind     string  `json:"kind"`
	BaseURL  string  `json:"baseUrl"`
	Model    string  `json:"model"`
	Key      *string `json:"key"`
	PriceIn  float64 `json:"priceIn"`
	PriceOut float64 `json:"priceOut"`
	Enabled  bool    `json:"enabled"`
}

var reModel = regexp.MustCompile(`^[A-Za-z0-9._:/@+-]{1,120}$`)

// resolveProvider validates input and fills the stored key; it does not save.
func (s *SettingsStore) resolveProvider(in AIProviderInput) (AIProvider, error) {
	p := AIProvider{ID: in.ID, Name: strings.TrimSpace(in.Name), Kind: in.Kind, BaseURL: strings.TrimRight(strings.TrimSpace(in.BaseURL), "/"),
		Model: strings.TrimSpace(in.Model), PriceIn: in.PriceIn, PriceOut: in.PriceOut, Enabled: in.Enabled}
	if n := len([]rune(p.Name)); n < 1 || n > 40 {
		return p, errors.New("tên tài khoản 1–40 ký tự")
	}
	switch p.Kind {
	case "anthropic":
		p.BaseURL = ""
		if p.Model == "" {
			p.Model = "claude-opus-5-5"
		}
	case "openai":
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" {
			return p, errors.New("địa chỉ API phải dạng https://…/v1")
		}
	default:
		return p, errors.New("loại tài khoản không hợp lệ")
	}
	if p.Model != "" && !reModel.MatchString(p.Model) {
		return p, errors.New("tên mô hình không hợp lệ")
	}
	if p.PriceIn < 0 || p.PriceOut < 0 || p.PriceIn > 1000 || p.PriceOut > 1000 {
		return p, errors.New("giá không hợp lệ")
	}
	if old, ok := s.provider(p.ID); ok {
		p.Key = old.Key
	}
	if in.Key != nil {
		k := strings.TrimSpace(*in.Key)
		if len(k) > 400 || strings.ContainsAny(k, " \t\r\n") {
			return p, errors.New("API key không đúng định dạng")
		}
		p.Key = s.box.Seal(k)
	}
	if p.Kind == "anthropic" && p.Key == "" {
		return p, errors.New("tài khoản Claude cần API key")
	}
	return p, nil
}

// SaveProvider creates or updates an account and returns it (without key).
func (s *SettingsStore) SaveProvider(in AIProviderInput) (AIProvider, error) {
	p, err := s.resolveProvider(in)
	if err != nil {
		return p, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cur
	next.AIProviders = append([]AIProvider{}, s.cur.AIProviders...)
	found := false
	for i := range next.AIProviders {
		if next.AIProviders[i].ID == p.ID && p.ID != "" {
			next.AIProviders[i], found = p, true
		}
	}
	if !found {
		if len(next.AIProviders) >= 12 {
			return p, errors.New("tối đa 12 tài khoản AI")
		}
		p.ID = randomToken(4)
		next.AIProviders = append(next.AIProviders, p)
	}
	if next.AIDefault == "" || len(next.AIProviders) == 1 {
		next.AIDefault = p.ID
	}
	if err := s.st.SetJSON("settings", next); err != nil {
		return p, err
	}
	s.cur = next
	p.Key = ""
	return p, nil
}

func (s *SettingsStore) DeleteProvider(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cur
	next.AIProviders = nil
	for _, p := range s.cur.AIProviders {
		if p.ID != id {
			next.AIProviders = append(next.AIProviders, p)
		}
	}
	if next.AIDefault == id {
		next.AIDefault = ""
		if len(next.AIProviders) > 0 {
			next.AIDefault = next.AIProviders[0].ID
		}
	}
	if err := s.st.SetJSON("settings", next); err != nil {
		return err
	}
	s.cur = next
	return nil
}

func (s *SettingsStore) SetDefaultProvider(id string) error {
	if _, ok := s.provider(id); !ok {
		return ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cur
	next.AIDefault = id
	if err := s.st.SetJSON("settings", next); err != nil {
		return err
	}
	s.cur = next
	return nil
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
	AIFallback    bool    `json:"aiFallback"`
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
	next.AIFallback, next.AIConsent, next.AIAnonymize, next.AutoAudit = in.AIFallback, in.AIConsent, in.AIAnonymize, in.AutoAudit
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
