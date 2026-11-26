package host

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	vibechat "github.com/buildytech/vibechat-desktop"
)

func pinnedSeam(t *testing.T) Seam {
	t.Helper()
	s, err := LoadSeam(vibechat.HostSeam())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestHostSeamContract runs every route of the pinned studio host-seam.json against the host: each answers JSON
// with { error } or a body, never a blank page or an HTML fallback.
func TestHostSeamContract(t *testing.T) {
	s := pinnedSeam(t)
	h := testHost(t, s)
	for _, r := range append(append([]SeamRoute{}, s.Routes...), s.Remote...) {
		r := r
		t.Run(r.Key(), func(t *testing.T) {
			if r.SSE {
				t.Skip("SSE stream: covered by the agent tests")
			}
			path := strings.NewReplacer(":id", "nope", ":sid", "nope", ":rid", "nope", ":runtime", "cursor-sdk", ":providerId", "cursor").Replace(r.Path)
			req := httptest.NewRequest(r.Method, s.Prefix+path, strings.NewReader("{}"))
			req.Header.Set("content-type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if !strings.HasPrefix(rec.Header().Get("content-type"), "application/json") && rec.Code != http.StatusNoContent {
				t.Fatalf("%d with content-type %q", rec.Code, rec.Header().Get("content-type"))
			}
			if rec.Code >= 400 {
				var body map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == "" {
					t.Fatalf("%d without { error }: %q", rec.Code, rec.Body.String())
				}
			}
		})
	}
}

func TestCapabilitiesCarryEverySeamField(t *testing.T) {
	s := pinnedSeam(t)
	h := testHost(t, s)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, s.Prefix+"/host/capabilities", nil))
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, f := range s.Capabilities {
		if _, ok := got[f]; !ok {
			t.Errorf("capabilities miss %q", f)
		}
	}
	if got["seam"] != s.Seam {
		t.Errorf("seam %v, want %s", got["seam"], s.Seam)
	}
}

func TestUnknownPathIs404JSON(t *testing.T) {
	s := pinnedSeam(t)
	h := testHost(t, s)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, s.Prefix+"/auth/login", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

func TestMiddlewareSplitsSeamFromStudio(t *testing.T) {
	studio := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("studio")) })
	seam := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("seam")) })
	h := Middleware("/runtime/v1", seam)(studio)
	for path, want := range map[string]string{"/": "studio", "/p/x": "studio", "/runtime/v1/projects": "seam", "/runtime/v10": "studio"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Body.String() != want {
			t.Errorf("%s → %q", path, rec.Body.String())
		}
	}
}

func TestHelloCarriesEverySeamField(t *testing.T) {
	s := pinnedSeam(t)
	if s.Protocol < 1 || s.MinClientProtocol < 1 || len(s.Remote) == 0 || s.Auth.SessionPath == "" {
		t.Fatalf("pinned seam has no remote part: %+v", s)
	}
	h := testHost(t, s)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, s.Prefix+"/host/hello", nil))
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
		t.Fatalf("%d %v", rec.Code, err)
	}
	for _, f := range s.Hello {
		if f == "previewPort" { // optional: only a host with a preview gateway has one
			continue
		}
		if _, ok := got[f]; !ok {
			t.Errorf("hello misses %q", f)
		}
	}
	if got["protocol"] != float64(s.Protocol) {
		t.Errorf("protocol %v", got["protocol"])
	}
}

func TestTicketIsOffInTheDesktopApp(t *testing.T) {
	s := pinnedSeam(t)
	h := testHost(t, s)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, s.Prefix+"/auth/ticket", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}
