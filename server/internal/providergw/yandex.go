package providergw

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// YandexBaseURL is the OpenAI-compatible endpoint of Yandex AI Studio.
const YandexBaseURL = "https://ai.api.cloud.yandex.net/v1"

// Yandex is Yandex AI Studio: OpenAI Chat Completions, so the request and the answer pass through unchanged and only
// the credential is added (Authorization: Api-Key, the folder in OpenAI-Project, provider-side logging off).
type Yandex struct {
	BaseURL  string // "" = YandexBaseURL
	FolderID string
	// Key returns the API key (from the server's environment); it is called per request and never stored.
	Key    func() string
	Client *http.Client
	// KeepLogging leaves the provider's request logging on (x-data-logging-enabled is not sent as false).
	KeepLogging bool
}

func (y *Yandex) base() string {
	if y.BaseURL != "" {
		return strings.TrimRight(y.BaseURL, "/")
	}
	return YandexBaseURL
}

func (y *Yandex) Name() string { return "yandex" }

func (y *Yandex) Hosts() []string {
	if u, err := url.Parse(y.base()); err == nil && u.Hostname() != "" {
		return []string{u.Hostname()}
	}
	return []string{"ai.api.cloud.yandex.net"}
}

func (y *Yandex) Ready() error {
	if y.Key == nil || y.Key() == "" {
		return errors.New("the Yandex AI Studio API key is not set in the server environment")
	}
	if y.FolderID == "" {
		return errors.New("the Yandex Cloud folder id is not set in the contour config")
	}
	return nil
}

func (y *Yandex) Chat(ctx context.Context, body []byte, w http.ResponseWriter) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, y.base()+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Api-Key "+y.Key())
	req.Header.Set("OpenAI-Project", y.FolderID)
	if !y.KeepLogging {
		req.Header.Set("x-data-logging-enabled", "false")
	}
	resp, err := y.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	return copyStream(w, resp.Body)
}
