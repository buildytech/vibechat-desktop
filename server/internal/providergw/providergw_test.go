package providergw_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/providergw"
	"github.com/buildytech/vibechat-desktop/server/internal/providergw/stub"
)

const secret = "SECRET-PROVIDER-KEY-0123456789"

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) log(s string) { l.mu.Lock(); l.lines = append(l.lines, s); l.mu.Unlock() }
func (l *logSink) all() string  { l.mu.Lock(); defer l.mu.Unlock(); return strings.Join(l.lines, "\n") }

func client(t *testing.T) *http.Client {
	t.Helper()
	c, err := providergw.NewClient([]string{"127.0.0.1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func post(t *testing.T, base, token string, body map[string]any) (*http.Response, []byte) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, base+"/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func yandexGW(t *testing.T, upstream *stub.Server, keyOK bool) (*providergw.Gateway, string, *logSink) {
	t.Helper()
	ls := &logSink{}
	key := func() string {
		if keyOK {
			return secret
		}
		return ""
	}
	y := &providergw.Yandex{BaseURL: upstream.URL + "/v1", FolderID: "b1gfolder", Key: key, Client: client(t)}
	gw := providergw.New(y, ls.log)
	base, err := gw.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Close)
	return gw, base, ls
}

func TestGatewayNeedsItsOwnToken(t *testing.T) {
	up := stub.OpenAI(func(int, stub.Request) stub.Reply { return stub.Reply{Text: "ok"} })
	defer up.Close()
	gw, base, _ := yandexGW(t, up, true)
	body := map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	if r, _ := post(t, base, "", body); r.StatusCode != 401 {
		t.Fatalf("no token: %d", r.StatusCode)
	}
	if r, _ := post(t, base, "vcgw_guess", body); r.StatusCode != 401 {
		t.Fatalf("wrong token: %d", r.StatusCode)
	}
	tok, revoke := gw.Mint()
	if r, _ := post(t, base, tok, body); r.StatusCode != 200 {
		t.Fatalf("minted token: %d", r.StatusCode)
	}
	revoke()
	if r, _ := post(t, base, tok, body); r.StatusCode != 401 {
		t.Fatalf("revoked token still works: %d", r.StatusCode)
	}
	if len(up.Requests()) != 1 {
		t.Fatalf("refused requests reached the provider: %d", len(up.Requests()))
	}
}

func TestYandexPassThroughInjectsTheKeyAndKeepsTheBody(t *testing.T) {
	up := stub.OpenAI(func(int, stub.Request) stub.Reply {
		return stub.Reply{Text: "hello world", Calls: []stub.Call{{Name: "write", Args: map[string]any{"path": "a.txt"}}}}
	})
	defer up.Close()
	gw, base, ls := yandexGW(t, up, true)
	tok, _ := gw.Mint()
	body := map[string]any{"model": "gpt://b1gfolder/yandexgpt/rc", "stream": true, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "write"}}},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	resp, out := post(t, base, tok, body)
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("content-type"), "event-stream") {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("content-type"))
	}
	s := string(out)
	for _, want := range []string{`"content":"hello "`, `"name":"write"`, `"finish_reason":"tool_calls"`, `"usage"`, "data: [DONE]"} {
		if !strings.Contains(s, want) {
			t.Errorf("stream lacks %s:\n%s", want, s)
		}
	}
	seen := up.Requests()[0]
	if seen.Authorization != "Api-Key "+secret || seen.Headers.Get("OpenAI-Project") != "b1gfolder" || seen.Headers.Get("x-data-logging-enabled") != "false" {
		t.Fatalf("provider headers: %q %q %q", seen.Authorization, seen.Headers.Get("OpenAI-Project"), seen.Headers.Get("x-data-logging-enabled"))
	}
	var sent map[string]any
	_ = json.Unmarshal(seen.Raw, &sent)
	if sent["model"] != "gpt://b1gfolder/yandexgpt/rc" || sent["stream"] != true || sent["tools"] == nil {
		t.Fatalf("body changed: %s", seen.Raw)
	}
	if strings.Contains(ls.all(), secret) || strings.Contains(ls.all(), "Api-Key") {
		t.Fatalf("log shows the credential: %s", ls.all())
	}
	if strings.Contains(string(out), secret) {
		t.Fatal("the answer to the agent carries the key")
	}
}

func TestGatewayFailsWithoutTheKey(t *testing.T) {
	up := stub.OpenAI(func(int, stub.Request) stub.Reply { return stub.Reply{Text: "ok"} })
	defer up.Close()
	gw, base, _ := yandexGW(t, up, false)
	tok, _ := gw.Mint()
	r, out := post(t, base, tok, map[string]any{"model": "m"})
	if r.StatusCode != 503 || !strings.Contains(string(out), "API key is not set") {
		t.Fatalf("%d %s", r.StatusCode, out)
	}
	if len(up.Requests()) != 0 {
		t.Fatal("a request went upstream without a key")
	}
}

func TestClientReachesOnlyTheProviderHosts(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "reached") }))
	defer other.Close()
	c, err := providergw.NewClient([]string{"api.example.test"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(other.URL); err == nil || !strings.Contains(err.Error(), "egress policy") {
		t.Fatalf("a host outside the provider's list was reachable: %v", err)
	}
	// A redirect to another host is not followed.
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 302) }))
	defer redir.Close()
	c2, _ := providergw.NewClient([]string{"127.0.0.1"}, "")
	resp, err := c2.Get(redir.URL)
	if err != nil || resp.StatusCode != 302 {
		t.Fatalf("redirect: %v %v", resp, err)
	}
	if _, err := providergw.NewClient(nil, "/nonexistent/ca.pem"); err == nil {
		t.Fatal("a missing CA bundle must be an error")
	}
}

func gigaGW(t *testing.T, up *stub.Server, ls *logSink) (*providergw.Gateway, string, *providergw.GigaTokens) {
	t.Helper()
	tokens := &providergw.GigaTokens{AuthKey: func() string { return secret }, Scope: "GIGACHAT_API_PERS", URL: up.URL + "/api/v2/oauth", Client: client(t)}
	g := &providergw.GigaChat{BaseURL: up.URL + "/v1", Tokens: tokens, Client: client(t)}
	gw := providergw.New(g, ls.log)
	base, err := gw.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Close)
	return gw, base, tokens
}

func openAITools() []any {
	return []any{map[string]any{"type": "function", "function": map[string]any{"name": "write", "description": "write a file", "strict": true,
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}}}
}

func TestGigaChatTranslatesToolsToFunctionsAndBack(t *testing.T) {
	up := stub.GigaChat(func(n int, req stub.Request) stub.Reply {
		if n == 0 {
			return stub.Reply{Calls: []stub.Call{{Name: "write", Args: map[string]any{"path": "hello.txt", "content": "hi"}}}}
		}
		return stub.Reply{Text: "done"}
	})
	defer up.Close()
	ls := &logSink{}
	gw, base, _ := gigaGW(t, up, ls)
	tok, _ := gw.Mint()
	msgs := []any{map[string]any{"role": "system", "content": "be brief"}, map[string]any{"role": "user", "content": "make hello.txt"}}
	resp, out := post(t, base, tok, map[string]any{"model": "GigaChat-2-Pro", "messages": msgs, "tools": openAITools(), "tool_choice": "auto", "max_completion_tokens": 100, "stream_options": map[string]any{"include_usage": true}, "store": false})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	var oa map[string]any
	_ = json.Unmarshal(out, &oa)
	ch := oa["choices"].([]any)[0].(map[string]any)
	msg := ch["message"].(map[string]any)
	calls := msg["tool_calls"].([]any)
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	args, _ := fn["arguments"].(string)
	if ch["finish_reason"] != "tool_calls" || fn["name"] != "write" || !strings.Contains(args, `"path":"hello.txt"`) {
		t.Fatalf("answer: %s", out)
	}
	id := calls[0].(map[string]any)["id"].(string)
	// what GigaChat received: functions, not tools; no OpenAI-only fields
	sent := up.Requests()[0]
	if sent.Authorization != "Bearer giga-token-1" || sent.Body["tools"] != nil || sent.Body["store"] != nil || sent.Body["stream_options"] != nil {
		t.Fatalf("request to GigaChat: %s", sent.Raw)
	}
	f0 := sent.Body["functions"].([]any)[0].(map[string]any)
	if f0["name"] != "write" || f0["strict"] != nil || sent.Body["function_call"] != "auto" || sent.Body["max_tokens"] != float64(100) {
		t.Fatalf("functions: %s", sent.Raw)
	}
	// second turn: the tool result goes back as a function message, the call keeps its state id
	msgs = append(msgs,
		map[string]any{"role": "assistant", "content": nil, "tool_calls": calls},
		map[string]any{"role": "tool", "tool_call_id": id, "content": "Successfully wrote 2 bytes"})
	resp, out = post(t, base, tok, map[string]any{"model": "GigaChat-2-Pro", "messages": msgs, "tools": openAITools()})
	if resp.StatusCode != 200 || !strings.Contains(string(out), `"content":"done"`) {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	gm := up.Requests()[1].Body["messages"].([]any)
	if len(gm) != 4 {
		t.Fatalf("messages: %s", up.Requests()[1].Raw)
	}
	as := gm[2].(map[string]any)
	fc := as["function_call"].(map[string]any)
	if as["role"] != "assistant" || fc["name"] != "write" || fc["arguments"].(map[string]any)["path"] != "hello.txt" || as["functions_state_id"] != "00000000-0000-4000-8000-000000000000" {
		t.Fatalf("assistant message: %v", as)
	}
	fm := gm[3].(map[string]any)
	if fm["role"] != "function" || fm["name"] != "write" || !strings.Contains(fm["content"].(string), `"result"`) {
		t.Fatalf("function message: %v", fm)
	}
	if strings.Contains(ls.all(), secret) || strings.Contains(string(out), secret) {
		t.Fatal("the credential leaked")
	}
}

func TestGigaChatStreamBecomesOpenAIChunks(t *testing.T) {
	up := stub.GigaChat(func(n int, req stub.Request) stub.Reply {
		if n == 0 {
			return stub.Reply{Text: "I will write it. ", Calls: []stub.Call{{Name: "write", Args: map[string]any{"path": "a.txt"}}}}
		}
		return stub.Reply{Text: "all done"}
	})
	defer up.Close()
	gw, base, _ := gigaGW(t, up, &logSink{})
	tok, _ := gw.Mint()
	_, out := post(t, base, tok, map[string]any{"model": "GigaChat", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "x"}}, "tools": openAITools()})
	var text strings.Builder
	var toolName, toolArgs, finish string
	var usage bool
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.HasSuffix(line, "[DONE]") {
			continue
		}
		var c map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &c); err != nil {
			t.Fatalf("bad chunk %q", line)
		}
		if c["object"] != "chat.completion.chunk" {
			t.Fatalf("object %v", c["object"])
		}
		if c["usage"] != nil {
			usage = true
		}
		for _, ch := range c["choices"].([]any) {
			cm := ch.(map[string]any)
			d := cm["delta"].(map[string]any)
			if s, ok := d["content"].(string); ok {
				text.WriteString(s)
			}
			if tcs, ok := d["tool_calls"].([]any); ok {
				f := tcs[0].(map[string]any)["function"].(map[string]any)
				toolName, toolArgs = f["name"].(string), f["arguments"].(string)
			}
			if f, ok := cm["finish_reason"].(string); ok {
				finish = f
			}
		}
	}
	if text.String() != "I will write it. " || toolName != "write" || !strings.Contains(toolArgs, `"path":"a.txt"`) || finish != "tool_calls" || !usage {
		t.Fatalf("text %q tool %s %s finish %s usage %v\n%s", text.String(), toolName, toolArgs, finish, usage, out)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(out)), "data: [DONE]") {
		t.Fatal("no [DONE]")
	}
}

func TestGigaChatSplitsParallelCallsAndWrapsResults(t *testing.T) {
	in := map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "user", "content": "go"},
		map[string]any{"role": "assistant", "content": "two things", "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "ls", "arguments": `{"path":"."}`}},
			map[string]any{"id": "c2", "type": "function", "function": map[string]any{"name": "read", "arguments": `{"path":"a"}`}}}},
		map[string]any{"role": "tool", "tool_call_id": "c2", "content": `{"ok":true}`},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "a\nb"},
	}, "tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "read"}}, "tools": openAITools()}
	out, _ := providergw.ToGigaChat(in)
	msgs := out["messages"].([]any)
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "user,assistant,function,assistant,function" {
		t.Fatalf("roles %v", roles)
	}
	f1 := msgs[2].(map[string]any)["content"].(string)
	f2 := msgs[4].(map[string]any)["content"].(string)
	if f1 != `{"result":"a\nb"}` || f2 != `{"ok":true}` {
		t.Fatalf("results %q %q", f1, f2)
	}
	if fc, _ := out["function_call"].(map[string]any); fc["name"] != "read" {
		t.Fatalf("tool_choice: %v", out["function_call"])
	}
}

func TestGigaChatTokenIsRefreshed(t *testing.T) {
	up := stub.GigaChat(func(int, stub.Request) stub.Reply { return stub.Reply{Text: "ok"} })
	defer up.Close()
	up.TokenTTL = 100 * time.Millisecond // shorter than the one-minute margin: every request renews
	gw, base, tokens := gigaGW(t, up, &logSink{})
	now := time.Now()
	tokens.Now = func() time.Time { return now }
	tok, _ := gw.Mint()
	body := map[string]any{"model": "GigaChat", "messages": []any{map[string]any{"role": "user", "content": "x"}}}
	for i := 0; i < 3; i++ {
		if r, out := post(t, base, tok, body); r.StatusCode != 200 {
			t.Fatalf("%d %s", r.StatusCode, out)
		}
	}
	if up.TokenCalls != 3 {
		t.Fatalf("a token that ends within a minute must be renewed: %d OAuth calls", up.TokenCalls)
	}
	// A normal 30-minute token is reused until a minute before its end.
	up2 := stub.GigaChat(func(int, stub.Request) stub.Reply { return stub.Reply{Text: "ok"} })
	defer up2.Close()
	gw2, base2, tokens2 := gigaGW(t, up2, &logSink{})
	clock := time.Now()
	tokens2.Now = func() time.Time { return clock }
	tok2, _ := gw2.Mint()
	post(t, base2, tok2, body)
	post(t, base2, tok2, body)
	if up2.TokenCalls != 1 {
		t.Fatalf("token not reused: %d", up2.TokenCalls)
	}
	clock = clock.Add(29*time.Minute + 30*time.Second)
	// the stub's own clock is real, so this token is still valid there: only the renewal margin is checked
	post(t, base2, tok2, body)
	if up2.TokenCalls != 2 {
		t.Fatalf("token not renewed a minute before its end: %d", up2.TokenCalls)
	}
}

func TestGigaChatRenewsOnceOn401(t *testing.T) {
	up := stub.GigaChat(func(int, stub.Request) stub.Reply { return stub.Reply{Text: "ok"} })
	defer up.Close()
	gw, base, tokens := gigaGW(t, up, &logSink{})
	tok, _ := gw.Mint()
	if _, err := tokens.Token(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	up.TokenTTL = time.Hour
	// the provider forgets the token (revoked early): the first call gets 401, the gateway renews and retries once
	up.Expire()
	r, out := post(t, base, tok, map[string]any{"model": "GigaChat", "messages": []any{map[string]any{"role": "user", "content": "x"}}})
	if r.StatusCode != 200 || up.TokenCalls != 2 {
		t.Fatalf("%d %s, OAuth calls %d", r.StatusCode, out, up.TokenCalls)
	}
}

func TestGigaChatNeedsTheAuthorizationKey(t *testing.T) {
	up := stub.GigaChat(func(int, stub.Request) stub.Reply { return stub.Reply{Text: "ok"} })
	defer up.Close()
	tokens := &providergw.GigaTokens{AuthKey: func() string { return "" }, URL: up.URL + "/api/v2/oauth", Client: client(t)}
	g := &providergw.GigaChat{BaseURL: up.URL + "/v1", Tokens: tokens, Client: client(t)}
	if err := g.Ready(); err == nil {
		t.Fatal("ready without a key")
	}
	if hosts := g.Hosts(); len(hosts) != 2 {
		t.Fatalf("hosts %v", hosts)
	}
	def := &providergw.GigaChat{Tokens: &providergw.GigaTokens{}}
	if h := strings.Join(def.Hosts(), ","); h != "api.giga.chat,ngw.devices.sberbank.ru" {
		t.Fatalf("default hosts %s", h)
	}
	y := &providergw.Yandex{}
	if h := strings.Join(y.Hosts(), ","); h != "ai.api.cloud.yandex.net" {
		t.Fatalf("yandex hosts %s", h)
	}
}

func TestWithModelsRewritesAndRefuses(t *testing.T) {
	up := stub.OpenAI(func(int, stub.Request) stub.Reply { return stub.Reply{Text: "ok"} })
	defer up.Close()
	ls := &logSink{}
	y := &providergw.Yandex{BaseURL: up.URL + "/v1", FolderID: "f", Key: func() string { return secret }, Client: client(t)}
	gw := providergw.New(providergw.WithModels(y, map[string]string{"yagpt": "gpt://f/yandexgpt/rc"}), ls.log)
	base, err := gw.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	tok, _ := gw.Mint()
	if r, out := post(t, base, tok, map[string]any{"model": "yagpt", "max_tokens": 9007199254740993}); r.StatusCode != 200 {
		t.Fatalf("%d %s", r.StatusCode, out)
	}
	if got := up.Requests()[0].Body["model"]; got != "gpt://f/yandexgpt/rc" {
		t.Fatalf("model sent upstream: %v", got)
	}
	if !strings.Contains(string(up.Requests()[0].Raw), "9007199254740993") {
		t.Fatalf("a large number was altered: %s", up.Requests()[0].Raw)
	}
	if r, _ := post(t, base, tok, map[string]any{"model": "gpt://f/other/latest"}); r.StatusCode != 400 {
		t.Fatalf("an unconfigured model was passed: %d", r.StatusCode)
	}
	if len(up.Requests()) != 1 {
		t.Fatal("refused model reached the provider")
	}
	if err := providergw.WithModels(y, nil).Ready(); err == nil {
		t.Fatal("ready with no models")
	}
}
