package providergw

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// OpenAICompat is any OpenAI Chat Completions endpoint, described by data: where it is, how the credential is
// presented, which fixed headers it wants and which of its quirks the gateway irons out. A new provider of this kind
// is one entry of the contour config, no code. Yandex AI Studio is a preset of it.
type OpenAICompat struct {
	// ID is the provider id of the contour ("yandex", "gptoss", ...).
	ID      string
	BaseURL string // https://host/v1 (the path before /chat/completions)
	// Key returns the API key (from the server's environment); called per request, never stored.
	Key func() string
	// AuthHeader and AuthScheme say how the key is sent: "<AuthHeader>: <AuthScheme> <key>". Defaults: Authorization
	// and Bearer; AuthScheme "none" sends the bare key (x-api-key style).
	AuthHeader string
	AuthScheme string
	// Headers are fixed extra headers (no secrets: the config file holds none).
	Headers map[string]string
	// UniqueToolCallIDs makes tool-call ids unique (for providers that answer with the function name as the id) and
	// maps them back on the next request.
	UniqueToolCallIDs bool
	// NoKey lets a keyless endpoint (an internal gateway) work.
	NoKey  bool
	Client *http.Client
}

var headerName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,63}$`)

// reserved headers belong to the gateway, not to the config.
var reservedHeaders = map[string]bool{"authorization": true, "host": true, "content-length": true, "content-type": true,
	"cookie": true, "set-cookie": true, "transfer-encoding": true, "connection": true, "proxy-authorization": true,
	"x-forwarded-for": true, "x-forwarded-host": true, "x-real-ip": true}

// ValidateHeaders checks the fixed headers of a provider entry.
func ValidateHeaders(h map[string]string) error {
	for k, v := range h {
		if !headerName.MatchString(k) {
			return fmt.Errorf("header %q: not a plain header name", k)
		}
		if reservedHeaders[strings.ToLower(k)] {
			return fmt.Errorf("header %q is set by the gateway (the credential goes in auth, not in headers)", k)
		}
		if strings.ContainsAny(v, "\r\n\x00") || len(v) > 512 {
			return fmt.Errorf("header %q: bad value", k)
		}
	}
	return nil
}

// ValidateAuth checks the auth header name and scheme.
func ValidateAuth(header, scheme string) error {
	if header != "" && !headerName.MatchString(header) {
		return fmt.Errorf("auth header %q: not a plain header name", header)
	}
	if scheme != "" && scheme != "none" && !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,31}$`).MatchString(scheme) {
		return fmt.Errorf("auth scheme %q: use a word such as Bearer or Api-Key, or none", scheme)
	}
	return nil
}

func (o *OpenAICompat) base() string { return strings.TrimRight(o.BaseURL, "/") }

func (o *OpenAICompat) Name() string { return o.ID }

func (o *OpenAICompat) Hosts() []string {
	if u, err := url.Parse(o.base()); err == nil && u.Hostname() != "" {
		return []string{u.Hostname()}
	}
	return nil
}

func (o *OpenAICompat) Ready() error {
	if o.BaseURL == "" {
		return errors.New("the provider's base URL is not set in the contour config")
	}
	if !o.NoKey && (o.Key == nil || o.Key() == "") {
		return fmt.Errorf("the %s API key is not set in the server environment", o.ID)
	}
	return nil
}

func (o *OpenAICompat) Chat(ctx context.Context, body []byte, w http.ResponseWriter) error {
	if o.UniqueToolCallIDs {
		var err error
		if body, err = RestoreToolIDs(body); err != nil {
			WriteError(w, http.StatusBadRequest, "the request is not a JSON object")
			return nil
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base()+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range o.Headers {
		req.Header.Set(k, v)
	}
	if !o.NoKey {
		h, scheme := o.AuthHeader, o.AuthScheme
		if h == "" {
			h = "Authorization"
		}
		switch scheme {
		case "":
			scheme = "Bearer "
		case "none":
			scheme = ""
		default:
			scheme += " "
		}
		req.Header.Set(h, scheme+o.Key())
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if o.UniqueToolCallIDs {
		return forwardAnswer(w, resp)
	}
	return passAnswer(w, resp)
}

// passAnswer writes the provider's answer through unchanged.
func passAnswer(w http.ResponseWriter, resp *http.Response) error {
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	return copyStream(w, resp.Body)
}
