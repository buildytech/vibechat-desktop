package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/buildytech/vibechat-desktop/internal/agent"
	"github.com/buildytech/vibechat-desktop/internal/sandbox"
	"github.com/buildytech/vibechat-desktop/internal/store"
)

const (
	promptTextMax   = 32000
	promptImagesMax = 4
	imageDataMax    = (4*1024*1024 + 2) / 3 * 4
	// agentRunTimeout bounds one prompt (vibecode AGENT_RUN_TIMEOUT_MS default).
	agentRunTimeout = 30 * time.Minute
)

type activeRun struct {
	runID   string
	agentID string
	cancel  context.CancelFunc
	tr      *agent.Translator
}

func (h *Host) registerAgent() {
	h.router.Handle("GET /projects/:id/agent/capabilities", h.agentCapabilities)
	h.router.Handle("GET /projects/:id/agent/sessions", h.listSessions)
	h.router.Handle("POST /projects/:id/agent/sessions", h.createSession)
	h.router.Handle("DELETE /projects/:id/agent/sessions/:sid", h.deleteSession)
	h.router.Handle("POST /projects/:id/agent/sessions/:sid/messages", h.prompt)
	h.router.Handle("GET /projects/:id/agent/sessions/:sid/messages", h.transcript)
	h.router.Handle("GET /projects/:id/agent/sessions/:sid/events", h.events)
	h.router.Handle("GET /projects/:id/agent/sessions/:sid/export", h.exportSession)
	h.router.Handle("POST /projects/:id/agent/sessions/:sid/abort", h.abort)
	h.router.Handle("POST /projects/:id/agent/runs/:rid/feedback", h.feedback)
}

func (h *Host) agentCapabilities(w http.ResponseWriter, r *http.Request, prm Params) {
	if _, ok := h.project(w, prm["id"]); !ok {
		return
	}
	// The menu is the key's model list from Cursor. Without a key or a reachable Cursor only Auto is offered (the
	// run then reports what is wrong).
	models := []agent.Model{{ID: agent.AutoID, Provider: "cursor", Label: "Auto"}}
	if h.opts.Agent != nil && h.opts.Agent.HasKey() && h.opts.Agent.SandboxError() == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		list, err := h.opts.Agent.Models(ctx)
		cancel()
		if err == nil && len(list) > 0 {
			models = list
		} else if err != nil {
			h.logf("model list: %v", err)
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"runtime": "cursor-sdk", "runtimeLabel": "Cursor", "streaming": true, "tools": true, "pty": false, "snapshots": false,
		"cost_usage": false, "evidence_pack": false, "subagents": false, "models": models,
		"ui": map[string]bool{"showThinking": true, "showToolTrace": true},
	})
}

func (h *Host) sessionStatus(sid string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.runs[sid]; ok {
		return "running"
	}
	return "idle"
}

func (h *Host) sessionJSON(s store.Session) map[string]any {
	var title, last any
	if s.Title != "" {
		title = s.Title
	}
	if s.LastMessageAt != "" {
		last = s.LastMessageAt
	}
	return map[string]any{"id": s.ID, "projectId": s.ProjectID, "userId": "local", "runtime": "cursor-sdk", "status": h.sessionStatus(s.ID),
		"title": title, "lastMessageAt": last, "createdAt": s.CreatedAt, "updatedAt": s.UpdatedAt}
}

func (h *Host) listSessions(w http.ResponseWriter, _ *http.Request, prm Params) {
	p, ok := h.project(w, prm["id"])
	if !ok {
		return
	}
	out := []map[string]any{}
	for _, s := range h.opts.Store.Sessions(p.ID) {
		out = append(out, h.sessionJSON(s))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (h *Host) createSession(w http.ResponseWriter, r *http.Request, prm Params) {
	p, ok := h.project(w, prm["id"])
	if !ok {
		return
	}
	sid := store.NewID()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	dir, branch, err := h.createWorkspace(ctx, p, sid)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "Could not prepare the chat's worktree: "+err.Error())
		return
	}
	now := store.Now()
	s := store.Session{ID: sid, ProjectID: p.ID, CreatedAt: now, UpdatedAt: now, Branch: branch}
	if dir != p.Path {
		s.Worktree = dir
	}
	if err := h.opts.Store.PutSession(s); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	WriteJSON(w, http.StatusCreated, map[string]any{"session": h.sessionJSON(s)})
}

func (h *Host) session(w http.ResponseWriter, prm Params) (store.Project, store.Session, bool) {
	p, ok := h.project(w, prm["id"])
	if !ok {
		return p, store.Session{}, false
	}
	s, err := h.opts.Store.Session(p.ID, prm["sid"])
	if err != nil {
		WriteError(w, http.StatusNotFound, "Chat not found")
		return p, s, false
	}
	return p, s, true
}

func (h *Host) stopRun(sid string) {
	h.mu.Lock()
	run := h.runs[sid]
	h.mu.Unlock()
	if run == nil {
		return
	}
	if run.tr != nil && run.tr.BridgeRun != "" && h.opts.Agent != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = h.opts.Agent.Cancel(ctx, run.agentID, run.tr.BridgeRun)
		cancel()
	}
	go func() {
		time.Sleep(15 * time.Second)
		run.cancel()
	}()
}

func (h *Host) deleteSession(w http.ResponseWriter, _ *http.Request, prm Params) {
	p, s, ok := h.session(w, prm)
	if !ok {
		return
	}
	h.stopRun(s.ID)
	h.removeWorktree(p, s)
	_ = h.opts.Store.DeleteSession(s.ID)
	w.WriteHeader(http.StatusNoContent)
}

type promptBody struct {
	Text        string             `json:"text"`
	ModelID     string             `json:"modelId"`
	ModelParams []agent.ModelParam `json:"modelParams"`
	ClientMsgID string             `json:"clientMsgId"`
	Images      []agent.Image      `json:"images"`
}

var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true}

func (b promptBody) validate() error {
	n := utf8.RuneCountInString(b.Text)
	if strings.TrimSpace(b.Text) == "" || n > promptTextMax {
		return fmt.Errorf("Prompt must be 1–%d characters", promptTextMax)
	}
	if !strings.HasPrefix(b.ModelID, "cursor/") || len(b.ModelID) > 128 {
		return fmt.Errorf("Model %q is not a Cursor model", b.ModelID)
	}
	if len(b.Images) > promptImagesMax {
		return fmt.Errorf("At most %d images per prompt", promptImagesMax)
	}
	for _, im := range b.Images {
		if !imageTypes[im.MediaType] || im.Data == "" || len(im.Data) > imageDataMax {
			return errors.New("Unsupported or too large image")
		}
	}
	if len(b.ClientMsgID) > 128 {
		return errors.New("clientMsgId too long")
	}
	return nil
}

func (h *Host) publish(sid string, e agent.Event) { h.bus.Publish(sid, e) }

func (h *Host) prompt(w http.ResponseWriter, r *http.Request, prm Params) {
	p, s, ok := h.session(w, prm)
	if !ok {
		return
	}
	var body promptBody
	if err := readJSON(r, 24<<20, &body); err != nil {
		WriteError(w, http.StatusBadRequest, "Invalid request")
		return
	}
	if err := body.validate(); err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.opts.Agent == nil {
		WriteError(w, http.StatusServiceUnavailable, "The agent runtime is not part of this build")
		return
	}
	// Fail closed: no SDK sandbox, no agent.
	if err := h.opts.Agent.SandboxError(); err != nil {
		h.agentUnavailable(w, err)
		return
	}
	if !h.opts.Agent.HasKey() {
		WriteError(w, http.StatusServiceUnavailable, agent.ErrNoKey.Error())
		return
	}
	cwd := h.sessionDir(p, s)
	if err := sandbox.CheckWorkspace(cwd); err != nil {
		WriteError(w, http.StatusConflict, err.Error())
		return
	}
	runID := store.NewID()
	ctx, cancel := context.WithTimeout(context.Background(), agentRunTimeout)
	defer cancel()
	tr := &agent.Translator{SessionID: s.ID, RunID: runID, Cwd: cwd}
	h.mu.Lock()
	if _, busy := h.runs[s.ID]; busy {
		h.mu.Unlock()
		WriteError(w, http.StatusConflict, "This chat is still working; wait or stop it")
		return
	}
	h.runs[s.ID] = &activeRun{runID: runID, cancel: cancel, tr: tr}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.runs, s.ID)
		h.mu.Unlock()
	}()

	if _, err := h.opts.Store.Append(store.Message{SessionID: s.ID, Role: "user", Content: body.Text, ClientID: body.ClientMsgID, RunID: runID}); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.Title == "" {
		s.Title = oneLineTitle(body.Text)
	}
	h.publish(s.ID, agent.Event{"type": "session.status", "sessionId": s.ID, "status": "running", "runId": runID})

	outcome := h.runPrompt(ctx, p, &s, body, tr)
	h.publish(s.ID, agent.Event{"type": "session.status", "sessionId": s.ID, "status": outcome["status"], "runId": runID})
	WriteJSON(w, http.StatusOK, outcome)
}

// runPrompt does the run and answers the PromptOutcome; the transcript gets the assistant row either way.
func (h *Host) runPrompt(ctx context.Context, p store.Project, s *store.Session, body promptBody, tr *agent.Translator) map[string]any {
	fail := func(msg string) map[string]any {
		_, _ = h.opts.Store.Append(store.Message{SessionID: s.ID, Role: "assistant", Content: "⚠ " + msg, RunID: tr.RunID})
		h.touch(s)
		return map[string]any{"status": "error", "text": "", "error": msg, "runId": tr.RunID}
	}
	if err := h.syncBeforeRun(ctx, p, *s); err != nil {
		return fail(err.Error())
	}
	model, err := h.opts.Agent.Select(ctx, body.ModelID, body.ModelParams)
	if err != nil {
		if errors.Is(err, agent.ErrUnknownModel) {
			return fail(err.Error() + " — pick another model (the list is refreshed when the chat is reopened)")
		}
		return fail(h.agentError("Could not start the agent: ", err))
	}
	cwd := h.sessionDir(p, *s)
	agentID, err := h.opts.Agent.EnsureAgent(ctx, s.AgentID, cwd, model)
	if err != nil {
		return fail(h.agentError("Could not start the agent: ", err) + h.modelDropped(body.ModelID, err))
	}
	if agentID != s.AgentID {
		s.AgentID = agentID
		_ = h.opts.Store.PutSession(*s)
	}
	h.mu.Lock()
	if run := h.runs[s.ID]; run != nil {
		run.agentID = agentID
	}
	h.mu.Unlock()
	sendErr := h.opts.Agent.Send(ctx, agentID, body.Text, body.Images, model, tr, func(e agent.Event) { h.publish(s.ID, e) })
	lctx, lcancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer lcancel()
	changed, landErr := h.landRun(lctx, p, *s, body.Text)
	if len(changed) > 0 {
		paths := make([]any, len(changed))
		for i, c := range changed {
			paths[i] = c
		}
		h.publish(s.ID, agent.Event{"type": "files.changed", "sessionId": s.ID, "paths": paths, "runId": tr.RunID})
	}
	text := tr.Text.String()
	if text == "" {
		text = tr.ResultText
	}
	errMsg := ""
	switch {
	case sendErr != nil:
		errMsg = h.agentError("Agent run failed: ", sendErr) + h.modelDropped(body.ModelID, sendErr)
	case tr.Error != "":
		errMsg = "Agent run " + strings.ToLower(tr.Error) + h.modelDropped(body.ModelID, errors.New(tr.Error))
	case landErr != nil:
		errMsg = landErr.Error()
	}
	content := text
	if errMsg != "" {
		content = strings.TrimSpace(text + "\n\n⚠ " + errMsg)
	}
	if content != "" {
		_, _ = h.opts.Store.Append(store.Message{SessionID: s.ID, Role: "assistant", Content: content, RunID: tr.RunID})
	}
	h.touch(s)
	out := map[string]any{"status": "idle", "text": text, "runId": tr.RunID, "toolCalls": tr.ToolCalls, "filesChanged": len(changed)}
	if errMsg != "" {
		out["status"], out["error"] = "error", errMsg
	}
	if u := tr.Usage; u != nil {
		out["usage"] = map[string]int64{"tokensIn": u.InputTokens, "tokensOut": u.OutputTokens, "cacheReadTokens": u.CacheReadTokens,
			"cacheWriteTokens": u.CacheWriteTokens, "reasoningTokens": u.ReasoningTokens, "totalTokens": u.TotalTokens}
	}
	h.opts.Log(fmt.Sprintf("run %s session %s model %s: status %v tools %d files %d usage %+v", tr.RunID, s.ID, body.ModelID, out["status"], tr.ToolCalls, len(changed), tr.Usage))
	return out
}

func (h *Host) touch(s *store.Session) {
	now := store.Now()
	s.LastMessageAt, s.UpdatedAt = now, now
	_ = h.opts.Store.PutSession(*s)
	if p, err := h.opts.Store.Project(s.ProjectID); err == nil {
		p.UpdatedAt = now
		_ = h.opts.Store.PutProject(p)
	}
}

func (h *Host) transcript(w http.ResponseWriter, r *http.Request, prm Params) {
	_, s, ok := h.session(w, prm)
	if !ok {
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	msgs, err := h.opts.Store.Messages(s.ID, after)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		row := map[string]any{"id": m.ID, "sessionId": m.SessionID, "runId": m.RunID, "role": m.Role, "content": m.Content, "seq": m.Seq, "createdAt": m.CreatedAt}
		if m.Feedback != "" {
			row["feedback"] = m.Feedback
		}
		out = append(out, row)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"messages": out})
}

func (h *Host) events(w http.ResponseWriter, r *http.Request, prm Params) {
	_, s, ok := h.session(w, prm)
	if !ok {
		return
	}
	fl, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-store")
	w.Header().Set("x-accel-buffering", "no")
	w.WriteHeader(http.StatusOK)
	ch, unsub := h.bus.Subscribe(s.ID)
	defer unsub()
	send := func(e agent.Event) bool {
		raw, _ := json.Marshal(e)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return false
		}
		if fl != nil {
			fl.Flush()
		}
		return true
	}
	if !send(agent.Event{"type": "session.status", "sessionId": s.ID, "status": h.sessionStatus(s.ID)}) {
		return
	}
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			if !send(e) {
				return
			}
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

func (h *Host) exportSession(w http.ResponseWriter, _ *http.Request, prm Params) {
	p, s, ok := h.session(w, prm)
	if !ok {
		return
	}
	msgs, _ := h.opts.Store.Messages(s.ID, 0)
	rows := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		rows = append(rows, map[string]any{"role": m.Role, "text": m.Content, "createdAt": m.CreatedAt, "seq": m.Seq, "runId": m.RunID})
	}
	var title any
	if s.Title != "" {
		title = s.Title
	}
	bundle := map[string]any{"format": "buildy.chat.v1", "exportedAt": store.Now(), "projectId": p.ID,
		"session": map[string]any{"id": s.ID, "title": title, "runtime": "cursor-sdk"}, "messages": rows}
	raw, _ := json.Marshal(bundle)
	sum := sha256.Sum256(raw)
	bundle["checksum"] = "sha256:" + hex.EncodeToString(sum[:])
	WriteJSON(w, http.StatusOK, bundle)
}

func (h *Host) abort(w http.ResponseWriter, _ *http.Request, prm Params) {
	_, s, ok := h.session(w, prm)
	if !ok {
		return
	}
	h.stopRun(s.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Host) feedback(w http.ResponseWriter, r *http.Request, prm Params) {
	p, ok := h.project(w, prm["id"])
	if !ok {
		return
	}
	var body struct {
		Rating string `json:"rating"`
	}
	if err := readJSON(r, 16<<10, &body); err != nil || (body.Rating != "up" && body.Rating != "down") {
		WriteError(w, http.StatusBadRequest, "rating must be up or down")
		return
	}
	s, err := h.opts.Store.SessionByRun(p.ID, prm["rid"])
	if err != nil {
		WriteError(w, http.StatusNotFound, "Run not found")
		return
	}
	if err := h.opts.Store.SetFeedback(s.ID, prm["rid"], body.Rating); err != nil {
		WriteError(w, http.StatusNotFound, "Run not found")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// modelDropped marks a model that failed with an access or plan error unavailable for this key and says so.
func (h *Host) modelDropped(modelID string, err error) string {
	if h.opts.Agent != nil && h.opts.Agent.MarkUnavailable(modelID, err) {
		return "\n\nThis model is not available for your Cursor key and was removed from the model list; pick another one."
	}
	return ""
}
