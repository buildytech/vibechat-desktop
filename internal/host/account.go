package host

import (
	"context"
	"errors"
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
		WriteError(w, http.StatusBadRequest, "Paste a Cursor API key")
		return
	}
	if err := h.opts.Keys.Store(body.Key); err != nil {
		WriteError(w, http.StatusInternalServerError, "Could not save the key in the system keychain: "+err.Error())
		return
	}
	key, source := h.opts.Keys.Key()
	WriteJSON(w, http.StatusOK, map[string]any{"credential": credentialJSON(key, source)})
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
