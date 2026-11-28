package providergw

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
)

// Yandex AI Studio answers tool calls with id == the function name ("write"), so two parallel calls of one tool share an
// id and the agent cannot tell their results apart. The gateway makes the ids unique on the way out
// (vc<8 hex>_<index>_<original>) and puts the original back on the way in (assistant tool_calls and role:tool messages),
// statelessly: the original is inside the new id, so it survives a resumed chat and a server restart.

var wrappedID = regexp.MustCompile(`^vc[0-9a-f]{8}_\d+_(.*)$`)

// Some providers keep conversation state next to a tool call in a field of their own that the OpenAI format does not
// know (Cloud.ru GigaChat: "tool_state_id" on the assistant message) and may want it back with the follow-up request.
// A provider entry lists such fields as quirks.echoFields. The gateway carries their values inside the tool-call ids it
// hands to the agent (vx.<base64url of a JSON object>.<id>) and puts them back as fields of the assistant message when
// the agent sends the calls back: statelessly, like the unique ids above, so it survives a resumed chat and a restart.
// The id the provider sees is its own again. Only the fields named in the config are ever restored.

var (
	echoedID      = regexp.MustCompile(`^vx\.([A-Za-z0-9_-]+)\.(.*)$`)
	echoFieldName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
	echoValue     = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

// ValidateEchoFields checks the names of quirks.echoFields.
func ValidateEchoFields(names []string) error {
	seen := map[string]bool{}
	for _, n := range names {
		if !echoFieldName.MatchString(n) {
			return fmt.Errorf("echo field %q: lower-case letters, digits and '_'", n)
		}
		switch n {
		case "role", "content", "tool_calls", "tool_call_id", "name", "function_call":
			return fmt.Errorf("echo field %q is part of the OpenAI message format", n)
		}
		if seen[n] {
			return fmt.Errorf("echo field %q is listed twice", n)
		}
		seen[n] = true
	}
	return nil
}

func wrapEcho(fields map[string]string, id string) string {
	raw, _ := json.Marshal(fields) // map keys are sorted: the same state gives the same id
	return "vx." + base64.RawURLEncoding.EncodeToString(raw) + "." + id
}

// splitEcho takes the echoed fields (only the allowed names, only plain values) off an id.
func splitEcho(id string, allow []string) (map[string]string, string) {
	m := echoedID.FindStringSubmatch(id)
	if m == nil {
		return nil, id
	}
	raw, err := base64.RawURLEncoding.DecodeString(m[1])
	if err != nil {
		return nil, id
	}
	var all map[string]string
	if json.Unmarshal(raw, &all) != nil {
		return nil, id
	}
	got := map[string]string{}
	for _, n := range allow {
		if v, ok := all[n]; ok && echoValue.MatchString(v) {
			got[n] = v
		}
	}
	return got, m[2]
}

func newSeed() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func wrapID(seed string, index int, orig string) string {
	return "vc" + seed + "_" + strconv.Itoa(index) + "_" + orig
}

// UnwrapID is the original id of an id the gateway made (any other id comes back unchanged).
func UnwrapID(id string) string {
	if m := echoedID.FindStringSubmatch(id); m != nil {
		id = m[2]
	}
	if m := wrappedID.FindStringSubmatch(id); m != nil {
		return m[1]
	}
	return id
}

func decodeNumbers(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(v)
}

// RestoreToolIDs puts the providers' original tool-call ids back in a request; a request without tool calls in its
// history is returned byte for byte.
func RestoreToolIDs(body []byte) ([]byte, error) { return restoreRequest(body, nil) }

// restoreRequest is RestoreToolIDs that also puts the echo fields (the allowed names) back on the assistant messages
// whose tool-call ids carry them.
func restoreRequest(body []byte, echo []string) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"tool_call_id"`)) && !bytes.Contains(body, []byte(`"tool_calls"`)) {
		return body, nil
	}
	var req map[string]any
	if err := decodeNumbers(body, &req); err != nil {
		return nil, err
	}
	msgs, _ := req["messages"].([]any)
	changed := false
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		if id, ok := msg["tool_call_id"].(string); ok {
			if u := UnwrapID(id); u != id {
				msg["tool_call_id"], changed = u, true
			}
		}
		calls, _ := msg["tool_calls"].([]any)
		for _, c := range calls {
			call, _ := c.(map[string]any)
			if id, ok := call["id"].(string); ok {
				fields, rest := splitEcho(id, echo)
				for k, v := range fields {
					if _, set := msg[k]; !set {
						msg[k] = v
					}
				}
				if u := UnwrapID(rest); u != id {
					call["id"], changed = u, true
				}
			}
		}
	}
	if !changed {
		return body, nil
	}
	return json.Marshal(req)
}

// rewriter says how the tool-call ids of an answer are rewritten on the way out.
type rewriter struct {
	unique bool     // make the ids unique (vc<seed>_<index>_<id>)
	echo   []string // carry these message fields inside the ids
	seed   string
	state  map[int]map[string]string // per choice: the echo fields seen so far in this stream
}

func (rw *rewriter) active() bool { return rw != nil && (rw.unique || len(rw.echo) > 0) }

// wrapChoices rewrites the tool-call ids of one answer or one stream chunk; field is "message" or "delta".
// The index of a call is its own "index" (a stream) or its place in the list (a whole answer).
func wrapChoices(v map[string]any, rw *rewriter, field string) bool {
	changed := false
	choices, _ := v["choices"].([]any)
	for ci, c := range choices {
		choice, _ := c.(map[string]any)
		holder, _ := choice[field].(map[string]any)
		for _, n := range rw.echo {
			if val, ok := holder[n].(string); ok && echoValue.MatchString(val) {
				if rw.state == nil {
					rw.state = map[int]map[string]string{}
				}
				if rw.state[ci] == nil {
					rw.state[ci] = map[string]string{}
				}
				rw.state[ci][n] = val
			}
		}
		calls, _ := holder["tool_calls"].([]any)
		for i, tc := range calls {
			call, _ := tc.(map[string]any)
			id, ok := call["id"].(string)
			if !ok || id == "" {
				continue
			}
			idx := i
			if n, ok := call["index"].(json.Number); ok {
				if k, err := n.Int64(); err == nil {
					idx = int(k)
				}
			}
			if rw.unique {
				id = wrapID(rw.seed, ci*1000+idx, id)
			}
			if st := rw.state[ci]; len(st) > 0 {
				id = wrapEcho(st, id)
			}
			call["id"] = id
			changed = true
		}
	}
	return changed
}

// wrapAnswer rewrites a whole (non-streamed) chat completion.
func wrapAnswer(body []byte, rw *rewriter) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"tool_calls"`)) {
		return body, nil
	}
	var v map[string]any
	if err := decodeNumbers(body, &v); err != nil {
		return nil, err
	}
	rw.seed = newSeed()
	if !wrapChoices(v, rw, "message") {
		return body, nil
	}
	return json.Marshal(v)
}

// wrapStream copies an SSE stream to w, rewriting the tool-call ids of the chunks that carry them; every other line
// passes unchanged. One seed per stream: the same call index always gets the same id.
func wrapStream(w http.ResponseWriter, r io.Reader, rw *rewriter) error {
	fl, _ := w.(http.Flusher)
	br := bufio.NewReaderSize(r, 64<<10)
	rw.seed = newSeed()
	total := 0
	for {
		line, err := br.ReadBytes('\n')
		total += len(line)
		if total > 512<<20 {
			return errors.New("stream too long")
		}
		if len(line) > 0 {
			out := line
			if bytes.HasPrefix(line, []byte("data:")) && (bytes.Contains(line, []byte(`"tool_calls"`)) || rw.watches(line)) {
				payload := bytes.TrimSpace(line[len("data:"):])
				var v map[string]any
				if decodeNumbers(payload, &v) == nil && wrapChoices(v, rw, "delta") {
					if b, merr := json.Marshal(v); merr == nil {
						out = append(append([]byte("data: "), b...), '\n')
					}
				}
			}
			if _, werr := w.Write(out); werr != nil {
				return werr
			}
			if fl != nil && (len(bytes.TrimSpace(line)) == 0 || err != nil) {
				fl.Flush()
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// watches reports whether a chunk carries one of the echo fields (it is parsed so that a later chunk with tool calls
// can use the value).
func (rw *rewriter) watches(line []byte) bool {
	for _, n := range rw.echo {
		if bytes.Contains(line, []byte(`"`+n+`"`)) {
			return true
		}
	}
	return false
}

// forwardAnswer writes a provider answer to w with the tool-call ids rewritten as rw says.
func forwardAnswer(w http.ResponseWriter, resp *http.Response, rw *rewriter) error {
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	ct := resp.Header.Get("Content-Type")
	switch {
	case resp.StatusCode == http.StatusOK && bytes.Contains([]byte(ct), []byte("event-stream")):
		w.WriteHeader(resp.StatusCode)
		return wrapStream(w, resp.Body, rw)
	case resp.StatusCode == http.StatusOK:
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		if err != nil {
			return err
		}
		if len(raw) > maxBody {
			return fmt.Errorf("the answer is over %d bytes", maxBody)
		}
		out, err := wrapAnswer(raw, rw)
		if err != nil {
			out = raw // not the JSON we expected: pass it on as it is
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		_, err = w.Write(out)
		return err
	}
	w.WriteHeader(resp.StatusCode)
	return copyStream(w, resp.Body)
}
