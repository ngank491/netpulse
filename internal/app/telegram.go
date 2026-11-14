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

// Notifier delivers Telegram messages through a persistent outbox: an alert
// is written to SQLite first, so restarts and Internet outages never lose it.
type Notifier struct {
	st       *Store
	settings *SettingsStore
	hub      *Hub
	api      string
	hc       *http.Client
	wake     chan struct{}

	mu        sync.Mutex
	lastError string
}

func NewNotifier(st *Store, settings *SettingsStore) *Notifier {
	return &Notifier{st: st, settings: settings, api: "https://api.telegram.org",
		hc: &http.Client{Timeout: 70 * time.Second}, wake: make(chan struct{}, 1)}
}

// Alert sends to the admin chat and, if set, the router's own group chat.
func (n *Notifier) Alert(routerChat, text string) {
	admin := n.settings.Get().AdminChat
	if admin != "" {
		n.Send(admin, text)
	}
	if routerChat != "" && routerChat != admin {
		n.Send(routerChat, text)
	}
}

func (n *Notifier) Send(chat, text string) {
	if chat == "" || text == "" {
		return
	}
	if err := n.st.Enqueue(chat, text); err != nil {
		log.Printf("telegram: không ghi được hàng đợi: %v", err)
		return
	}
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

func (n *Notifier) LastError() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.lastError
}

func (n *Notifier) setErr(e string) {
	n.mu.Lock()
	n.lastError = e
	n.mu.Unlock()
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
			// small delay so a burst of alerts becomes one message
			select {
			case <-time.After(1500 * time.Millisecond):
			case <-ctx.Done():
				return
			}
		}
	}
}

type tgError struct {
	permanent  bool
	retryAfter time.Duration
	msg        string
}

func (e *tgError) Error() string { return e.msg }

func (n *Notifier) flush(ctx context.Context) {
	now := nowMs()
	if dropped, err := n.st.DropOutboxBefore(now - 24*3600*1000); err == nil && dropped > 0 {
		log.Printf("telegram: bỏ %d tin quá 24 giờ chưa gửi được", dropped)
	}
	token := n.settings.Token()
	if token == "" {
		return
	}
	msgs, err := n.st.DueOutbox(now, 100)
	if err != nil || len(msgs) == 0 {
		return
	}
	// Group by chat, keep order, and pack into messages under Telegram's limit.
	order := []string{}
	byChat := map[string][]OutMsg{}
	for _, m := range msgs {
		if _, ok := byChat[m.Chat]; !ok {
			order = append(order, m.Chat)
		}
		byChat[m.Chat] = append(byChat[m.Chat], m)
	}
	for _, chat := range order {
		list := byChat[chat]
		for len(list) > 0 {
			var ids []int64
			var text strings.Builder
			attempts := 0
			for len(list) > 0 {
				m := list[0]
				if text.Len() > 0 && text.Len()+len(m.Text)+2 > 3800 {
					break
				}
				if text.Len() > 0 {
					text.WriteString("\n\n")
				}
				t := m.Text
				if len(t) > 3800 {
					t = t[:3800] + "…"
				}
				text.WriteString(t)
				ids = append(ids, m.ID)
				attempts = max(attempts, m.Attempts)
				list = list[1:]
			}
			err := n.sendMessage(ctx, token, chat, text.String())
			var te *tgError
			switch {
			case err == nil:
				n.setErr("")
				n.st.DeleteOutbox(ids)
			case errors.As(err, &te) && te.permanent:
				n.setErr(err.Error())
				log.Printf("telegram: bỏ tin tới %s: %v", chat, err)
				n.st.DeleteOutbox(ids)
			default:
				n.setErr(err.Error())
				wait := time.Duration(min(30*(1<<min(attempts, 5)), 600)) * time.Second
				if errors.As(err, &te) && te.retryAfter > wait {
					wait = te.retryAfter
				}
				n.st.RetryOutbox(ids, nowMs()+wait.Milliseconds())
				if ctx.Err() == nil {
					return // network likely down; stop this round
				}
			}
		}
	}
}

func (n *Notifier) call(ctx context.Context, token, method string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.api+"/bot"+token+"/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.hc.Do(req)
	if err != nil {
		// never leak the token through URL-bearing errors
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return &tgError{msg: "không kết nối được Telegram: " + err.Error()}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var r struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	json.Unmarshal(data, &r)
	if !r.OK {
		e := &tgError{msg: fmt.Sprintf("Telegram %d: %s", resp.StatusCode, r.Description)}
		switch resp.StatusCode {
		case 400, 403, 404:
			e.permanent = true
		case 401:
			e.msg = "Bot Token sai hoặc đã bị thu hồi"
		case 429:
			e.retryAfter = time.Duration(r.Parameters.RetryAfter) * time.Second
		}
		return e
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func (n *Notifier) sendMessage(ctx context.Context, token, chat, text string) error {
	return n.call(ctx, token, "sendMessage", map[string]any{"chat_id": chat, "text": text, "disable_web_page_preview": true}, nil)
}

// Test sends immediately (bypassing the outbox) and returns the real error.
func (n *Notifier) Test(ctx context.Context, chat string) error {
	token := n.settings.Token()
	if token == "" {
		return errors.New("chưa lưu Bot Token")
	}
	if chat == "" {
		return errors.New("chưa nhập Chat ID")
	}
	return n.sendMessage(ctx, token, chat, "✅ NetPulse đã kết nối Telegram thành công.")
}

// RunBot answers /start (shows the chat ID) and /status for known chats.
func (n *Notifier) RunBot(ctx context.Context) {
	offset := int64(0)
	lastToken := ""
	for ctx.Err() == nil {
		token := n.settings.Token()
		if token == "" {
			sleepCtx(ctx, 30*time.Second)
			continue
		}
		if token != lastToken {
			offset, lastToken = 0, token
		}
		var updates []struct {
			UpdateID int64 `json:"update_id"`
			Message  *struct {
				Text string `json:"text"`
				Chat struct {
					ID   int64  `json:"id"`
					Type string `json:"type"`
				} `json:"chat"`
			} `json:"message"`
		}
		err := n.call(ctx, token, "getUpdates", map[string]any{"offset": offset, "timeout": 50, "allowed_updates": []string{"message"}}, &updates)
		if err != nil {
			sleepCtx(ctx, 60*time.Second)
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			if u.Message == nil {
				continue
			}
			chat := strconv.FormatInt(u.Message.Chat.ID, 10)
			cmd := strings.ToLower(strings.SplitN(strings.Fields(u.Message.Text + " x")[0], "@", 2)[0])
			switch cmd {
			case "/start", "/id":
				n.Send(chat, "Chat ID của cuộc trò chuyện này: "+chat+"\nNhập Chat ID này vào NetPulse để nhận cảnh báo.")
			case "/status":
				if filter, ok := n.chatScope(chat); ok && n.hub != nil {
					n.Send(chat, n.hub.SummaryText(filter))
				}
			}
		}
	}
}

// chatScope decides which routers a chat may see: admin chat → all.
func (n *Notifier) chatScope(chat string) (func(int64) bool, bool) {
	if chat == n.settings.Get().AdminChat {
		return nil, true
	}
	allowed := map[int64]bool{}
	for _, rs := range n.hub.all() {
		rs.mu.RLock()
		if rs.cfg.ChatID == chat {
			allowed[rs.id] = true
		}
		rs.mu.RUnlock()
	}
	if len(allowed) == 0 {
		return nil, false
	}
	return func(id int64) bool { return allowed[id] }, true
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
