package host

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/buildytech/vibechat-desktop/internal/agent"
	"github.com/buildytech/vibechat-desktop/internal/secrets"
	"github.com/buildytech/vibechat-desktop/internal/store"
)

func (h *Host) registerAccount() {
	h.router.Handle("GET /me/cursor", h.meCursor)
	h.router.Handle("GET /me/credentials", h.listCredentials)
	h.router.Handle("PUT /me/credentials/:runtime/:providerId", h.putCredential)
	h.router.Handle("DELETE /me/credentials/:runtime/:providerId", h.deleteCredential)
}

func (h *Host) agentUnavailable(w http.ResponseWriter, err error) {
	if errors.Is(err, agent.ErrNoKey) {
		WriteError(w, http.StatusServiceUnavailable, agent.ErrNoKey.Error())
		return
	}
	WriteError(w, http.StatusServiceUnavailable, "The agent is off on this machine: "+err.Error())
}

func (h *Host) meCursor(w http.ResponseWriter, r *http.Request, _ Params) {
	if h.opts.Agent == nil {
		WriteError(w, http.StatusServiceUnavailable, "The agent runtime is not part of this build")
		return
	}
	if !h.opts.Agent.HasKey() {
		WriteError(w, http.StatusServiceUnavailable, agent.ErrNoKey.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	acc, err := h.opts.Agent.Me(ctx)
	if err != nil {
		if agent.IsUnauthenticated(err) {
			WriteError(w, http.StatusUnauthorized, h.agentError("", err))
			return
		}
		h.agentUnavailable(w, err)
		return
	}
	display := strings.TrimSpace(acc.UserFirstName + " " + acc.UserLastName)
	nul := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	WriteJSON(w, http.StatusOK, map[string]any{"account": map[string]any{
		"firstName": nul(acc.UserFirstName), "lastName": nul(acc.UserLastName), "displayName": nul(display),
		"email": nul(acc.UserEmail), "apiKeyName": nul(acc.APIKeyName),
	}})
}

func credentialJSON(key, source string) map[string]any {
	now := store.Now()
	return map[string]any{"runtime": "cursor-sdk", "providerId": "cursor", "hint": source, "masked": secrets.Mask(key), "createdAt": now, "updatedAt": now}
}

func (h *Host) listCredentials(w http.ResponseWriter, _ *http.Request, _ Params) {
	out := []map[string]any{}
	if h.opts.Keys != nil {
		if key, source := h.opts.Keys.Key(); key != "" {
			out = append(out, credentialJSON(key, source))
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"credentials": out})
}

func cursorSlot(p Params) bool { return p["runtime"] == "cursor-sdk" && p["providerId"] == "cursor" }

func (h *Host) putCredential(w http.ResponseWriter, r *http.Request, p Params) {
	if !cursorSlot(p) || h.opts.Keys == nil {
		WriteError(w, http.StatusNotFound, "Only the Cursor key is kept by this app")
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := readJSON(r, 8<<10, &body); err != nil || strings.TrimSpace(body.Key) == "" {
		WriteError(w, http.StatusBadRequest, "Paste a Cursor API key: "+secrets.WhereToGet)
		return
	}
	key := strings.TrimSpace(body.Key)
	if err := secrets.CheckFormat(key); err != nil {
		WriteError(w, http.StatusBadRequest, "Not saved: "+err.Error()+".")
		return
	}
	if f := secrets.Foreign(key); f != "" {
		WriteError(w, http.StatusBadRequest, "Not saved: this is a "+f+" key, not a key of type Cursor; "+secrets.WhereToGet+".")
		return
	}
	if err := h.opts.Keys.Store(key); err != nil {
		WriteError(w, http.StatusInternalServerError, "Could not save the key in the system keychain: "+err.Error())
		return
	}
	stored, source := h.opts.Keys.Key()
	out := map[string]any{"credential": credentialJSON(stored, source)}
	// Online check through the bridge (Me). The key is kept either way: offline the check cannot decide, and a
	// rejected key is reported with what to do instead of being thrown away.
	if h.opts.Agent != nil && h.opts.Agent.SandboxError() == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		acc, err := h.opts.Agent.CheckKey(ctx, key)
		cancel()
		switch {
		case err == nil:
			out["account"] = map[string]any{"email": acc.UserEmail, "apiKeyName": acc.APIKeyName}
			h.logf("key saved and accepted by Cursor (%s)", secrets.Describe(key, source))
		case agent.IsUnauthenticated(err):
			out["warning"] = secrets.AuthHint(key, source) + " Cursor said: " + err.Error()
			h.logf("key saved but rejected by Cursor: %v (%s)", err, secrets.Describe(key, source))
		default:
			out["warning"] = "Saved, but Cursor could not check the key now: " + err.Error()
			h.logf("key saved, not checked: %v (%s)", err, secrets.Describe(key, source))
		}
	}
	WriteJSON(w, http.StatusOK, out)
}

// agentError is the user-facing text of an agent failure; a key Cursor rejected gets what to do about it.
func (h *Host) agentError(prefix string, err error) string {
	msg := prefix + err.Error()
	if agent.IsUnauthenticated(err) && h.opts.Keys != nil {
		key, source := h.opts.Keys.Key()
		h.logf("cursor rejected the key: %v (%s)", err, secrets.Describe(key, source))
		msg += "\n\n" + secrets.AuthHint(key, source)
	}
	return msg
}

func (h *Host) deleteCredential(w http.ResponseWriter, _ *http.Request, p Params) {
	if !cursorSlot(p) || h.opts.Keys == nil {
		WriteError(w, http.StatusNotFound, "Only the Cursor key is kept by this app")
		return
	}
	if err := h.opts.Keys.Forget(); err != nil {
		WriteError(w, http.StatusInternalServerError, "Could not remove the key: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Host) logf(format string, args ...any) {
	if h.opts.Log != nil {
		h.opts.Log(fmt.Sprintf(format, args...))
	}
}
