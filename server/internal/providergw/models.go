package providergw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// mapped rewrites the "model" of a request from the agent's model id (a short safe name from the contour config) to the
// provider's own model name (Yandex: gpt://<folder>/yandexgpt/rc), and refuses a model that is not in the config, so a
// compromised agent cannot pick another model of the same account.
type mapped struct {
	Provider
	models map[string]string
}

// WithModels wraps p: ids maps the agent's model id to the provider's model name.
func WithModels(p Provider, ids map[string]string) Provider { return &mapped{Provider: p, models: ids} }

func (m *mapped) Ready() error {
	if len(m.models) == 0 {
		return errors.New("no models are configured for this provider in the contour config")
	}
	return m.Provider.Ready()
}

func (m *mapped) Chat(ctx context.Context, body []byte, w http.ResponseWriter) error {
	var req map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "the request is not a JSON object")
		return nil
	}
	var id string
	_ = json.Unmarshal(req["model"], &id)
	upstream, ok := m.models[id]
	if !ok {
		WriteError(w, http.StatusBadRequest, fmt.Sprintf("model %q is not configured for this server", id))
		return nil
	}
	req["model"], _ = json.Marshal(upstream)
	out, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return m.Provider.Chat(ctx, out, w)
}
