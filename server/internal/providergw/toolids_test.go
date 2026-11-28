package providergw_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/buildytech/vibechat-desktop/server/internal/providergw"
	"github.com/buildytech/vibechat-desktop/server/internal/providergw/stub"
)

func twoWrites() stub.Reply {
	return stub.Reply{Calls: []stub.Call{
		{Name: "write", Args: map[string]any{"path": "a.txt", "content": "A"}},
		{Name: "write", Args: map[string]any{"path": "b.txt", "content": "B"}}}}
}

// Yandex AI Studio gives every tool call the function name as its id. The gateway makes the ids unique (two parallel
// calls of one tool must be told apart) and restores the originals when the agent sends the results back.
func TestYandexToolCallIDsAreMadeUniqueAndRestored(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := map[bool]string{false: "plain", true: "stream"}[stream]
		t.Run(name, func(t *testing.T) {
			up := stub.OpenAI(func(n int, req stub.Request) stub.Reply {
				if n == 0 {
					return twoWrites()
				}
				return stub.Reply{Text: "done"}
			}, stub.NameAsID)
			defer up.Close()
			gw, base, _ := yandexGW(t, up, true)
			tok, _ := gw.Mint()
			msgs := []any{map[string]any{"role": "user", "content": "make a.txt and b.txt"}}
			_, out := post(t, base, tok, map[string]any{"model": "m", "stream": stream, "messages": msgs, "tools": openAITools()})
			ids, args := toolCalls(t, string(out), stream)
			if len(ids) != 2 || ids[0] == ids[1] || ids[0] == "write" || ids[1] == "write" {
				t.Fatalf("ids %v in %s", ids, out)
			}
			if !strings.Contains(args[0], "a.txt") || !strings.Contains(args[1], "b.txt") {
				t.Fatalf("arguments %v", args)
			}
			if providergw.UnwrapID(ids[0]) != "write" || providergw.UnwrapID(ids[1]) != "write" {
				t.Fatalf("ids do not carry the original: %v", ids)
			}
			// the agent's next request: the assistant message with its calls and the two results, by the new ids
			calls := []any{}
			for i, id := range ids {
				calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "write", "arguments": args[i]}})
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": nil, "tool_calls": calls},
				map[string]any{"role": "tool", "tool_call_id": ids[0], "content": "ok a"},
				map[string]any{"role": "tool", "tool_call_id": ids[1], "content": "ok b"})
			post(t, base, tok, map[string]any{"model": "m", "messages": msgs, "tools": openAITools()})
			sent := up.Requests()[1].Body["messages"].([]any)
			a := sent[1].(map[string]any)["tool_calls"].([]any)
			if a[0].(map[string]any)["id"] != "write" || a[1].(map[string]any)["id"] != "write" ||
				sent[2].(map[string]any)["tool_call_id"] != "write" || sent[3].(map[string]any)["tool_call_id"] != "write" ||
				sent[2].(map[string]any)["content"] != "ok a" || sent[3].(map[string]any)["content"] != "ok b" {
				t.Fatalf("the provider must see its own ids, in order: %s", up.Requests()[1].Raw)
			}
		})
	}
}

func toolCalls(t *testing.T, out string, stream bool) (ids, args []string) {
	t.Helper()
	if !stream {
		var v struct {
			Choices []struct {
				Message struct {
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct{ Arguments string }
					} `json:"tool_calls"`
				}
			}
		}
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		for _, c := range v.Choices[0].Message.ToolCalls {
			ids, args = append(ids, c.ID), append(args, c.Function.Arguments)
		}
		return
	}
	byIndex := map[int]int{}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var v struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct{ Arguments string }
					} `json:"tool_calls"`
				}
			}
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		for _, ch := range v.Choices {
			for _, c := range ch.Delta.ToolCalls {
				k, seen := byIndex[c.Index]
				if !seen {
					k = len(ids)
					byIndex[c.Index] = k
					ids, args = append(ids, ""), append(args, "")
				}
				if c.ID != "" {
					ids[k] = c.ID
				}
				args[k] += c.Function.Arguments
			}
		}
	}
	return
}

func TestToolIDsOfOtherProvidersAndPlainRequestsAreLeftAlone(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":9007199254740993,"messages":[{"role":"user","content":"hi"}]}`)
	got, err := providergw.RestoreToolIDs(body)
	if err != nil || string(got) != string(body) {
		t.Fatalf("a request without tool history must pass byte for byte: %s %v", got, err)
	}
	withCalls := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_abc","type":"function","function":{"name":"x","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_abc","content":"r"}],"n":9007199254740993}`)
	got, err = providergw.RestoreToolIDs(withCalls)
	if err != nil || string(got) != string(withCalls) {
		t.Fatalf("ids the gateway did not make stay: %s %v", got, err)
	}
	wrapped := []byte(`{"messages":[{"role":"tool","tool_call_id":"vc0a1b2c3d_1_my_tool","content":"r"}],"n":9007199254740993}`)
	got, err = providergw.RestoreToolIDs(wrapped)
	if err != nil || !strings.Contains(string(got), `"tool_call_id":"my_tool"`) || !strings.Contains(string(got), "9007199254740993") {
		t.Fatalf("%s %v", got, err)
	}
	if providergw.UnwrapID("write") != "write" || providergw.UnwrapID("vc0a1b2c3d_0_") != "" || providergw.UnwrapID("vcXYZ_0_write") != "vcXYZ_0_write" {
		t.Fatal("UnwrapID")
	}
	if _, err := providergw.RestoreToolIDs([]byte(`{"tool_calls": nope`)); err == nil {
		t.Fatal("garbage accepted")
	}
}
