package providergw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Part is one provider of a Route and the models it serves (the agent's model id -> the provider's model name).
type Part struct {
	Provider Provider
	Models   map[string]string
}

// router sends each request to the provider that owns its model, so one contour can offer models of several providers
// (Cloud.ru first, OpenRouter second) behind the one gateway of a run. Every part keeps its own credential, headers and
// quirks; the egress hosts of the contour are the union of the parts'.
type router struct {
	parts  []Part
	mapped []Provider
	owner  map[string]int
}

// Route builds the provider of a contour with several providers. Model ids must be unique across the parts.
func Route(parts ...Part) (Provider, error) {
	r := &router{parts: parts, owner: map[string]int{}}
	for i, p := range parts {
		for id := range p.Models {
			if j, dup := r.owner[id]; dup {
				return nil, fmt.Errorf("model id %q is served by both %s and %s", id, parts[j].Provider.Name(), p.Provider.Name())
			}
			r.owner[id] = i
		}
		r.mapped = append(r.mapped, WithModels(p.Provider, p.Models))
	}
	if len(parts) == 0 {
		return nil, errors.New("no providers")
	}
	return r, nil
}

func (r *router) Name() string {
	names := make([]string, len(r.parts))
	for i, p := range r.parts {
		names[i] = p.Provider.Name()
	}
	return strings.Join(names, "+")
}

func (r *router) Hosts() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range r.parts {
		for _, h := range p.Provider.Hosts() {
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Ready is nil when at least one provider can answer; a request for a model of a provider that cannot is refused on its
// own (see Chat), so one missing key does not switch the other models off.
func (r *router) Ready() error {
	var first error
	for _, m := range r.mapped {
		err := m.Ready()
		if err == nil {
			return nil
		}
		if first == nil {
			first = err
		}
	}
	return first
}

func (r *router) Chat(ctx context.Context, body []byte, w http.ResponseWriter) error {
	var req struct {
		Model string `json:"model"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "the request is not a JSON object")
		return nil
	}
	i, ok := r.owner[req.Model]
	if !ok {
		WriteError(w, http.StatusBadRequest, fmt.Sprintf("model %q is not configured for this server", req.Model))
		return nil
	}
	if err := r.mapped[i].Ready(); err != nil {
		WriteError(w, http.StatusServiceUnavailable, err.Error())
		return nil
	}
	return r.mapped[i].Chat(ctx, body, w)
}

// Parts are the providers of a Route, for the doctor (nil for a single provider).
func Parts(p Provider) []Part {
	if r, ok := p.(*router); ok {
		return r.parts
	}
	return nil
}
