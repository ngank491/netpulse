package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Channels supported for alerts. Both expose a Telegram-style bot API:
// POST <base>/bot<TOKEN>/<method> with JSON, answering {ok, result, description, error_code}.
type channel struct {
	Name   string // "telegram" | "zalo"
	Title  string
	Base   string
	MaxLen int // per-message text limit
}

var channels = []channel{
	{"telegram", "Telegram", "https://api.telegram.org", 3800},
	{"zalo", "Zalo", "https://bot-api.zaloplatforms.com", 1900},
}

func channelByName(name string) *channel {
	for i := range channels {
		if channels[i].Name == name {
			return &channels[i]
		}
	}
	return nil
}

// RecentChat is someone who recently messaged a bot (helps find Chat IDs).
type RecentChat struct {
	Channel string `json:"channel"`
	ChatID  string `json:"chatId"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Text    string `json:"text"`
	At      int64  `json:"at"`
}

// Notifier delivers alerts through a persistent outbox: a message is written
// to SQLite first, so restarts and Internet outages never lose it.
type Notifier struct {
	st       *Store
	settings *SettingsStore
	hub      *Hub
	base     map[string]string // overridable API bases (tests)
	hc       *http.Client
	wake     chan struct{}

	mu       sync.Mutex
	lastErr  map[string]string
	recent   []RecentChat
	seenMsgs map[string]int64
}

func NewNotifier(st *Store, settings *SettingsStore) *Notifier {
	n := &Notifier{st: st, settings: settings, base: map[string]string{}, hc: &http.Client{Timeout: 70 * time.Second},
		wake: make(chan struct{}, 1), lastErr: map[string]string{}, seenMsgs: map[string]int64{}}
	for _, c := range channels {
		n.base[c.Name] = c.Base
	}
	return n
}

// Targets are the per-router extra recipients (group chats).
type Targets struct {
	Telegram string
	Zalo     string
}

func (t Targets) For(channel string) string {
	if channel == "zalo" {
		return t.Zalo
	}
	return t.Telegram
}

// Alert sends to every configured admin chat and the router's own chats.
func (n *Notifier) Alert(t Targets, text string) {
	for _, c := range channels {
		admin := n.settings.AdminChat(c.Name)
		if admin != "" {
			n.Send(c.Name, admin, text)
		}
		if extra := t.For(c.Name); extra != "" && extra != admin {
			n.Send(c.Name, extra, text)
		}
	}
}

// ToAdmins sends to the admin chat of every channel.
func (n *Notifier) ToAdmins(text string) {
	for _, c := range channels {
		if admin := n.settings.AdminChat(c.Name); admin != "" {
			n.Send(c.Name, admin, text)
		}
	}
}

func (n *Notifier) Send(ch, chat, text string) {
	if chat == "" || text == "" || channelByName(ch) == nil {
		return
	}
	if err := n.st.Enqueue(ch, chat, text); err != nil {
		log.Printf("cảnh báo: không ghi được hàng đợi: %v", err)
		return
	}
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

func (n *Notifier) LastErrors() map[string]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := map[string]string{}
	for k, v := range n.lastErr {
		out[k] = v
	}
	return out
}

func (n *Notifier) setErr(ch, e string) {
	n.mu.Lock()
	n.lastErr[ch] = e
	n.mu.Unlock()
}

func (n *Notifier) Recent() []RecentChat {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]RecentChat{}, n.recent...)
}

func (n *Notifier) remember(rc RecentChat) {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := []RecentChat{rc}
	for _, r := range n.recent {
		if !(r.Channel == rc.Channel && r.ChatID == rc.ChatID) && len(out) < 20 {
			out = append(out, r)
		}
	}
	n.recent = out
}

func (n *Notifier) Run(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		n.flush(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-n.wake:
			// short delay so a burst of alerts becomes one message
			select {
			case <-time.After(1500 * time.Millisecond):
			case <-ctx.Done():
				return
			}
		}
	}
}

type botError struct {
	permanent  bool
	retryAfter time.Duration
	msg        string
}

func (e *botError) Error() string { return e.msg }

func (n *Notifier) flush(ctx context.Context) {
	now := nowMs()
	if dropped, err := n.st.DropOutboxBefore(now - 24*3600*1000); err == nil && dropped > 0 {
		log.Printf("cảnh báo: bỏ %d tin quá 24 giờ chưa gửi được", dropped)
	}
	msgs, err := n.st.DueOutbox(now, 200)
	if err != nil || len(msgs) == 0 {
		return
	}
	type key struct{ ch, chat string }
	var order []key
	groups := map[key][]OutMsg{}
	for _, m := range msgs {
		k := key{m.Channel, m.Chat}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], m)
	}
	down := map[string]bool{} // channel considered unreachable this round
	for _, k := range order {
		ch := channelByName(k.ch)
		token := n.settings.Token(k.ch)
		if ch == nil || token == "" || down[k.ch] {
			continue // keep queued (up to 24h) until a token is configured / network returns
		}
		list := groups[k]
		for len(list) > 0 && !down[k.ch] {
			var ids []int64
			var text strings.Builder
			attempts := 0
			for len(list) > 0 {
				m := list[0]
				t := m.Text
				if len(t) > ch.MaxLen {
					t = truncateUTF8(t, ch.MaxLen-3) + "…"
				}
				if text.Len() > 0 && text.Len()+len(t)+2 > ch.MaxLen {
					break
				}
				if text.Len() > 0 {
					text.WriteString("\n\n")
				}
				text.WriteString(t)
				ids = append(ids, m.ID)
				attempts = max(attempts, m.Attempts)
				list = list[1:]
			}
			err := n.sendMessage(ctx, k.ch, token, k.chat, text.String())
			var be *botError
			switch {
			case err == nil:
				n.setErr(k.ch, "")
				n.st.DeleteOutbox(ids)
			case errors.As(err, &be) && be.permanent:
				n.setErr(k.ch, err.Error())
				log.Printf("%s: bỏ tin tới %s: %v", ch.Title, k.chat, err)
				n.st.DeleteOutbox(ids)
			default:
				n.setErr(k.ch, err.Error())
				wait := time.Duration(min(30*(1<<min(attempts, 5)), 600)) * time.Second
				if errors.As(err, &be) && be.retryAfter > wait {
					wait = be.retryAfter
				}
				n.st.RetryOutbox(ids, nowMs()+wait.Milliseconds())
				down[k.ch] = true
			}
		}
	}
}

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

func (n *Notifier) call(ctx context.Context, ch, token, method string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.base[ch]+"/bot"+token+"/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.hc.Do(req)
	if err != nil {
		var ue *url.Error // never leak the token through URL-bearing errors
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return &botError{msg: "không kết nối được " + channelByName(ch).Title + ": " + err.Error()}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var r struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
		Result      json.RawMessage `json:"result"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if json.Unmarshal(data, &r) != nil {
		return &botError{msg: fmt.Sprintf("%s trả lời không hợp lệ (HTTP %d)", channelByName(ch).Title, resp.StatusCode)}
	}
	if !r.OK {
		code := r.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		e := &botError{msg: fmt.Sprintf("%s %d: %s", channelByName(ch).Title, code, r.Description)}
		switch code {
		case 400, 403, 404:
			e.permanent = true // bad chat id / bot blocked: retrying will not help
		case 401:
			e.msg = channelByName(ch).Title + ": Bot Token sai hoặc đã bị thu hồi"
		case 429:
			e.retryAfter = time.Duration(max(r.Parameters.RetryAfter, 60)) * time.Second
		}
		return e
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func (n *Notifier) sendMessage(ctx context.Context, ch, token, chat, text string) error {
	body := map[string]any{"chat_id": chat, "text": text}
	if ch == "telegram" {
		body["disable_web_page_preview"] = true
	}
	return n.call(ctx, ch, token, "sendMessage", body, nil)
}

// Test sends immediately (bypassing the outbox) and returns the real error.
func (n *Notifier) Test(ctx context.Context, ch, chat string) error {
	c := channelByName(ch)
	if c == nil {
		return errors.New("kênh không hợp lệ")
	}
	token := n.settings.Token(ch)
	if token == "" {
		return errors.New("chưa lưu Bot Token " + c.Title)
	}
	if chat == "" {
		return errors.New("chưa nhập Chat ID " + c.Title)
	}
	return n.sendMessage(ctx, ch, token, chat, "✅ NetPulse đã kết nối "+c.Title+" thành công.")
}

// RunBots polls every configured bot for messages (/start, /id, /status).
func (n *Notifier) RunBots(ctx context.Context) {
	var wg sync.WaitGroup
	for _, c := range channels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.runBot(ctx, c.Name)
		}()
	}
	wg.Wait()
}

// update is the common subset of Telegram and Zalo bot updates.
type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID any    `json:"message_id"`
		Text      string `json:"text"`
		Chat      struct {
			ID        any    `json:"id"`
			Type      string `json:"type"`
			ChatType  string `json:"chat_type"`
			Title     string `json:"title"`
			FirstName string `json:"first_name"`
			Username  string `json:"username"`
		} `json:"chat"`
		From struct {
			DisplayName string `json:"display_name"`
			FirstName   string `json:"first_name"`
			LastName    string `json:"last_name"`
			Username    string `json:"username"`
		} `json:"from"`
	} `json:"message"`
}

func (n *Notifier) runBot(ctx context.Context, ch string) {
	offset := int64(0)
	lastToken := ""
	for ctx.Err() == nil {
		token := n.settings.Token(ch)
		if token == "" {
			sleepCtx(ctx, 30*time.Second)
			continue
		}
		if token != lastToken {
			offset, lastToken = 0, token
		}
		body := map[string]any{"timeout": 30}
		if ch == "telegram" {
			body = map[string]any{"offset": offset, "timeout": 50, "allowed_updates": []string{"message"}}
		}
		var raw json.RawMessage
		started := time.Now()
		if err := n.call(ctx, ch, token, "getUpdates", body, &raw); err != nil {
			sleepCtx(ctx, 60*time.Second)
			continue
		}
		// Telegram returns an array; Zalo may return a single update object.
		var ups []update
		if json.Unmarshal(raw, &ups) != nil {
			var one update
			if json.Unmarshal(raw, &one) == nil && one.Message != nil {
				ups = []update{one}
			}
		}
		for _, u := range ups {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.Message != nil {
				n.handleMessage(ch, u)
			}
		}
		if len(ups) == 0 && time.Since(started) < 2*time.Second {
			sleepCtx(ctx, 3*time.Second) // API without long polling: don't hammer it
		}
	}
}

func (n *Notifier) handleMessage(ch string, u update) {
	m := u.Message
	chat := scalar(m.Chat.ID)
	if chat == "" {
		return
	}
	// Zalo has no offset: drop messages we already handled.
	key := ch + "|" + chat + "|" + scalar(m.MessageID) + "|" + strconv.FormatInt(u.UpdateID, 10)
	n.mu.Lock()
	if _, dup := n.seenMsgs[key]; dup {
		n.mu.Unlock()
		return
	}
	now := nowMs()
	n.seenMsgs[key] = now
	if len(n.seenMsgs) > 500 {
		for k, t := range n.seenMsgs {
			if now-t > 3600_000 {
				delete(n.seenMsgs, k)
			}
		}
	}
	n.mu.Unlock()

	name := firstNonEmpty(m.Chat.Title, m.From.DisplayName, strings.TrimSpace(m.From.FirstName+" "+m.From.LastName), m.Chat.FirstName, m.From.Username, m.Chat.Username)
	kind := firstNonEmpty(m.Chat.Type, m.Chat.ChatType)
	text := truncateUTF8(strings.TrimSpace(m.Text), 60)
	n.remember(RecentChat{Channel: ch, ChatID: chat, Name: name, Kind: strings.ToLower(kind), Text: text, At: now})

	cmd := strings.ToLower(strings.SplitN(strings.Fields(m.Text + " x")[0], "@", 2)[0])
	switch cmd {
	case "/start", "/id", "id":
		n.Send(ch, chat, "Chat ID của cuộc trò chuyện này: "+chat+"\nNhập Chat ID này vào NetPulse (Cài đặt → "+channelByName(ch).Title+") để nhận cảnh báo.")
	case "/status", "status":
		if filter, ok := n.chatScope(ch, chat); ok && n.hub != nil {
			n.Send(ch, chat, n.hub.SummaryText(filter))
		}
	}
}

// chatScope decides which routers a chat may see: the channel's admin chat → all.
func (n *Notifier) chatScope(ch, chat string) (func(int64) bool, bool) {
	if chat == n.settings.AdminChat(ch) {
		return nil, true
	}
	allowed := map[int64]bool{}
	for _, rs := range n.hub.all() {
		rs.mu.RLock()
		if (Targets{rs.cfg.ChatID, rs.cfg.ZaloChat}).For(ch) == chat {
			allowed[rs.id] = true
		}
		rs.mu.RUnlock()
	}
	if len(allowed) == 0 {
		return nil, false
	}
	return func(id int64) bool { return allowed[id] }, true
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
