package providergw

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// GigaChat endpoints. The OAuth host is on port 9443.
const (
	GigaChatBaseURL  = "https://api.giga.chat/v1"
	GigaChatOAuthURL = "https://ngw.devices.sberbank.ru:9443/api/v2/oauth"
)

// GigaTokens gets and refreshes the GigaChat access token: POST the authorization key to the OAuth endpoint, get a
// bearer token that lives 30 minutes, renew it a minute before it ends. The authorization key comes from the server's
// environment per call and is never stored here.
type GigaTokens struct {
	AuthKey func() string
	Scope   string // GIGACHAT_API_PERS, GIGACHAT_API_B2B or GIGACHAT_API_CORP
	URL     string // "" = GigaChatOAuthURL
	Client  *http.Client
	Now     func() time.Time

	mu  sync.Mutex
	tok string
	exp time.Time
}

func (t *GigaTokens) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Token returns a valid access token; force renews it first (after the API answered 401).
func (t *GigaTokens) Token(ctx context.Context, force bool) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !force && t.tok != "" && t.now().Before(t.exp.Add(-time.Minute)) {
		return t.tok, nil
	}
	key := ""
	if t.AuthKey != nil {
		key = t.AuthKey()
	}
	if key == "" {
		return "", errors.New("the GigaChat authorization key is not set in the server environment")
	}
	u := t.URL
	if u == "" {
		u = GigaChatOAuthURL
	}
	scope := t.Scope
	if scope == "" {
		scope = "GIGACHAT_API_PERS"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(url.Values{"scope": {scope}}.Encode()))
	if err != nil {
		return "", err
	}
	var rq [16]byte
	_, _ = rand.Read(rq[:])
	rq[6], rq[8] = rq[6]&0x0f|0x40, rq[8]&0x3f|0x80
	h := hex.EncodeToString(rq[:])
	req.Header.Set("RqUID", h[0:8]+"-"+h[8:12]+"-"+h[12:16]+"-"+h[16:20]+"-"+h[20:])
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Basic "+key)
	resp, err := t.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GigaChat OAuth: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GigaChat OAuth answered %d", resp.StatusCode) // the body can echo request ids only; not logged
	}
	var out struct {
		Token     string `json:"access_token"`
		ExpiresAt int64  `json:"expires_at"` // epoch milliseconds
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return "", errors.New("GigaChat OAuth: no access_token in the answer")
	}
	t.tok = out.Token
	t.exp = time.UnixMilli(out.ExpiresAt)
	if out.ExpiresAt == 0 {
		t.exp = t.now().Add(29 * time.Minute)
	}
	return t.tok, nil
}

// GigaChat translates OpenAI Chat Completions to the GigaChat API and back: tools <-> functions, tool_calls <->
// function_call (JSON-string arguments <-> object arguments), role tool <-> function, SSE with the whole function call
// in one chunk <-> OpenAI tool_call deltas. GigaChat's /v1 is only partly OpenAI compatible and does not take tools.
type GigaChat struct {
	BaseURL string // "" = GigaChatBaseURL
	Tokens  *GigaTokens
	Client  *http.Client
}

func (g *GigaChat) base() string {
	if g.BaseURL != "" {
		return strings.TrimRight(g.BaseURL, "/")
	}
	return GigaChatBaseURL
}

func (g *GigaChat) Name() string { return "gigachat" }

func (g *GigaChat) Hosts() []string {
	hosts := []string{}
	for _, raw := range []string{g.base(), g.oauth()} {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			hosts = append(hosts, u.Hostname())
		}
	}
	return hosts
}

func (g *GigaChat) oauth() string {
	if g.Tokens != nil && g.Tokens.URL != "" {
		return g.Tokens.URL
	}
	return GigaChatOAuthURL
}

func (g *GigaChat) Ready() error {
	if g.Tokens == nil || g.Tokens.AuthKey == nil || g.Tokens.AuthKey() == "" {
		return errors.New("the GigaChat authorization key is not set in the server environment")
	}
	return nil
}

func (g *GigaChat) Chat(ctx context.Context, body []byte, w http.ResponseWriter) error {
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		return fmt.Errorf("bad request: %w", err)
	}
	out, stream := ToGigaChat(in)
	raw, _ := json.Marshal(out)
	var resp *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := g.Tokens.Token(ctx, attempt == 1)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.base()+"/chat/completions", bytes.NewReader(raw))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		if stream {
			req.Header.Set("Accept", "text/event-stream")
		}
		resp, err = g.Client.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt == 1 {
			break
		}
		resp.Body.Close() // the token expired early: renew it once and retry
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		WriteError(w, resp.StatusCode, "GigaChat: "+oneLine(string(msg), 400))
		return nil
	}
	if !stream {
		var gc map[string]any
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&gc); err != nil {
			return fmt.Errorf("bad answer: %w", err)
		}
		oa, err := FromGigaChat(gc)
		if err != nil {
			return err
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		return json.NewEncoder(w).Encode(oa)
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	return translateStream(resp.Body, w)
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// ---- request: OpenAI -> GigaChat

// ToGigaChat converts an OpenAI chat-completions request; stream tells whether the caller asked for SSE.
func ToGigaChat(in map[string]any) (out map[string]any, stream bool) {
	out = map[string]any{}
	for _, k := range []string{"model", "temperature", "top_p", "stream"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	stream, _ = in["stream"].(bool)
	switch {
	case in["max_tokens"] != nil:
		out["max_tokens"] = in["max_tokens"]
	case in["max_completion_tokens"] != nil:
		out["max_tokens"] = in["max_completion_tokens"]
	}
	if tools, ok := in["tools"].([]any); ok {
		var fns []any
		for _, t := range tools {
			tm, _ := t.(map[string]any)
			if tm == nil {
				continue
			}
			f, _ := tm["function"].(map[string]any)
			if f == nil || f["name"] == nil {
				continue
			}
			fn := map[string]any{"name": f["name"]}
			if d, ok := f["description"]; ok {
				fn["description"] = d
			}
			if p, ok := f["parameters"]; ok {
				fn["parameters"] = p
			} else {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			fns = append(fns, fn)
		}
		if len(fns) > 0 {
			out["functions"] = fns
			out["function_call"] = functionCall(in["tool_choice"])
		}
	}
	msgs, _ := in["messages"].([]any)
	out["messages"] = toGigaMessages(msgs)
	return out, stream
}

// functionCall maps tool_choice: auto and required -> auto, none -> none, a named function -> {"name": ...}.
func functionCall(choice any) any {
	switch c := choice.(type) {
	case string:
		if c == "none" {
			return "none"
		}
	case map[string]any:
		if f, ok := c["function"].(map[string]any); ok && f["name"] != nil {
			return map[string]any{"name": f["name"]}
		}
	}
	return "auto"
}

func textOf(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok {
				if t, ok := pm["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}

func toGigaMessages(msgs []any) []any {
	var out []any
	results := map[string]string{} // tool_call_id -> content
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok && mm["role"] == "tool" {
			id, _ := mm["tool_call_id"].(string)
			results[id] = textOf(mm["content"])
		}
	}
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		role, _ := mm["role"].(string)
		switch role {
		case "system", "developer":
			out = append(out, map[string]any{"role": "system", "content": textOf(mm["content"])})
		case "user":
			out = append(out, map[string]any{"role": "user", "content": textOf(mm["content"])})
		case "assistant":
			calls, _ := mm["tool_calls"].([]any)
			text := textOf(mm["content"])
			if len(calls) == 0 {
				out = append(out, map[string]any{"role": "assistant", "content": text})
				continue
			}
			// GigaChat takes one function call per assistant message: split parallel calls into pairs.
			for i, c := range calls {
				cm, _ := c.(map[string]any)
				f, _ := cm["function"].(map[string]any)
				if f == nil {
					continue
				}
				name, _ := f["name"].(string)
				id, _ := cm["id"].(string)
				var args any = map[string]any{}
				if s, ok := f["arguments"].(string); ok && strings.TrimSpace(s) != "" {
					var parsed any
					if json.Unmarshal([]byte(s), &parsed) == nil {
						args = parsed
					}
				}
				am := map[string]any{"role": "assistant", "content": "", "function_call": map[string]any{"name": name, "arguments": args}}
				if i == 0 {
					am["content"] = text
				}
				if state := stateOfCallID(id); state != "" {
					am["functions_state_id"] = state
				}
				out = append(out, am)
				res, found := results[id]
				if !found {
					res = `{"error":"no result"}`
				}
				out = append(out, map[string]any{"role": "function", "name": name, "content": jsonObject(res)})
			}
		case "tool":
			// answered together with its assistant message
		}
	}
	return out
}

// jsonObject makes GigaChat's required "valid JSON object in a string" out of a tool result.
func jsonObject(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		if _, ok := v.(map[string]any); ok {
			return s
		}
	}
	b, _ := json.Marshal(map[string]any{"result": s})
	return string(b)
}

// A tool call id carries the GigaChat functions_state_id so that it survives the agent's history: gc-<state>-<n>.
func callID(state string, n int) string {
	if state == "" {
		var b [6]byte
		_, _ = rand.Read(b[:])
		state = "x" + hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("gc-%s-%d", state, n)
}

func stateOfCallID(id string) string {
	if !strings.HasPrefix(id, "gc-") {
		return ""
	}
	rest := strings.TrimPrefix(id, "gc-")
	i := strings.LastIndex(rest, "-")
	if i <= 0 {
		return ""
	}
	if s := rest[:i]; !strings.HasPrefix(s, "x") {
		return s
	}
	return ""
}

// ---- answer: GigaChat -> OpenAI

var finishReasons = map[string]string{"stop": "stop", "length": "length", "function_call": "tool_calls", "blacklist": "content_filter"}

func openAIUsage(u map[string]any) map[string]any {
	if u == nil {
		return nil
	}
	return map[string]any{"prompt_tokens": u["prompt_tokens"], "completion_tokens": u["completion_tokens"], "total_tokens": u["total_tokens"]}
}

func argumentsString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// FromGigaChat converts a non-streaming GigaChat answer to an OpenAI chat.completion.
func FromGigaChat(gc map[string]any) (map[string]any, error) {
	choices, _ := gc["choices"].([]any)
	var outChoices []any
	for i, c := range choices {
		cm, _ := c.(map[string]any)
		msg, _ := cm["message"].(map[string]any)
		if msg == nil {
			continue
		}
		reason, _ := cm["finish_reason"].(string)
		if reason == "error" {
			return nil, errors.New("GigaChat produced invalid function arguments (finish_reason error)")
		}
		om := map[string]any{"role": "assistant", "content": msg["content"]}
		if fc, ok := msg["function_call"].(map[string]any); ok && fc["name"] != nil {
			state, _ := msg["functions_state_id"].(string)
			om["tool_calls"] = []any{map[string]any{"id": callID(state, 0), "type": "function",
				"function": map[string]any{"name": fc["name"], "arguments": argumentsString(fc["arguments"])}}}
			if s, _ := om["content"].(string); s == "" {
				om["content"] = nil
			}
		}
		fr := finishReasons[reason]
		if fr == "" {
			fr = "stop"
		}
		outChoices = append(outChoices, map[string]any{"index": i, "message": om, "finish_reason": fr})
	}
	out := map[string]any{"id": "chatcmpl-gc-" + fmt.Sprint(gc["created"]), "object": "chat.completion", "created": gc["created"],
		"model": gc["model"], "choices": outChoices}
	if u := openAIUsage(mapOf(gc["usage"])); u != nil {
		out["usage"] = u
	}
	return out, nil
}

func mapOf(v any) map[string]any { m, _ := v.(map[string]any); return m }

// translateStream converts GigaChat SSE chunks to OpenAI chat.completion.chunk SSE.
func translateStream(r io.Reader, w http.ResponseWriter) error {
	fl, _ := w.(http.Flusher)
	send := func(v any) error {
		b, _ := json.Marshal(v)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	chunk := func(id string, created, model any, delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	id := "chatcmpl-gc-stream"
	started := false
	var usage map[string]any
	var created, model any
	finish := ""
	toolIdx := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var gc map[string]any
		if json.Unmarshal([]byte(payload), &gc) != nil {
			continue
		}
		created, model = gc["created"], gc["model"]
		if u := mapOf(gc["usage"]); u != nil {
			usage = openAIUsage(u)
		}
		choices, _ := gc["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		c0, _ := choices[0].(map[string]any)
		delta := mapOf(c0["delta"])
		if !started {
			started = true
			if err := send(chunk(id, created, model, map[string]any{"role": "assistant", "content": ""}, nil)); err != nil {
				return err
			}
		}
		if t, _ := delta["content"].(string); t != "" {
			if err := send(chunk(id, created, model, map[string]any{"content": t}, nil)); err != nil {
				return err
			}
		}
		if fc := mapOf(delta["function_call"]); fc != nil && fc["name"] != nil {
			state, _ := delta["functions_state_id"].(string)
			d := map[string]any{"tool_calls": []any{map[string]any{"index": toolIdx, "id": callID(state, toolIdx), "type": "function",
				"function": map[string]any{"name": fc["name"], "arguments": argumentsString(fc["arguments"])}}}}
			toolIdx++
			if err := send(chunk(id, created, model, d, nil)); err != nil {
				return err
			}
		}
		if fr, _ := c0["finish_reason"].(string); fr != "" {
			finish = finishReasons[fr]
			if fr == "error" {
				finish = "stop"
			}
			if finish == "" {
				finish = "stop"
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if !started {
		_ = send(chunk(id, created, model, map[string]any{"role": "assistant", "content": ""}, nil))
	}
	if finish == "" {
		finish = "stop"
	}
	if err := send(chunk(id, created, model, map[string]any{}, finish)); err != nil {
		return err
	}
	if usage != nil {
		if err := send(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{}, "usage": usage}); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
	return err
}
