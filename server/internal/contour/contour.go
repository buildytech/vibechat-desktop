// Package contour is the server's jurisdiction contour (ADR 0002): which coding-agent runtime serves it and, for Pi,
// which model provider. EU and US servers run the Cursor SDK (no contour file needed); an RU server runs Pi with
// GigaChat or Yandex AI Studio. The config holds no secret: the provider keys come from the server's environment
// (VIBECHAT_YANDEX_API_KEY, VIBECHAT_GIGACHAT_AUTH_KEY) and are read per request by the host-side provider gateway.
package contour

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/buildytech/vibechat-desktop/server/internal/codingagent"
	"github.com/buildytech/vibechat-desktop/server/internal/codingagent/pi"
	"github.com/buildytech/vibechat-desktop/server/internal/providergw"
)

// Environment variables of the provider credentials and of the config file.
const (
	EnvYandexKey   = "VIBECHAT_YANDEX_API_KEY"
	EnvGigaChatKey = "VIBECHAT_GIGACHAT_AUTH_KEY"
	EnvFile        = "VIBECHAT_CONTOUR_FILE"
	EnvPiDir       = "VIBECHAT_PI_DIR"
	EnvPiNode      = "VIBECHAT_PI_NODE"
)

// Config is contour.json.
type Config struct {
	// Contour is a label for logs and doctor ("ru", "eu", "us").
	Contour string `json:"contour,omitempty"`
	// Runtime serves new chats: "cursor-sdk" (default) or "pi". Existing chats keep the runtime they started with.
	Runtime  string          `json:"runtime,omitempty"`
	Provider string          `json:"provider,omitempty"` // pi: "yandex", "gigachat" or "openai" (any OpenAI-compatible endpoint)
	Pi       PiConfig        `json:"pi,omitempty"`
	Yandex   *YandexConfig   `json:"yandex,omitempty"`
	GigaChat *GigaChatConfig `json:"gigachat,omitempty"`
	OpenAI   *OpenAIConfig   `json:"openai,omitempty"`
}

// OpenAIConfig is any OpenAI-compatible provider (gpt-oss-120b on a Russian cloud, a vLLM gateway, ...) described by
// data: a new provider is one block here, no code. The key is the environment variable named by keyEnv.
type OpenAIConfig struct {
	// ID is the provider id of the Studio's model menu ("gptoss" gives "gptoss/<model id>"): [a-z][a-z0-9-]*.
	ID    string `json:"id"`
	Label string `json:"label,omitempty"` // "gpt-oss-120b (Selectel)"
	// BaseURL is the endpoint up to /chat/completions: https://host/v1.
	BaseURL string `json:"baseUrl"`
	// KeyEnv names the environment variable with the API key: VIBECHAT_<NAME>_KEY. Empty with noKey: a keyless endpoint.
	KeyEnv string `json:"keyEnv,omitempty"`
	NoKey  bool   `json:"noKey,omitempty"`
	// Auth says how the key is presented (default "Authorization: Bearer <key>"; scheme "none" = the bare key).
	Auth *AuthConfig `json:"auth,omitempty"`
	// Headers are fixed extra headers (no secrets).
	Headers map[string]string `json:"headers,omitempty"`
	// Quirks are the provider's known deviations the gateway irons out.
	Quirks QuirkConfig `json:"quirks,omitempty"`
	// Compat overrides single keys of Pi's models.json compat block (supportsDeveloperRole, maxTokensField, ...).
	Compat map[string]any `json:"compat,omitempty"`
	// CAFile is an extra CA bundle (PEM) for a provider behind a private or Russian trusted root.
	CAFile string          `json:"caFile,omitempty"`
	Models []pi.ModelEntry `json:"models"`
}

// AuthConfig is the credential header of an OpenAI-compatible provider.
type AuthConfig struct {
	Header string `json:"header,omitempty"` // default Authorization
	Scheme string `json:"scheme,omitempty"` // default Bearer; Api-Key; none
}

// QuirkConfig lists the provider deviations the gateway handles.
type QuirkConfig struct {
	// UniqueToolCallIds: the provider answers tool calls with a non-unique id (Yandex: the function name); the gateway
	// makes them unique and maps them back.
	UniqueToolCallIds bool `json:"uniqueToolCallIds,omitempty"`
}

// PiConfig locates the runtime: the Node binary and the folder fetch-pi installed Pi into.
type PiConfig struct {
	Node string `json:"node,omitempty"` // "" = VIBECHAT_PI_NODE, else node on PATH
	Dir  string `json:"dir,omitempty"`  // "" = VIBECHAT_PI_DIR
}

// YandexConfig is Yandex AI Studio (OpenAI-compatible, pass-through).
type YandexConfig struct {
	// FolderID is the Yandex Cloud folder (sent as OpenAI-Project; "{folder}" in a model's upstream name).
	FolderID string `json:"folderId"`
	BaseURL  string `json:"baseUrl,omitempty"`
	// KeepLogging does not send x-data-logging-enabled: false (provider-side request logging stays on).
	KeepLogging bool            `json:"keepLogging,omitempty"`
	Models      []pi.ModelEntry `json:"models"`
}

// GigaChatConfig is GigaChat (translated: tools <-> functions).
type GigaChatConfig struct {
	Scope    string          `json:"scope"`            // GIGACHAT_API_PERS, GIGACHAT_API_B2B, GIGACHAT_API_CORP
	CAFile   string          `json:"caFile,omitempty"` // the Russian trusted root CA bundle (PEM)
	BaseURL  string          `json:"baseUrl,omitempty"`
	OAuthURL string          `json:"oauthUrl,omitempty"`
	Models   []pi.ModelEntry `json:"models"`
}

var (
	folderPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	providerID    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	keyEnvName    = regexp.MustCompile(`^VIBECHAT_[A-Z0-9_]{1,48}_KEY$`)
	scopes        = map[string]bool{"GIGACHAT_API_PERS": true, "GIGACHAT_API_B2B": true, "GIGACHAT_API_CORP": true}
)

// Load reads the contour file. A missing file is not an error: it is the default contour (Cursor SDK, as before).
// The file is strict: unknown fields (a key typed into it, a typo) are errors, never ignored.
func Load(path string) (Config, error) {
	if path == "" {
		return Config{}, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	return Parse(raw, path)
}

// Parse validates a contour document (name is for messages).
func Parse(raw []byte, name string) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %v (the file holds no keys; provider keys are environment variables)", name, err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %v", name, err)
	}
	return c, nil
}

// RuntimeID is the runtime of new chats.
func (c Config) RuntimeID() string {
	if c.Runtime == "" {
		return codingagent.RuntimeCursorSDK
	}
	return c.Runtime
}

// UsesPi reports whether this contour runs Pi.
func (c Config) UsesPi() bool { return c.RuntimeID() == codingagent.RuntimePi }

// Validate checks the config; it never touches the environment or the disk.
func (c Config) Validate() error {
	switch c.RuntimeID() {
	case codingagent.RuntimeCursorSDK:
		if c.Provider != "" || c.Yandex != nil || c.GigaChat != nil || c.OpenAI != nil {
			return errors.New(`runtime "cursor-sdk" takes no provider: remove "provider", "yandex", "gigachat" and "openai", or set "runtime": "pi"`)
		}
		return nil
	case codingagent.RuntimePi:
	default:
		return fmt.Errorf(`unknown runtime %q (cursor-sdk or pi)`, c.Runtime)
	}
	switch c.Provider {
	case "yandex":
		y := c.Yandex
		if y == nil {
			return errors.New(`provider "yandex" needs a "yandex" block`)
		}
		if c.GigaChat != nil || c.OpenAI != nil {
			return errors.New(`one provider only: remove the other provider blocks (egress is opened for the chosen provider alone)`)
		}
		if !folderPattern.MatchString(y.FolderID) {
			return errors.New(`yandex.folderId is the Yandex Cloud folder id (letters, digits, - and _)`)
		}
		if err := checkURL("yandex.baseUrl", y.BaseURL); err != nil {
			return err
		}
		return pi.ValidateModels(y.Models)
	case "gigachat":
		g := c.GigaChat
		if g == nil {
			return errors.New(`provider "gigachat" needs a "gigachat" block`)
		}
		if c.Yandex != nil || c.OpenAI != nil {
			return errors.New(`one provider only: remove the other provider blocks (egress is opened for the chosen provider alone)`)
		}
		if !scopes[g.Scope] {
			return errors.New(`gigachat.scope must be GIGACHAT_API_PERS, GIGACHAT_API_B2B or GIGACHAT_API_CORP`)
		}
		if err := checkURL("gigachat.baseUrl", g.BaseURL); err != nil {
			return err
		}
		if err := checkURL("gigachat.oauthUrl", g.OAuthURL); err != nil {
			return err
		}
		return pi.ValidateModels(g.Models)
	case "openai":
		o := c.OpenAI
		if o == nil {
			return errors.New(`provider "openai" needs an "openai" block`)
		}
		if c.Yandex != nil || c.GigaChat != nil {
			return errors.New(`one provider only: remove the other provider blocks (egress is opened for the chosen provider alone)`)
		}
		return o.validate()
	case "":
		return errors.New(`runtime "pi" needs "provider": "yandex", "gigachat" or "openai"`)
	}
	return fmt.Errorf(`unknown provider %q (yandex, gigachat or openai)`, c.Provider)
}

func (o *OpenAIConfig) validate() error {
	if !providerID.MatchString(o.ID) {
		return errors.New(`openai.id: lower-case letters, digits and '-', starting with a letter`)
	}
	if o.BaseURL == "" {
		return errors.New(`openai.baseUrl is required (https://host/v1)`)
	}
	if err := checkURL("openai.baseUrl", o.BaseURL); err != nil {
		return err
	}
	switch {
	case o.NoKey && o.KeyEnv != "":
		return errors.New(`openai: "noKey" and "keyEnv" exclude each other`)
	case !o.NoKey && !keyEnvName.MatchString(o.KeyEnv):
		return errors.New(`openai.keyEnv names the environment variable of the key and must look like VIBECHAT_<NAME>_KEY`)
	}
	if o.Auth != nil {
		if err := providergw.ValidateAuth(o.Auth.Header, o.Auth.Scheme); err != nil {
			return fmt.Errorf("openai.auth: %v", err)
		}
		if o.NoKey {
			return errors.New(`openai: "auth" makes no sense with "noKey"`)
		}
	}
	if err := providergw.ValidateHeaders(o.Headers); err != nil {
		return fmt.Errorf("openai.headers: %v", err)
	}
	if err := pi.ValidateCompat(o.Compat); err != nil {
		return fmt.Errorf("openai.compat: %v", err)
	}
	return pi.ValidateModels(o.Models)
}

// ProviderName is the provider id of the model menu: the preset's name, or the id of an "openai" block.
func (c Config) ProviderName() string {
	if c.Provider == "openai" && c.OpenAI != nil {
		return c.OpenAI.ID
	}
	return c.Provider
}

// Compat is the models.json compat overrides of the provider.
func (c Config) Compat() map[string]any {
	if c.Provider == "openai" && c.OpenAI != nil {
		return c.OpenAI.Compat
	}
	return nil
}

// checkURL: an override must be https (a loopback http URL is allowed for tests and local stubs).
func checkURL(field, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s: not a URL", field)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost") {
		return nil
	}
	return fmt.Errorf("%s must be https", field)
}

// Env is what Build needs from the process.
type Env struct {
	Getenv   func(string) string
	LookPath pi.LookPath
	DataDir  string
	Log      func(string)
}

// Provider builds the chosen provider with its credential read from the environment per call (nothing is stored).
func (c Config) BuildProvider(env Env) (providergw.Provider, []pi.ModelEntry, error) {
	getenv := env.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	switch c.Provider {
	case "yandex":
		y := c.Yandex
		client, err := providergw.NewClient(hostsOf(&providergw.Yandex{BaseURL: y.BaseURL}), "")
		if err != nil {
			return nil, nil, err
		}
		prov := &providergw.Yandex{BaseURL: y.BaseURL, FolderID: y.FolderID, KeepLogging: y.KeepLogging, Client: client,
			Key: func() string { return strings.TrimSpace(getenv(EnvYandexKey)) }}
		models, ids := resolve(y.Models, y.FolderID)
		return providergw.WithModels(prov, ids), models, nil
	case "gigachat":
		g := c.GigaChat
		prov0 := &providergw.GigaChat{BaseURL: g.BaseURL, Tokens: &providergw.GigaTokens{URL: g.OAuthURL}}
		client, err := providergw.NewClient(prov0.Hosts(), g.CAFile)
		if err != nil {
			return nil, nil, err
		}
		tokens := &providergw.GigaTokens{Scope: g.Scope, URL: g.OAuthURL, Client: client,
			AuthKey: func() string { return strings.TrimSpace(getenv(EnvGigaChatKey)) }}
		prov := &providergw.GigaChat{BaseURL: g.BaseURL, Tokens: tokens, Client: client}
		models, ids := resolve(g.Models, "")
		return providergw.WithModels(prov, ids), models, nil
	case "openai":
		o := c.OpenAI
		client, err := providergw.NewClient((&providergw.OpenAICompat{BaseURL: o.BaseURL}).Hosts(), o.CAFile)
		if err != nil {
			return nil, nil, err
		}
		prov := &providergw.OpenAICompat{ID: o.ID, BaseURL: o.BaseURL, NoKey: o.NoKey, Headers: o.Headers,
			UniqueToolCallIDs: o.Quirks.UniqueToolCallIds, Client: client,
			Key: func() string { return strings.TrimSpace(getenv(o.KeyEnv)) }}
		if o.Auth != nil {
			prov.AuthHeader, prov.AuthScheme = o.Auth.Header, o.Auth.Scheme
		}
		models, ids := resolve(o.Models, "")
		return providergw.WithModels(prov, ids), models, nil
	}
	return nil, nil, fmt.Errorf("unknown provider %q", c.Provider)
}

func hostsOf(p providergw.Provider) []string { return p.Hosts() }

// resolve fills the upstream names: "" = the id, "{folder}" = the Yandex folder.
func resolve(ms []pi.ModelEntry, folder string) ([]pi.ModelEntry, map[string]string) {
	out := make([]pi.ModelEntry, len(ms))
	ids := make(map[string]string, len(ms))
	for i, m := range ms {
		up := m.Upstream
		if up == "" {
			up = m.ID
		}
		up = strings.ReplaceAll(up, "{folder}", folder)
		m.Upstream = up
		out[i] = m
		ids[m.ID] = up
	}
	return out, ids
}

// EgressHosts are the hostnames the contour's agent traffic may reach: the chosen provider's, nothing else. The server
// opens exactly these in its network policy (`vibechat-server egress-domains`).
func (c Config) EgressHosts() ([]string, error) {
	if !c.UsesPi() {
		return nil, nil
	}
	prov, _, err := c.BuildProvider(Env{Getenv: func(string) string { return "" }})
	if err != nil {
		return nil, err
	}
	return prov.Hosts(), nil
}

// Build is the Pi adapter of this contour. A broken config is an error (the server does not start); a missing Node,
// Pi, bwrap or key is not: the adapter reports it through Ready and the agent stays off.
func Build(c Config, env Env) (*pi.Adapter, error) {
	if !c.UsesPi() {
		return nil, errors.New("this contour does not run Pi")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	getenv := env.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	prov, models, err := c.BuildProvider(env)
	if err != nil {
		return nil, err
	}
	node, dir := c.Pi.Node, c.Pi.Dir
	if node == "" {
		node = getenv(EnvPiNode)
	}
	if dir == "" {
		dir = getenv(EnvPiDir)
	}
	install, locErr := pi.Locate(pi.PinnedLock(), node, dir, env.LookPath)
	label := "Pi · " + map[string]string{"yandex": "Yandex AI Studio", "gigachat": "GigaChat"}[c.Provider]
	if c.Provider == "openai" {
		label = "Pi · " + firstNonEmpty(c.OpenAI.Label, c.OpenAI.ID)
	}
	return pi.New(pi.Options{
		Label: label, Provider: c.ProviderName(), Compat: c.Compat(), DataDir: env.DataDir, Install: install, LocateErr: locErr,
		Gateway: providergw.New(prov, env.Log), Models: models, Log: env.Log,
	}), nil
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
