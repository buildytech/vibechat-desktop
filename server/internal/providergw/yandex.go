package providergw

import (
	"context"
	"errors"
	"net/http"
)

// YandexBaseURL is the OpenAI-compatible endpoint of Yandex AI Studio.
const YandexBaseURL = "https://ai.api.cloud.yandex.net/v1"

// Yandex is Yandex AI Studio: OpenAI Chat Completions, so the request and the answer pass through and only the
// credential is added (Authorization: Api-Key, the folder in OpenAI-Project, provider-side logging off). It is a preset
// of OpenAICompat with the quirk Yandex has: tool-call ids that are the function name (made unique by the gateway).
type Yandex struct {
	BaseURL  string // "" = YandexBaseURL
	FolderID string
	// Key returns the API key (from the server's environment); it is called per request and never stored.
	Key    func() string
	Client *http.Client
	// KeepLogging leaves the provider's request logging on (x-data-logging-enabled is not sent as false).
	KeepLogging bool
}

func (y *Yandex) compat() *OpenAICompat {
	base := y.BaseURL
	if base == "" {
		base = YandexBaseURL
	}
	h := map[string]string{"OpenAI-Project": y.FolderID}
	if !y.KeepLogging {
		h["x-data-logging-enabled"] = "false"
	}
	return &OpenAICompat{ID: "yandex", BaseURL: base, Key: y.Key, AuthScheme: "Api-Key", Headers: h, UniqueToolCallIDs: true, Client: y.Client}
}

func (y *Yandex) Name() string    { return "yandex" }
func (y *Yandex) Hosts() []string { return y.compat().Hosts() }

func (y *Yandex) Ready() error {
	if err := y.compat().Ready(); err != nil {
		return errors.New("the Yandex AI Studio API key is not set in the server environment")
	}
	if y.FolderID == "" {
		return errors.New("the Yandex Cloud folder id is not set in the contour config")
	}
	return nil
}

func (y *Yandex) Chat(ctx context.Context, body []byte, w http.ResponseWriter) error {
	return y.compat().Chat(ctx, body, w)
}
