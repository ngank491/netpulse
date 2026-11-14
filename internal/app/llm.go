package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// AIProvider is one configured AI account. Kind "anthropic" uses the official
// Anthropic SDK; kind "openai" speaks the OpenAI-compatible Chat Completions
// protocol (OpenAI, Gemini, DeepSeek, Grok, Mistral, Groq, OpenRouter, Qwen,
// Ollama, LM Studio, ...).
type AIProvider struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Kind     string  `json:"kind"`
	BaseURL  string  `json:"baseUrl"`
	Model    string  `json:"model"`
	Key      string  `json:"key,omitempty"` // sealed
	PriceIn  float64 `json:"priceIn"`       // optional USD per 1M input tokens
	PriceOut float64 `json:"priceOut"`      // optional USD per 1M output tokens
	Enabled  bool    `json:"enabled"`
}

// ---------- provider-neutral agent session ----------

type llmTool struct {
	Name   string
	Desc   string
	Schema map[string]any // JSON schema (object, additionalProperties false, all required)
}

type llmCall struct{ ID, Name, Args string }

type llmResult struct {
	ID, Content string
	IsError     bool
}

type llmTurn struct {
	Text                           string
	Calls                          []llmCall
	Stop                           string // end | tool | length | refusal
	In, Out, CacheRead, CacheWrite int64
	Model                          string
}

type llmSession interface {
	// Next sends the results of the previous turn's calls (plus an optional
	// nudge for the model) and returns the model's next turn.
	Next(ctx context.Context, results []llmResult, nudge string) (*llmTurn, error)
}

func newSession(p AIProvider, key, system, first string, tools []llmTool, anthropicBase string) (llmSession, error) {
	switch p.Kind {
	case "anthropic":
		return newClaudeSession(p, key, system, first, tools, anthropicBase), nil
	case "openai":
		return newOpenAISession(p, key, system, first, tools), nil
	}
	return nil, fmt.Errorf("loại tài khoản %q không hỗ trợ", p.Kind)
}

// ---------- Claude (official Anthropic SDK) ----------

type claudeSession struct {
	client anthropic.Client
	params anthropic.BetaMessageNewParams
	sent   bool
}

// Models that accept server-side refusal fallbacks ("default" routing).
var claudeFallbackModels = map[string]bool{"claude-fable-5-2": true, "claude-fable-5-1": true, "claude-opus-5-5": true, "claude-opus-5": true, "claude-sonnet-5-5": true}

var reClaudeEffort = regexp.MustCompile(`^claude-(opus|sonnet|fable|mythos|haiku-5)`)

func newClaudeSession(p AIProvider, key, system, first string, tools []llmTool, base string) *claudeSession {
	opts := []option.RequestOption{option.WithAPIKey(key), option.WithRequestTimeout(5 * time.Minute), option.WithMaxRetries(3)}
	if base != "" {
		opts = append(opts, option.WithBaseURL(base))
	}
	model := firstNonEmpty(p.Model, "claude-opus-5-5")
	var ts []anthropic.BetaToolUnionParam
	for _, t := range tools {
		tp := &anthropic.BetaToolParam{Name: t.Name, Description: anthropic.String(t.Desc), Strict: anthropic.Bool(true),
			InputSchema: anthropic.BetaToolInputSchemaParam{Properties: t.Schema["properties"], Required: t.Schema["required"].([]string),
				ExtraFields: map[string]any{"additionalProperties": false}}}
		ts = append(ts, anthropic.BetaToolUnionParam{OfTool: tp})
	}
	params := anthropic.BetaMessageNewParams{
		Model:        model,
		MaxTokens:    16000,
		System:       []anthropic.BetaTextBlockParam{{Text: system}},
		Tools:        ts,
		Messages:     []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(first))},
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
	}
	if reClaudeEffort.MatchString(model) {
		params.OutputConfig = anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffortHigh}
	}
	if claudeFallbackModels[model] {
		params.Fallbacks = anthropic.BetaFallbacksParamOfDefault()
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
	}
	return &claudeSession{client: anthropic.NewClient(opts...), params: params}
}

func (s *claudeSession) Next(ctx context.Context, results []llmResult, nudge string) (*llmTurn, error) {
	if s.sent {
		var blocks []anthropic.BetaContentBlockParamUnion
		for _, r := range results {
			blocks = append(blocks, anthropic.NewBetaToolResultBlock(r.ID, r.Content, r.IsError))
		}
		if nudge != "" {
			blocks = append(blocks, anthropic.NewBetaTextBlock(nudge))
		}
		s.params.Messages = append(s.params.Messages, anthropic.NewBetaUserMessage(blocks...))
	}
	s.sent = true
	resp, err := s.client.Beta.Messages.New(ctx, s.params)
	if err != nil {
		return nil, friendlyClaudeError(err)
	}
	s.params.Messages = append(s.params.Messages, resp.ToParam())
	t := &llmTurn{Model: string(resp.Model), In: resp.Usage.InputTokens, Out: resp.Usage.OutputTokens,
		CacheRead: resp.Usage.CacheReadInputTokens, CacheWrite: resp.Usage.CacheCreationInputTokens}
	for _, block := range resp.Content {
		switch b := block.AsAny().(type) {
		case anthropic.BetaTextBlock:
			t.Text = b.Text
		case anthropic.BetaToolUseBlock:
			t.Calls = append(t.Calls, llmCall{ID: b.ID, Name: b.Name, Args: b.JSON.Input.Raw()})
		}
	}
	switch resp.StopReason {
	case anthropic.BetaStopReasonRefusal:
		t.Stop = "refusal"
	case anthropic.BetaStopReasonMaxTokens:
		t.Stop = "length"
	case anthropic.BetaStopReasonToolUse:
		t.Stop = "tool"
	default:
		t.Stop = "end"
	}
	return t, nil
}

func friendlyClaudeError(err error) error {
	var apierr *anthropic.Error
	if errors.As(err, &apierr) {
		return httpStatusError(apierr.StatusCode, "")
	}
	return friendlyTransportError(err)
}

func httpStatusError(code int, detail string) error {
	switch code {
	case 401:
		return errors.New("API key không hợp lệ")
	case 402:
		return errors.New("tài khoản hết credit hoặc chưa thanh toán")
	case 403:
		return errors.New("API key không có quyền dùng mô hình này")
	case 404:
		return errors.New("không tìm thấy mô hình hoặc sai địa chỉ API")
	case 429:
		return errors.New("vượt giới hạn tần suất hoặc hết hạn mức của tài khoản, thử lại sau")
	case 500, 502, 503, 529:
		return errors.New("dịch vụ AI đang lỗi hoặc quá tải, thử lại sau")
	}
	if detail != "" {
		return fmt.Errorf("lỗi %d: %s", code, truncateUTF8(detail, 300))
	}
	return fmt.Errorf("dịch vụ AI trả lỗi %d", code)
}

func friendlyTransportError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err // never echo URLs (they may carry keys)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("quá thời gian chờ dịch vụ AI")
	}
	return fmt.Errorf("không kết nối được dịch vụ AI: %v", err)
}

// ---------- OpenAI-compatible Chat Completions ----------

type openaiSession struct {
	p        AIProvider
	key      string
	hc       *http.Client
	tools    []map[string]any
	messages []map[string]any
	calls    int
}

func newOpenAISession(p AIProvider, key, system, first string, tools []llmTool) *openaiSession {
	strict := strings.Contains(p.BaseURL, "api.openai.com") // other services may reject the field
	var ts []map[string]any
	for _, t := range tools {
		fn := map[string]any{"name": t.Name, "description": t.Desc, "parameters": t.Schema}
		if strict {
			fn["strict"] = true
		}
		ts = append(ts, map[string]any{"type": "function", "function": fn})
	}
	return &openaiSession{p: p, key: key, hc: &http.Client{Timeout: 6 * time.Minute}, tools: ts,
		messages: []map[string]any{{"role": "system", "content": system}, {"role": "user", "content": first}}}
}

func (s *openaiSession) post(ctx context.Context, path string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.p.BaseURL, "/")+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.key != "" {
		req.Header.Set("Authorization", "Bearer "+s.key)
	}
	req.Header.Set("X-Title", "NetPulse") // OpenRouter attribution; ignored elsewhere
	return doJSON(s.hc, req, out)
}

func doJSON(hc *http.Client, req *http.Request, out any) error {
	var resp *http.Response
	var err error
	for attempt := 0; ; attempt++ {
		if attempt > 0 && req.GetBody != nil {
			req.Body, _ = req.GetBody()
		}
		resp, err = hc.Do(req)
		if err != nil {
			return friendlyTransportError(err)
		}
		if (resp.StatusCode == 429 || resp.StatusCode >= 500) && attempt < 2 {
			resp.Body.Close()
			select {
			case <-req.Context().Done():
				return friendlyTransportError(req.Context().Err())
			case <-time.After(time.Duration(2+attempt*6) * time.Second):
			}
			continue
		}
		break
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(data, &e)
		// Gemini answers some errors as a JSON array
		if e.Error.Message == "" {
			var arr []struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(data, &arr) == nil && len(arr) > 0 {
				e.Error.Message = arr[0].Error.Message
			}
		}
		return httpStatusError(resp.StatusCode, e.Error.Message)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return errors.New("dịch vụ AI trả dữ liệu không đúng định dạng OpenAI")
	}
	return nil
}

func (s *openaiSession) Next(ctx context.Context, results []llmResult, nudge string) (*llmTurn, error) {
	for _, r := range results {
		c := r.Content
		if r.IsError {
			c = "ERROR: " + c
		}
		s.messages = append(s.messages, map[string]any{"role": "tool", "tool_call_id": r.ID, "content": c})
	}
	if nudge != "" {
		s.messages = append(s.messages, map[string]any{"role": "user", "content": nudge})
	}
	body := map[string]any{"model": s.p.Model, "messages": s.messages, "tools": s.tools, "tool_choice": "auto"}
	if strings.Contains(s.p.BaseURL, "api.openai.com") {
		body["max_completion_tokens"] = 16000 // newer OpenAI models reject max_tokens
	} else {
		body["max_tokens"] = 8192
	}
	var resp struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string         `json:"finish_reason"`
			Message      map[string]any `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := s.post(ctx, "/chat/completions", body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, errors.New("dịch vụ AI không trả kết quả")
	}
	ch := resp.Choices[0]
	msg := ch.Message
	if msg == nil {
		msg = map[string]any{}
	}
	msg["role"] = "assistant"
	t := &llmTurn{Model: firstNonEmpty(resp.Model, s.p.Model), In: resp.Usage.PromptTokens - resp.Usage.PromptTokensDetails.CachedTokens,
		Out: resp.Usage.CompletionTokens, CacheRead: resp.Usage.PromptTokensDetails.CachedTokens}
	if txt, ok := msg["content"].(string); ok {
		t.Text = txt
	}
	if calls, ok := msg["tool_calls"].([]any); ok {
		for _, c := range calls {
			cm, _ := c.(map[string]any)
			fn, _ := cm["function"].(map[string]any)
			if fn == nil {
				continue
			}
			id, _ := cm["id"].(string)
			if id == "" { // some local servers omit ids; invent one and keep history consistent
				s.calls++
				id = fmt.Sprintf("call_%d", s.calls)
				cm["id"] = id
			}
			if cm["type"] == nil {
				cm["type"] = "function"
			}
			name, _ := fn["name"].(string)
			var args string
			switch a := fn["arguments"].(type) {
			case string:
				args = a
			default: // a few servers send an object
				b, _ := json.Marshal(a)
				args = string(b)
				fn["arguments"] = args
			}
			t.Calls = append(t.Calls, llmCall{ID: id, Name: name, Args: args})
		}
	}
	if msg["content"] == nil {
		msg["content"] = "" // some servers reject null content when the history is replayed
	}
	s.messages = append(s.messages, msg)
	switch {
	case ch.FinishReason == "content_filter":
		t.Stop = "refusal"
	case ch.FinishReason == "length":
		t.Stop = "length"
	case len(t.Calls) > 0:
		t.Stop = "tool"
	default:
		t.Stop = "end"
	}
	return t, nil
}

// ---------- model listing (also verifies the key) ----------

func listModels(ctx context.Context, p AIProvider, key, anthropicBase string) ([]string, error) {
	var out []string
	switch p.Kind {
	case "anthropic":
		opts := []option.RequestOption{option.WithAPIKey(key), option.WithRequestTimeout(30 * time.Second)}
		if anthropicBase != "" {
			opts = append(opts, option.WithBaseURL(anthropicBase))
		}
		client := anthropic.NewClient(opts...)
		pager := client.Models.ListAutoPaging(ctx, anthropic.ModelListParams{})
		for pager.Next() {
			out = append(out, pager.Current().ID)
			if len(out) >= 200 {
				break
			}
		}
		if err := pager.Err(); err != nil {
			return nil, friendlyClaudeError(err)
		}
	case "openai":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.BaseURL, "/")+"/models", nil)
		if err != nil {
			return nil, err
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		var resp struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := doJSON(&http.Client{Timeout: 30 * time.Second}, req, &resp); err != nil {
			return nil, err
		}
		for _, m := range resp.Data {
			out = append(out, strings.TrimPrefix(m.ID, "models/")) // Gemini lists "models/<id>"
		}
		sort.Strings(out)
	default:
		return nil, errors.New("loại tài khoản không hỗ trợ")
	}
	return out, nil
}
