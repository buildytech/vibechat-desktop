package providergw

import (
	"bufio"
	"bytes"
	"crypto/rand"
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
func RestoreToolIDs(body []byte) ([]byte, error) {
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
				if u := UnwrapID(id); u != id {
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

// wrapChoices makes the tool-call ids of one answer or one stream chunk unique; field is "message" or "delta".
// The index of a call is its own "index" (a stream) or its place in the list (a whole answer).
func wrapChoices(v map[string]any, seed, field string) bool {
	changed := false
	choices, _ := v["choices"].([]any)
	for ci, c := range choices {
		choice, _ := c.(map[string]any)
		holder, _ := choice[field].(map[string]any)
		calls, _ := holder["tool_calls"].([]any)
		for i, tc := range calls {
			call, _ := tc.(map[string]any)
			orig, ok := call["id"].(string)
			if !ok || orig == "" {
				continue
			}
			idx := i
			if n, ok := call["index"].(json.Number); ok {
				if k, err := n.Int64(); err == nil {
					idx = int(k)
				}
			}
			call["id"] = wrapID(seed, ci*1000+idx, orig)
			changed = true
		}
	}
	return changed
}

// wrapAnswer rewrites a whole (non-streamed) chat completion.
func wrapAnswer(body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"tool_calls"`)) {
		return body, nil
	}
	var v map[string]any
	if err := decodeNumbers(body, &v); err != nil {
		return nil, err
	}
	if !wrapChoices(v, newSeed(), "message") {
		return body, nil
	}
	return json.Marshal(v)
}

// wrapStream copies an SSE stream to w, rewriting the tool-call ids of the chunks that carry them; every other line
// passes unchanged. One seed per stream: the same call index always gets the same id.
func wrapStream(w http.ResponseWriter, r io.Reader) error {
	fl, _ := w.(http.Flusher)
	br := bufio.NewReaderSize(r, 64<<10)
	seed := newSeed()
	total := 0
	for {
		line, err := br.ReadBytes('\n')
		total += len(line)
		if total > 512<<20 {
			return errors.New("stream too long")
		}
		if len(line) > 0 {
			out := line
			if bytes.HasPrefix(line, []byte("data:")) && bytes.Contains(line, []byte(`"tool_calls"`)) {
				payload := bytes.TrimSpace(line[len("data:"):])
				var v map[string]any
				if decodeNumbers(payload, &v) == nil && wrapChoices(v, seed, "delta") {
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

// forwardAnswer writes a provider answer to w with unique tool-call ids.
func forwardAnswer(w http.ResponseWriter, resp *http.Response) error {
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	ct := resp.Header.Get("Content-Type")
	switch {
	case resp.StatusCode == http.StatusOK && bytes.Contains([]byte(ct), []byte("event-stream")):
		w.WriteHeader(resp.StatusCode)
		return wrapStream(w, resp.Body)
	case resp.StatusCode == http.StatusOK:
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		if err != nil {
			return err
		}
		if len(raw) > maxBody {
			return fmt.Errorf("the answer is over %d bytes", maxBody)
		}
		out, err := wrapAnswer(raw)
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
