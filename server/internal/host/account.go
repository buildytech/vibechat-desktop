package host

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
	"github.com/buildytech/vibechat-desktop/server/internal/store"
)

func (h *Host) registerAccount() {
	h.router.Handle("GET /me/cursor", h.meCursor)
	h.router.Handle("GET /me/credentials", h.listCredentials)
	h.router.Handle("PUT /me/credentials/:runtime/:providerId", h.putCredential)
	h.router.Handle("DELETE /me/credentials/:runtime/:providerId", h.deleteCredential)
}

func (h *Host) agentUnavailable(w http.ResponseWriter, err error) {
	if errors.Is(err, codingagent.ErrNoCredential) {
		WriteError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	WriteError(w, http.StatusServiceUnavailable, "The agent is off on this machine: "+err.Error())
}

// noCredential is the error of an agent that needs a credential and has none.
func noCredential(ad codingagent.Adapter) error {
	if c := ad.Credential(); c != nil && c.NoCredential != nil {
		return c.NoCredential
	}
	return codingagent.ErrNoCredential
}

func (h *Host) meCursor(w http.ResponseWriter, r *http.Request, _ Params) {
	ad := h.adapter(codingagent.RuntimeCursorSDK)
	if ad == nil {
		WriteError(w, http.StatusServiceUnavailable, "The agent runtime is not part of this build")
		return
	}
	if !ad.HasCredential() {
		WriteError(w, http.StatusServiceUnavailable, noCredential(ad).Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	acc, err := ad.Account(ctx)
	if err != nil {
		if ad.Unauthenticated(err) {
			WriteError(w, http.StatusUnauthorized, h.agentError(ad, "", err))
			return
		}
		h.agentUnavailable(w, err)
		return
	}
	display := strings.TrimSpace(acc.FirstName + " " + acc.LastName)
	nul := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	WriteJSON(w, http.StatusOK, map[string]any{"account": map[string]any{
		"firstName": nul(acc.FirstName), "lastName": nul(acc.LastName), "displayName": nul(display),
		"email": nul(acc.Email), "apiKeyName": nul(acc.KeyName),
	}})
}

func credentialJSON(runtime string, c *codingagent.CredentialSpec, key, source string) map[string]any {
	now := store.Now()
	return map[string]any{"runtime": runtime, "providerId": c.Provider, "hint": source, "masked": c.Mask(key), "createdAt": now, "updatedAt": now}
}

// slots are the credential slots of the registered runtimes, sorted by runtime.
func (h *Host) slots() (runtimes []string, specs map[string]*codingagent.CredentialSpec) {
	specs = map[string]*codingagent.CredentialSpec{}
	for _, rt := range h.opts.Agents.Runtimes() {
		if c := h.adapter(rt).Credential(); c != nil {
			runtimes = append(runtimes, rt)
			specs[rt] = c
		}
	}
	return runtimes, specs
}

func (h *Host) listCredentials(w http.ResponseWriter, _ *http.Request, _ Params) {
	out := []map[string]any{}
	runtimes, specs := h.slots()
	for _, rt := range runtimes {
		if key, source := specs[rt].Store.Key(); key != "" {
			out = append(out, credentialJSON(rt, specs[rt], key, source))
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"credentials": out})
}

// slot is the adapter and credential slot a /me/credentials/:runtime/:providerId path names.
func (h *Host) slot(w http.ResponseWriter, p Params) (codingagent.Adapter, *codingagent.CredentialSpec, bool) {
	if ad := h.adapter(p["runtime"]); ad != nil {
		if c := ad.Credential(); c != nil && c.Provider == p["providerId"] {
			return ad, c, true
		}
	}
	runtimes, specs := h.slots()
	labels := make([]string, 0, len(runtimes))
	for _, rt := range runtimes {
		labels = append(labels, specs[rt].Label)
	}
	WriteError(w, http.StatusNotFound, "Only the "+strings.Join(labels, ", ")+" key is kept by this app")
	return nil, nil, false
}

func (h *Host) putCredential(w http.ResponseWriter, r *http.Request, p Params) {
	ad, spec, ok := h.slot(w, p)
	if !ok {
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := readJSON(r, 8<<10, &body); err != nil || strings.TrimSpace(body.Key) == "" {
		WriteError(w, http.StatusBadRequest, spec.Missing)
		return
	}
	key := strings.TrimSpace(body.Key)
	if msg := spec.Validate(key); msg != "" {
		WriteError(w, http.StatusBadRequest, msg)
		return
	}
	if err := spec.Store.Store(key); err != nil {
		WriteError(w, http.StatusInternalServerError, "Could not save the key on the server: "+err.Error())
		return
	}
	stored, source := spec.Store.Key()
	out := map[string]any{"credential": credentialJSON(ad.Runtime(), spec, stored, source)}
	// Online check through the runtime. The key is kept either way: offline the check cannot decide, and a
	// rejected key is reported with what to do instead of being thrown away.
	if ad.Ready() == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		acc, err := ad.CheckCredential(ctx, key)
		cancel()
		switch {
		case err == nil:
			out["account"] = map[string]any{"email": acc.Email, "apiKeyName": acc.KeyName}
			h.logf("key saved and accepted by %s (%s)", spec.Label, spec.Describe(key, source))
		case ad.Unauthenticated(err):
			out["warning"] = spec.AuthHint(key, source) + " " + spec.Label + " said: " + err.Error()
			h.logf("key saved but rejected by %s: %v (%s)", spec.Label, err, spec.Describe(key, source))
		default:
			out["warning"] = "Saved, but " + spec.Label + " could not check the key now: " + err.Error()
			h.logf("key saved, not checked: %v (%s)", err, spec.Describe(key, source))
		}
	}
	WriteJSON(w, http.StatusOK, out)
}

// agentError is the user-facing text of an agent failure; a key the runtime rejected gets what to do about it.
func (h *Host) agentError(ad codingagent.Adapter, prefix string, err error) string {
	msg := prefix + err.Error()
	if spec := ad.Credential(); spec != nil && ad.Unauthenticated(err) {
		key, source := spec.Store.Key()
		h.logf("%s rejected the key: %v (%s)", ad.Runtime(), err, spec.Describe(key, source))
		msg += "\n\n" + spec.AuthHint(key, source)
	}
	return msg
}

func (h *Host) deleteCredential(w http.ResponseWriter, _ *http.Request, p Params) {
	_, spec, ok := h.slot(w, p)
	if !ok {
		return
	}
	if err := spec.Store.Forget(); err != nil {
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
