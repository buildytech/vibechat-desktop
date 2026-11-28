// Package stub is a stand-in model provider for tests: an OpenAI-compatible server and a GigaChat-format server that
// answer from a script (text, tool calls, streaming or not) and record what they received. No network, no key.
package stub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Call is a tool call in a Reply.
type Call struct {
	Name string
	Args map[string]any
}

// Reply is what the stub answers to one request.
type Reply struct {
	Text  string
	Calls []Call
	// Status other than 0 answers that HTTP status with Text as the error message.
	Status int
}

// Request is one request the stub saw (keys of headers are not recorded except the checked ones).
type Request struct {
	Path          string
	Authorization string
	Headers       http.Header
	Body          map[string]any
	Raw           []byte
}

// Script decides the reply from the request; n counts requests from 0.
type Script func(n int, req Request) Reply

// Server is a started stub.
type Server struct {
	*httptest.Server
	mu     sync.Mutex
	script Script
	reqs   []Request
	// TokenCalls counts OAuth token requests (GigaChat stub).
	TokenCalls int
	// TokenTTL is how long an issued token lives (GigaChat stub); 0 = 30 minutes.
	TokenTTL time.Duration
	tokens   map[string]time.Time
	seq      int
}

// Expire makes every token issued so far invalid (GigaChat stub).
func (s *Server) Expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.tokens {
		s.tokens[k] = time.Unix(0, 0)
	}
}

// Requests are the chat requests seen so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

func (s *Server) record(r *http.Request) (Request, int, bool) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	req := Request{Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Headers: r.Header.Clone(), Body: body, Raw: raw}
	s.mu.Lock()
	n := len(s.reqs)
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	stream, _ := body["stream"].(bool)
	return req, n, stream
}

func (s *Server) callID(i int) string {
	s.mu.Lock()
	s.seq++
	n := s.seq
	s.mu.Unlock()
	return fmt.Sprintf("call_%d_%d", n, i)
}

// OpenAI starts an OpenAI-compatible provider (POST /v1/chat/completions).
func OpenAI(script Script) *Server {
	s := &Server{script: script}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		req, n, stream := s.record(r)
		rep := script(n, req)
		if rep.Status != 0 {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(rep.Status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": rep.Text}})
			return
		}
		model, _ := req.Body["model"].(string)
		var calls []any
		for i, c := range rep.Calls {
			args, _ := json.Marshal(c.Args)
			calls = append(calls, map[string]any{"index": i, "id": s.callID(i), "type": "function", "function": map[string]any{"name": c.Name, "arguments": string(args)}})
		}
		finish := "stop"
		if len(calls) > 0 {
			finish = "tool_calls"
		}
		usage := map[string]any{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18}
		if !stream {
			msg := map[string]any{"role": "assistant", "content": rep.Text}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
			}
			w.Header().Set("content-type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "stub", "object": "chat.completion", "created": 1, "model": model,
				"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}, "usage": usage})
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		send := func(v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
			if fl != nil {
				fl.Flush()
			}
		}
		chunk := func(delta map[string]any, fin any) map[string]any {
			return map[string]any{"id": "stub", "object": "chat.completion.chunk", "created": 1, "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": fin}}}
		}
		send(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
		for _, part := range split(rep.Text) {
			send(chunk(map[string]any{"content": part}, nil))
		}
		for _, c := range calls {
			cm := c.(map[string]any)
			f := cm["function"].(map[string]any)
			args := f["arguments"].(string)
			half := len(args) / 2
			send(chunk(map[string]any{"tool_calls": []any{map[string]any{"index": cm["index"], "id": cm["id"], "type": "function", "function": map[string]any{"name": f["name"], "arguments": args[:half]}}}}, nil))
			send(chunk(map[string]any{"tool_calls": []any{map[string]any{"index": cm["index"], "function": map[string]any{"arguments": args[half:]}}}}, nil))
		}
		send(chunk(map[string]any{}, finish))
		send(map[string]any{"id": "stub", "object": "chat.completion.chunk", "created": 1, "model": model, "choices": []any{}, "usage": usage})
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	s.Server = httptest.NewServer(mux)
	return s
}

func split(text string) []string {
	if text == "" {
		return nil
	}
	words := strings.SplitAfter(text, " ")
	return words
}

// GigaChat starts a GigaChat-format provider: POST /api/v2/oauth (token), POST /v1/chat/completions with functions /
// function_call (arguments as an object, the whole call in one chunk when streaming). A Reply with Calls answers the
// first call only (GigaChat has no parallel calls outside its largest model).
func GigaChat(script Script) *Server {
	s := &Server{script: script, tokens: map[string]time.Time{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/oauth", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.TokenCalls++
		n := s.TokenCalls
		ttl := s.TokenTTL
		if ttl == 0 {
			ttl = 30 * time.Minute
		}
		tok := fmt.Sprintf("giga-token-%d", n)
		exp := time.Now().Add(ttl)
		s.tokens[tok] = exp
		s.mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), "scope=GIGACHAT_") || r.Header.Get("RqUID") == "" || !strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
			http.Error(w, "bad oauth request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "expires_at": exp.UnixMilli()})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		exp, ok := s.tokens[auth]
		s.mu.Unlock()
		if !ok || time.Now().After(exp) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		req, n, stream := s.record(r)
		rep := script(n, req)
		if rep.Status != 0 {
			w.WriteHeader(rep.Status)
			_, _ = io.WriteString(w, rep.Text)
			return
		}
		model, _ := req.Body["model"].(string)
		usage := map[string]any{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18, "precached_prompt_tokens": 0}
		msg := map[string]any{"role": "assistant", "content": rep.Text}
		finish := "stop"
		if len(rep.Calls) > 0 {
			msg["content"] = ""
			msg["function_call"] = map[string]any{"name": rep.Calls[0].Name, "arguments": rep.Calls[0].Args}
			msg["functions_state_id"] = fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
			finish = "function_call"
		}
		if !stream {
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": msg, "index": 0, "finish_reason": finish}},
				"created": 1, "model": model, "object": "chat.completion", "usage": usage})
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		send := func(v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
			if fl != nil {
				fl.Flush()
			}
		}
		for _, part := range split(rep.Text) {
			send(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": part, "role": "assistant"}, "index": 0}}, "created": 1, "model": model, "object": "chat.completion"})
		}
		last := map[string]any{"delta": map[string]any{"content": ""}, "index": 0, "finish_reason": finish}
		if len(rep.Calls) > 0 {
			last["delta"] = map[string]any{"content": "", "function_call": msg["function_call"], "functions_state_id": msg["functions_state_id"]}
		}
		send(map[string]any{"choices": []any{last}, "created": 1, "model": model, "object": "chat.completion", "usage": usage})
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	s.Server = httptest.NewServer(mux)
	return s
}
