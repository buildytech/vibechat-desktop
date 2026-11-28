package pi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fake Pi: the test binary itself, started as the "node" of an Install whose entry script is fakeEntry. It speaks the
// RPC protocol the way Pi does and does what the prompt's first word says. It lets the adapter be tested in the jail
// without Node or Pi (the E2E tests use the real ones).
const fakeEntry = "/fake-pi/cli.js"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == fakeEntry {
		os.Exit(fakePi(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func fakeInstall(t *testing.T) Install {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	return Install{Node: exe, NodeVersion: "22.99.0", Dir: dir, Version: "test", Entry: fakeEntry}
}

func out(w io.Writer, v map[string]any) {
	b, _ := json.Marshal(v)
	w.Write(append(b, '\n'))
}

func text(w io.Writer, s string) {
	out(w, map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": 0, "delta": s}})
}

func settled(w io.Writer, aborted bool) {
	out(w, map[string]any{"type": "agent_settled", "aborted": aborted})
}

func fakePi(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := map[string]string{}
	for i := 0; i < len(argv); i++ {
		if strings.HasPrefix(argv[i], "--") {
			if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
				flags[argv[i]] = argv[i+1]
				i++
			} else {
				flags[argv[i]] = "true"
			}
		}
	}
	br := bufio.NewReaderSize(stdin, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var cmd map[string]any
			_ = json.Unmarshal(line, &cmd)
			switch cmd["type"] {
			case "prompt":
				msg, _ := cmd["message"].(string)
				if !fakePrompt(cmd, msg, flags, br, stdout, stderr) {
					return 3
				}
			case "abort":
				out(stdout, map[string]any{"type": "response", "command": "abort", "success": true})
			}
		}
		if err != nil {
			return 0 // stdin closed: orderly shutdown
		}
	}
}

func fakePrompt(cmd map[string]any, msg string, flags map[string]string, br *bufio.Reader, stdout, stderr io.Writer) bool {
	id := cmd["id"]
	ok := func(disp string) {
		out(stdout, map[string]any{"id": id, "type": "response", "command": "prompt", "success": true, "data": map[string]any{"disposition": disp}})
	}
	scenario, _, _ := strings.Cut(msg, " ")
	switch scenario {
	case "reject":
		out(stdout, map[string]any{"id": id, "type": "response", "command": "prompt", "success": false, "error": "Model not found: " + os.Getenv("VIBECHAT_GATEWAY_KEY")})
		return true
	case "handled":
		ok("handled")
		return true
	case "crash":
		fmt.Fprintf(stderr, "boom: token %s\n", os.Getenv("VIBECHAT_GATEWAY_KEY"))
		return false
	case "ui":
		ok("started")
		out(stdout, map[string]any{"type": "extension_ui_request", "id": "ui-1", "method": "confirm", "title": "Run?", "message": "m"})
		out(stdout, map[string]any{"type": "extension_ui_request", "id": "ui-2", "method": "notify", "message": "just a notice"})
		line, _ := br.ReadBytes('\n')
		text(stdout, "ui answer: "+strings.TrimSpace(string(line)))
		settled(stdout, false)
		return true
	case "bigline":
		ok("started")
		big := strings.Repeat("x\u2028y", 700_000) // ~2.5 MB on one line, with U+2028 inside the JSON string
		text(stdout, big)
		settled(stdout, false)
		return true
	case "huge":
		ok("started")
		text(stdout, strings.Repeat("h", maxRecord+1024))
		settled(stdout, false)
		select {}
	case "hang", "deaf":
		ok("started")
		text(stdout, "working")
		if scenario == "deaf" {
			select {} // never answers abort or EOF: only the kill ends it
		}
		for {
			line, err := br.ReadBytes('\n')
			if strings.Contains(string(line), `"abort"`) {
				out(stdout, map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "content": []any{}, "stopReason": "aborted"}})
				settled(stdout, true)
				return true
			}
			if err != nil {
				return false
			}
		}
	case "observe":
		ok("started")
		obs := map[string]any{"flags": flags}
		env := os.Environ()
		obs["gatewayToken"] = os.Getenv("VIBECHAT_GATEWAY_KEY")
		var leaked []string
		for _, kv := range env {
			if strings.Contains(kv, testKey) || strings.HasPrefix(kv, "VIBECHAT_YANDEX") || strings.HasPrefix(kv, "VIBECHAT_GIGACHAT") {
				name, _, _ := strings.Cut(kv, "=")
				leaked = append(leaked, name)
			}
		}
		obs["leakedEnv"] = leaked
		obs["piOffline"] = os.Getenv("PI_OFFLINE")
		obs["agentDirEnv"] = os.Getenv("PI_CODING_AGENT_DIR")
		cwd, _ := os.Getwd()
		obs["cwd"] = cwd
		// models.json: written by the host, no key in it, pointing at loopback
		raw, err := os.ReadFile(filepath.Join(flags["--agent-dir"], "models.json"))
		obs["modelsJSON"] = string(raw)
		obs["modelsErr"] = fmt.Sprint(err)
		// the data folder is hidden: this probe file exists on the host
		_, serr := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(flags["--session-dir"])))), "secret.txt"))
		obs["secretVisible"] = serr == nil
		// the workspace is writable, the session folder too, the Pi folder is not
		obs["wsWrite"] = os.WriteFile(filepath.Join(cwd, "made-by-pi.txt"), []byte("hi"), 0o644) == nil
		obs["sessWrite"] = os.WriteFile(filepath.Join(flags["--session-dir"], "s.jsonl"), []byte("{}\n"), 0o600) == nil
		_, werr := os.Create(filepath.Join(filepath.Dir(os.Args[0]), "should-not-exist"))
		obs["exeDirWritable"] = werr == nil
		// call the gateway from inside the jail, the way Pi does
		var base struct {
			Providers map[string]struct {
				BaseURL string `json:"baseUrl"`
			} `json:"providers"`
		}
		_ = json.Unmarshal(raw, &base)
		body := `{"model":"yagpt","messages":[{"role":"user","content":"ping"}]}`
		req, _ := http.NewRequest("POST", base.Providers["vibechat"].BaseURL+"/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+os.Getenv("VIBECHAT_GATEWAY_KEY"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			obs["gatewayErr"] = err.Error()
		} else {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			obs["gatewayStatus"] = resp.StatusCode
			obs["gatewayBody"] = string(b)
		}
		b, _ := json.Marshal(obs)
		text(stdout, string(b))
		settled(stdout, false)
		return true
	}
	ok("started")
	text(stdout, "echo: "+msg)
	out(stdout, map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "echo: " + msg}},
		"stopReason": "stop", "usage": map[string]any{"input": 3, "output": 2, "totalTokens": 5}}})
	settled(stdout, false)
	return true
}
