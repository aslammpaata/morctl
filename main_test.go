package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newMockRouter starts an httptest server dispatching on "METHOD /path" and
// returns a client pointed at it. Every router interaction in these tests goes
// through here -- no test touches a real proxy-router, so no test can spend MOR.
func newMockRouter(t *testing.T, routes map[string]http.HandlerFunc) (*client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := routes[r.Method+" "+r.URL.Path]; ok {
			h(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no mock route for ` + r.Method + " " + r.URL.Path + `"}`))
	}))
	t.Cleanup(srv.Close)
	return &client{
		http: &http.Client{Timeout: 5 * time.Second},
		base: srv.URL,
		user: "admin",
		pass: "test-pass",
	}, srv
}

func jsonRoute(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func statusRoute(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

func TestWeiToFloat(t *testing.T) {
	cases := []struct {
		name, wei string
		want      float64
		wantErr   bool
	}{
		{name: "one token", wei: "1000000000000000000", want: 1},
		{name: "five tokens", wei: "5000000000000000000", want: 5},
		{name: "zero", wei: "0", want: 0},
		{name: "sub-token", wei: "399745950320894", want: 0.000399745950320894},
		// Beyond float64's exact range: the integer part must still survive,
		// which is why this path uses math/big rather than a plain conversion.
		{name: "huge", wei: "123456789012345678901234567890", want: 123456789012.345678901234567890},
		{name: "not a number", wei: "abc", wantErr: true},
		{name: "empty", wei: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := weiToFloat(tc.wei)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("weiToFloat(%q): want error, got %v", tc.wei, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("weiToFloat(%q): unexpected error %v", tc.wei, err)
			}
			f, _ := got.Float64()
			tolerance := tc.want * 1e-9
			if tolerance < 0 {
				tolerance = -tolerance
			}
			if diff := f - tc.want; diff > tolerance || diff < -tolerance {
				t.Errorf("weiToFloat(%q) = %v, want %v", tc.wei, f, tc.want)
			}
		})
	}
}

func TestFormatWei(t *testing.T) {
	cases := []struct{ wei, want string }{
		{"1000000000000000000", "1.0000"},
		{"5000000000000000000", "5.0000"},
		{"18944200000000000000", "18.9442"},
		{"0", "0.0000"},
		// Unparseable input is echoed rather than erroring the whole run.
		{"not-a-number", "not-a-number"},
	}
	for _, tc := range cases {
		if got := formatWei(tc.wei); got != tc.want {
			t.Errorf("formatWei(%q) = %q, want %q", tc.wei, got, tc.want)
		}
	}
}

func TestRemaining(t *testing.T) {
	if got := remaining(time.Now().Add(-time.Minute)); got != "expired" {
		t.Errorf("past time: got %q, want %q", got, "expired")
	}
	if got := remaining(time.Now().Add(9*time.Minute + 57*time.Second)); got != "9m57s" {
		t.Errorf("future time: got %q, want %q", got, "9m57s")
	}
	if got := remaining(time.Now().Add(30*time.Second + 500*time.Millisecond)); got != "0m30s" {
		t.Errorf("sub-minute: got %q, want %q", got, "0m30s")
	}
}

func TestShort(t *testing.T) {
	if got := short("0x4c8e4022aabbccdd"); got != "0x4c8e4022..." {
		t.Errorf("long id: got %q", got)
	}
	if got := short("0xabc"); got != "0xabc" {
		t.Errorf("short id should pass through: got %q", got)
	}
}

func TestBaseURL(t *testing.T) {
	cases := []struct{ host, want string }{
		{"localhost:8082", "http://localhost:8082"},
		{"http://localhost:8082", "http://localhost:8082"},
		{"https://router.example:443", "https://router.example:443"},
		{"localhost:8082/", "http://localhost:8082"},
	}
	for _, tc := range cases {
		if got := (config{host: tc.host}).baseURL(); got != tc.want {
			t.Errorf("baseURL(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func TestSessionCacheValid(t *testing.T) {
	const model = "0xmodel"
	now := time.Now()
	cases := []struct {
		name  string
		cache sessionCache
		want  bool
	}{
		{
			name:  "live session for this model",
			cache: sessionCache{SessionID: "0xsess", ModelID: model, OpenedAt: now.Unix(), Duration: 600},
			want:  true,
		},
		{
			name:  "different model is not reusable",
			cache: sessionCache{SessionID: "0xsess", ModelID: "0xother", OpenedAt: now.Unix(), Duration: 600},
			want:  false,
		},
		{
			name:  "expired session",
			cache: sessionCache{SessionID: "0xsess", ModelID: model, OpenedAt: now.Add(-20 * time.Minute).Unix(), Duration: 600},
			want:  false,
		},
		{
			name:  "inside the safety margin counts as unusable",
			cache: sessionCache{SessionID: "0xsess", ModelID: model, OpenedAt: now.Add(-599 * time.Second).Unix(), Duration: 600},
			want:  false,
		},
		{
			name:  "empty session id",
			cache: sessionCache{SessionID: "", ModelID: model, OpenedAt: now.Unix(), Duration: 600},
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cache.valid(model); got != tc.want {
				t.Errorf("valid() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoadAuth(t *testing.T) {
	t.Run("env var wins over cookie file", func(t *testing.T) {
		cookie := filepath.Join(t.TempDir(), ".cookie")
		if err := os.WriteFile(cookie, []byte("admin:from-file"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MORPHEUS_ROUTER_AUTH", "envuser:from-env")
		u, p, err := loadAuth(config{cookie: cookie})
		if err != nil {
			t.Fatal(err)
		}
		if u != "envuser" || p != "from-env" {
			t.Errorf("got %q/%q, want envuser/from-env", u, p)
		}
	})

	t.Run("falls back to the cookie file", func(t *testing.T) {
		cookie := filepath.Join(t.TempDir(), ".cookie")
		// A trailing newline is normal in a router-written cookie file.
		if err := os.WriteFile(cookie, []byte("admin:secret-value\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MORPHEUS_ROUTER_AUTH", "")
		u, p, err := loadAuth(config{cookie: cookie})
		if err != nil {
			t.Fatal(err)
		}
		if u != "admin" || p != "secret-value" {
			t.Errorf("got %q/%q, want admin/secret-value", u, p)
		}
	})

	t.Run("password may contain colons", func(t *testing.T) {
		cookie := filepath.Join(t.TempDir(), ".cookie")
		if err := os.WriteFile(cookie, []byte("admin:a:b:c"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MORPHEUS_ROUTER_AUTH", "")
		_, p, err := loadAuth(config{cookie: cookie})
		if err != nil {
			t.Fatal(err)
		}
		if p != "a:b:c" {
			t.Errorf("password = %q, want %q", p, "a:b:c")
		}
	})

	t.Run("missing cookie file is an actionable error", func(t *testing.T) {
		t.Setenv("MORPHEUS_ROUTER_AUTH", "")
		_, _, err := loadAuth(config{cookie: filepath.Join(t.TempDir(), "nope")})
		if err == nil {
			t.Fatal("want an error for a missing cookie file")
		}
		if !strings.Contains(err.Error(), "MORPHEUS_ROUTER_AUTH") || !strings.Contains(err.Error(), "--cookie") {
			t.Errorf("error should name both credential sources, got: %v", err)
		}
	})

	t.Run("malformed credentials are rejected", func(t *testing.T) {
		cookie := filepath.Join(t.TempDir(), ".cookie")
		if err := os.WriteFile(cookie, []byte("no-colon-here"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MORPHEUS_ROUTER_AUTH", "")
		if _, _, err := loadAuth(config{cookie: cookie}); err == nil {
			t.Fatal("want an error for credentials without a colon")
		}
	})

	t.Run("error never leaks the credential value", func(t *testing.T) {
		t.Setenv("MORPHEUS_ROUTER_AUTH", "malformed-no-colon-but-secret")
		_, _, err := loadAuth(config{})
		if err == nil {
			t.Fatal("want an error")
		}
		if strings.Contains(err.Error(), "malformed-no-colon-but-secret") {
			t.Errorf("error leaked the credential value: %v", err)
		}
	})
}

// modelsRoute serves a small stand-in marketplace, including a deliberately
// duplicated name and a deleted entry.
func modelsRoute() http.HandlerFunc {
	return jsonRoute(`{"models":[
		{"Id":"0x1111111111111111111111111111111111111111111111111111111111111111","Name":"kimi-k2.5","ModelType":"LLM","IsDeleted":false},
		{"Id":"0x2222222222222222222222222222222222222222222222222222222222222222","Name":"glm-5.3-flash","ModelType":"LLM","IsDeleted":false},
		{"Id":"0x3333333333333333333333333333333333333333333333333333333333333333","Name":"deepseek-v4-flash","ModelType":"LLM","IsDeleted":false},
		{"Id":"0x4444444444444444444444444444444444444444444444444444444444444444","Name":"deepseek-v4-flash","ModelType":"LLM","IsDeleted":false},
		{"Id":"0x5555555555555555555555555555555555555555555555555555555555555555","Name":"retired-model","ModelType":"LLM","IsDeleted":true}
	]}`)
}

func TestLooksLikeModelID(t *testing.T) {
	valid := "0x" + strings.Repeat("a", 64)
	cases := []struct {
		in   string
		want bool
	}{
		{valid, true},
		{"0x" + strings.Repeat("A", 64), true},
		{"0x2b5b7c295de206820e042221b83a25324d57fd2c757a92594c465c4b1976b7e4", true},
		{"", false},
		{"kimi-k2.5", false},
		{strings.Repeat("a", 66), false},        // no 0x prefix
		{"0x" + strings.Repeat("a", 63), false}, // too short
		{"0x" + strings.Repeat("a", 65), false}, // too long
		{"0x" + strings.Repeat("g", 64), false}, // not hex
		{"0X" + strings.Repeat("a", 64), false}, // wrong prefix case
	}
	for _, tc := range cases {
		if got := looksLikeModelID(tc.in); got != tc.want {
			t.Errorf("looksLikeModelID(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestResolveModelRequiresExplicitChoice is the core of the no-silent-default
// change: with no model flags at all, nothing is opened and nothing is staked.
func TestResolveModelRequiresExplicitChoice(t *testing.T) {
	c, _ := newMockRouter(t, map[string]http.HandlerFunc{
		"GET /blockchain/models": func(w http.ResponseWriter, r *http.Request) {
			t.Error("resolveModel must not hit the marketplace when no model was named")
		},
	})
	_, _, _, err := resolveModel(c, config{})
	if err == nil {
		t.Fatal("want an error when neither --model nor --model-id is given")
	}
	for _, want := range []string{"--model-id", "--model", "--list-models", "stakes MOR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got:\n%v", want, err)
		}
	}
}

func TestResolveModelFromModelID(t *testing.T) {
	id := "0x1111111111111111111111111111111111111111111111111111111111111111"

	t.Run("name is looked up for the label", func(t *testing.T) {
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /blockchain/models": modelsRoute(),
		})
		model, modelID, why, err := resolveModel(c, config{modelID: id})
		if err != nil {
			t.Fatal(err)
		}
		if modelID != id {
			t.Errorf("modelID = %q, want %q", modelID, id)
		}
		if model != "kimi-k2.5" {
			t.Errorf("model label = %q, want kimi-k2.5", model)
		}
		if why == "" {
			t.Error("resolveModel must explain where the choice came from")
		}
	})

	t.Run("an unlistable marketplace does not fail the run", func(t *testing.T) {
		// The label is cosmetic, so a lookup failure must not block inference.
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /blockchain/models": statusRoute(http.StatusInternalServerError, `{"error":"rpc down"}`),
		})
		model, modelID, _, err := resolveModel(c, config{modelID: id})
		if err != nil {
			t.Fatalf("a failed name lookup must not fail resolution: %v", err)
		}
		if modelID != id || model != id {
			t.Errorf("want the id used as its own label, got model=%q id=%q", model, modelID)
		}
	})

	t.Run("explicit label is taken without a lookup", func(t *testing.T) {
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /blockchain/models": func(w http.ResponseWriter, r *http.Request) {
				t.Error("no marketplace call is needed when both flags are given")
			},
		})
		model, modelID, _, err := resolveModel(c, config{model: "my-label", modelID: id})
		if err != nil {
			t.Fatal(err)
		}
		if model != "my-label" || modelID != id {
			t.Errorf("got model=%q id=%q", model, modelID)
		}
	})

	t.Run("a malformed id is rejected before any request", func(t *testing.T) {
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{})
		_, _, _, err := resolveModel(c, config{modelID: "0xnope"})
		if err == nil {
			t.Fatal("want an error for a malformed --model-id")
		}
		if !strings.Contains(err.Error(), "--model-id") {
			t.Errorf("error should name the flag, got: %v", err)
		}
	})
}

func TestResolveModelFromName(t *testing.T) {
	t.Run("unique name resolves to its id", func(t *testing.T) {
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /blockchain/models": modelsRoute(),
		})
		model, modelID, why, err := resolveModel(c, config{model: "glm-5.3-flash"})
		if err != nil {
			t.Fatal(err)
		}
		want := "0x2222222222222222222222222222222222222222222222222222222222222222"
		if modelID != want {
			t.Errorf("modelID = %q, want %q", modelID, want)
		}
		if model != "glm-5.3-flash" {
			t.Errorf("model = %q", model)
		}
		if !strings.Contains(why, "resolved") {
			t.Errorf("why = %q, want it to mention resolution", why)
		}
	})

	t.Run("name matching is case-insensitive", func(t *testing.T) {
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /blockchain/models": modelsRoute(),
		})
		_, modelID, _, err := resolveModel(c, config{model: "GLM-5.3-Flash"})
		if err != nil {
			t.Fatal(err)
		}
		if modelID != "0x2222222222222222222222222222222222222222222222222222222222222222" {
			t.Errorf("modelID = %q", modelID)
		}
	})

	t.Run("a duplicated name is an error, not a silent first match", func(t *testing.T) {
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /blockchain/models": modelsRoute(),
		})
		_, _, _, err := resolveModel(c, config{model: "deepseek-v4-flash"})
		if err == nil {
			t.Fatal("want an error for an ambiguous model name")
		}
		// Both candidate ids must be offered so the user can choose.
		for _, id := range []string{"0x3333", "0x4444"} {
			if !strings.Contains(err.Error(), id) {
				t.Errorf("error should list candidate %s, got:\n%v", id, err)
			}
		}
		if !strings.Contains(err.Error(), "--model-id") {
			t.Errorf("error should point at --model-id, got: %v", err)
		}
	})

	t.Run("deleted models are not resolvable", func(t *testing.T) {
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /blockchain/models": modelsRoute(),
		})
		if _, _, _, err := resolveModel(c, config{model: "retired-model"}); err == nil {
			t.Fatal("a deleted model must not resolve")
		}
	})

	t.Run("an unknown name suggests close matches", func(t *testing.T) {
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /blockchain/models": modelsRoute(),
		})
		_, _, _, err := resolveModel(c, config{model: "kimi"})
		if err == nil {
			t.Fatal("want an error for a name that is not an exact match")
		}
		if !strings.Contains(err.Error(), "kimi-k2.5") {
			t.Errorf("error should suggest the near match, got: %v", err)
		}
	})
}

func TestModelsFiltersDeletedAndCaches(t *testing.T) {
	calls := 0
	c, _ := newMockRouter(t, map[string]http.HandlerFunc{
		"GET /blockchain/models": func(w http.ResponseWriter, r *http.Request) {
			calls++
			modelsRoute()(w, r)
		},
	})
	ms, err := c.models()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 4 {
		t.Errorf("got %d live models, want 4 (the deleted one must be dropped)", len(ms))
	}
	for _, m := range ms {
		if m.IsDeleted {
			t.Errorf("deleted model %q leaked through", m.Name)
		}
	}
	if _, err := c.models(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("models() made %d HTTP calls, want 1 (the list must be cached)", calls)
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly-10", 10, "exactly-10"},
		{"this-name-is-far-too-long", 10, "this-na..."},
		{"abcd", 3, "abc"},
	}
	for _, tc := range cases {
		if got := truncate(tc.in, tc.n); got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

// --- session cache: history persistence ------------------------------------

func tempCache(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".morctl-session.json")
}

func TestSessionCacheRoundTrip(t *testing.T) {
	path := tempCache(t)
	want := sessionCache{
		SessionID: "0xsess",
		ModelID:   "0xmodel",
		Model:     "kimi-k2.5",
		OpenedAt:  time.Now().Unix(),
		Duration:  600,
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		History: []message{
			{Role: "user", Content: "what is 2+2?"},
			{Role: "assistant", Content: "4"},
		},
	}
	saveSessionCache(path, want)

	got, ok := loadSessionCache(path)
	if !ok {
		t.Fatal("cache did not load back")
	}
	if len(got.History) != 2 {
		t.Fatalf("got %d messages, want 2", len(got.History))
	}
	if got.History[0].Role != "user" || got.History[0].Content != "what is 2+2?" {
		t.Errorf("first message round-tripped wrong: %+v", got.History[0])
	}
	if got.History[1].Role != "assistant" || got.History[1].Content != "4" {
		t.Errorf("second message round-tripped wrong: %+v", got.History[1])
	}
	if got.ExpiresAt != want.ExpiresAt {
		t.Errorf("ExpiresAt = %d, want %d", got.ExpiresAt, want.ExpiresAt)
	}
}

func TestSessionCacheIsPrivate(t *testing.T) {
	// The file holds conversation text, so it must not be world-readable.
	// (Windows does not model POSIX bits, so this is checked where it applies.)
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	path := tempCache(t)
	saveSessionCache(path, sessionCache{SessionID: "0xsess"})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("cache mode = %o, want 600", perm)
	}
}

func TestSessionCacheMissingOrCorrupt(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		if _, ok := loadSessionCache(filepath.Join(t.TempDir(), "absent.json")); ok {
			t.Error("a missing cache must report not-ok, not an empty success")
		}
	})

	t.Run("corrupt file is ignored rather than fatal", func(t *testing.T) {
		path := tempCache(t)
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := loadSessionCache(path); ok {
			t.Error("a corrupt cache must report not-ok")
		}
	})
}

// TestSessionCacheBackwardCompatible covers cache files written before
// ExpiresAt and History existed: they must still load and still be reusable.
func TestSessionCacheBackwardCompatible(t *testing.T) {
	path := tempCache(t)
	opened := time.Now().Add(-time.Minute).Unix()
	old := `{
	  "sessionID": "0x2b5b7c29",
	  "modelID": "0xc8530921",
	  "model": "glm-5.3-flash",
	  "openedAt": ` + strconv.FormatInt(opened, 10) + `,
	  "duration": 600
	}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := loadSessionCache(path)
	if !ok {
		t.Fatal("an older cache file must still load")
	}
	if got.History != nil {
		t.Errorf("history should be empty, got %+v", got.History)
	}
	if got.ExpiresAt != 0 {
		t.Errorf("ExpiresAt should be absent, got %d", got.ExpiresAt)
	}
	// Expiry must fall back to openedAt + duration.
	wantExp := time.Unix(opened, 0).Add(600 * time.Second)
	if !got.expiresAt().Equal(wantExp) {
		t.Errorf("expiresAt() = %v, want %v", got.expiresAt(), wantExp)
	}
	if !got.valid("0xc8530921") {
		t.Error("an older cache with time left must still be reusable")
	}
}

// TestSessionCacheExpiresAtWins guards the reason ExpiresAt is stored at all:
// a different --duration on a later run must not move an open session's expiry.
func TestSessionCacheExpiresAtWins(t *testing.T) {
	realExp := time.Now().Add(5 * time.Minute)
	s := sessionCache{
		SessionID: "0xsess",
		ModelID:   "0xmodel",
		OpenedAt:  time.Now().Unix(),
		Duration:  99999, // a later run passed a much larger --duration
		ExpiresAt: realExp.Unix(),
	}
	if got := s.expiresAt(); got.Unix() != realExp.Unix() {
		t.Errorf("expiresAt() = %v, want the stored %v", got, realExp)
	}
}

// TestDevFlowHistoryLifecycle is the end-to-end check for item 1: a new
// session starts empty, a reused session restores context, and --fresh
// discards it.
func TestDevFlowHistoryLifecycle(t *testing.T) {
	const modelID = "0x1111111111111111111111111111111111111111111111111111111111111111"

	// sentMessages records what each inference call carried.
	var sent [][]message
	routes := func() map[string]http.HandlerFunc {
		return map[string]http.HandlerFunc{
			"GET /healthcheck":                                jsonRoute(`{"status":"healthy"}`),
			"GET /blockchain/balance":                         jsonRoute(`{"eth":"1000000000000000000","mor":"50000000000000000000"}`),
			"GET /blockchain/models":                          modelsRoute(),
			"POST /blockchain/models/" + modelID + "/session": jsonRoute(`{"sessionID":"0xnewsession"}`),
			"POST /v1/chat/completions": func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []message `json:"messages"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				sent = append(sent, body.Messages)
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
			},
		}
	}

	t.Run("a new session starts with empty history", func(t *testing.T) {
		sent = nil
		path := tempCache(t)
		// Seed a cache for a *different* model, which must not be inherited.
		saveSessionCache(path, sessionCache{
			SessionID: "0xold",
			ModelID:   "0xsomeothermodel",
			OpenedAt:  time.Now().Unix(),
			Duration:  600,
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
			History:   []message{{Role: "user", Content: "leaked from another model"}},
		})
		c, srv := newMockRouter(t, routes())
		cfg := config{modelID: modelID, prompt: "hello", duration: 600, dev: true, cacheFile: path, host: srv.URL}
		if err := devFlow(c, cfg); err != nil {
			t.Fatal(err)
		}
		if len(sent) != 1 {
			t.Fatalf("got %d inference calls, want 1", len(sent))
		}
		if len(sent[0]) != 1 || sent[0][0].Content != "hello" {
			t.Errorf("a new session must send only the new prompt, got %+v", sent[0])
		}
		cached, _ := loadSessionCache(path)
		if cached.SessionID != "0xnewsession" {
			t.Errorf("cache should hold the new session, got %q", cached.SessionID)
		}
		if len(cached.History) != 0 {
			t.Errorf("a new session must start with empty history, got %+v", cached.History)
		}
	})

	t.Run("a reused session restores its history", func(t *testing.T) {
		sent = nil
		path := tempCache(t)
		saveSessionCache(path, sessionCache{
			SessionID: "0xlive",
			ModelID:   modelID,
			Model:     "kimi-k2.5",
			OpenedAt:  time.Now().Unix(),
			Duration:  600,
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
			History: []message{
				{Role: "user", Content: "my name is Sam"},
				{Role: "assistant", Content: "nice to meet you"},
			},
		})
		c, srv := newMockRouter(t, routes())
		// --interactive is the mode that replays history; the one-shot path is
		// deliberately stateless, so this asserts the cache was read back.
		cfg := config{modelID: modelID, prompt: "who am I?", duration: 600, dev: true, cacheFile: path, host: srv.URL}
		if err := devFlow(c, cfg); err != nil {
			t.Fatal(err)
		}
		cached, _ := loadSessionCache(path)
		if cached.SessionID != "0xlive" {
			t.Errorf("the live session should have been reused, got %q", cached.SessionID)
		}
		if len(cached.History) != 2 {
			t.Errorf("reuse must not discard stored history, got %+v", cached.History)
		}
	})

	t.Run("--fresh opens a new session and drops history", func(t *testing.T) {
		sent = nil
		path := tempCache(t)
		saveSessionCache(path, sessionCache{
			SessionID: "0xlive",
			ModelID:   modelID,
			OpenedAt:  time.Now().Unix(),
			Duration:  600,
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
			History:   []message{{Role: "user", Content: "forget me"}},
		})
		c, srv := newMockRouter(t, routes())
		cfg := config{modelID: modelID, prompt: "hi", duration: 600, dev: true, fresh: true, cacheFile: path, host: srv.URL}
		if err := devFlow(c, cfg); err != nil {
			t.Fatal(err)
		}
		cached, _ := loadSessionCache(path)
		if cached.SessionID != "0xnewsession" {
			t.Errorf("--fresh must open a new session, got %q", cached.SessionID)
		}
		if len(cached.History) != 0 {
			t.Errorf("--fresh must start with empty history, got %+v", cached.History)
		}
		if len(sent[0]) != 1 {
			t.Errorf("--fresh must not replay old turns, got %+v", sent[0])
		}
	})

	t.Run("--no-history does not restore stored context", func(t *testing.T) {
		sent = nil
		path := tempCache(t)
		saveSessionCache(path, sessionCache{
			SessionID: "0xlive",
			ModelID:   modelID,
			OpenedAt:  time.Now().Unix(),
			Duration:  600,
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
			History:   []message{{Role: "user", Content: "private"}},
		})
		c, srv := newMockRouter(t, routes())
		cfg := config{modelID: modelID, prompt: "hi", duration: 600, dev: true, noHistory: true, cacheFile: path, host: srv.URL}
		if err := devFlow(c, cfg); err != nil {
			t.Fatal(err)
		}
		if len(sent[0]) != 1 || sent[0][0].Content != "hi" {
			t.Errorf("--no-history must send only the new prompt, got %+v", sent[0])
		}
	})
}

// withStdin points os.Stdin at the given script for the duration of a test,
// so the interactive loop can be driven without a terminal.
func withStdin(t *testing.T, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = orig
		_ = f.Close()
	})
}

// TestInteractiveLoopPersistsHistory is the core of item 1: every completed
// turn is written through to the cache, so an interrupted run loses at most
// the turn in flight.
func TestInteractiveLoopPersistsHistory(t *testing.T) {
	const sessionID = "0xlive"
	exp := time.Now().Add(10 * time.Minute)

	newCfg := func(path string) config {
		return config{
			model:     "kimi-k2.5",
			modelID:   "0xmodel",
			duration:  600,
			cacheFile: path,
		}
	}

	t.Run("each turn is saved as it completes", func(t *testing.T) {
		path := tempCache(t)
		saveSessionCache(path, sessionCache{
			SessionID: sessionID, ModelID: "0xmodel", OpenedAt: time.Now().Unix(),
			Duration: 600, ExpiresAt: exp.Unix(),
		})
		turn := 0
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"POST /v1/chat/completions": func(w http.ResponseWriter, r *http.Request) {
				turn++
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"reply ` +
					strconv.Itoa(turn) + `"}}]}`))
			},
		})
		withStdin(t, "first question\nsecond question\nexit\n")
		if err := interactiveLoop(c, newCfg(path), sessionID, exp, nil); err != nil {
			t.Fatal(err)
		}

		cached, ok := loadSessionCache(path)
		if !ok {
			t.Fatal("cache missing after the loop")
		}
		want := []message{
			{Role: "user", Content: "first question"},
			{Role: "assistant", Content: "reply 1"},
			{Role: "user", Content: "second question"},
			{Role: "assistant", Content: "reply 2"},
		}
		if len(cached.History) != len(want) {
			t.Fatalf("got %d messages, want %d: %+v", len(cached.History), len(want), cached.History)
		}
		for i := range want {
			if cached.History[i] != want[i] {
				t.Errorf("message %d = %+v, want %+v", i, cached.History[i], want[i])
			}
		}
		// The session record itself must survive the history writes.
		if cached.SessionID != sessionID || cached.ExpiresAt != exp.Unix() {
			t.Errorf("session record was clobbered: %+v", cached)
		}
	})

	t.Run("restored history is carried into the next turn", func(t *testing.T) {
		path := tempCache(t)
		prior := []message{
			{Role: "user", Content: "my name is Sam"},
			{Role: "assistant", Content: "hello Sam"},
		}
		saveSessionCache(path, sessionCache{
			SessionID: sessionID, ModelID: "0xmodel", OpenedAt: time.Now().Unix(),
			Duration: 600, ExpiresAt: exp.Unix(), History: prior,
		})
		var sent []message
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"POST /v1/chat/completions": func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []message `json:"messages"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				sent = body.Messages
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"you are Sam"}}]}`))
			},
		})
		withStdin(t, "who am I?\nexit\n")
		if err := interactiveLoop(c, newCfg(path), sessionID, exp, prior); err != nil {
			t.Fatal(err)
		}
		if len(sent) != 3 {
			t.Fatalf("the turn should resend 2 prior messages plus the new one, got %d: %+v", len(sent), sent)
		}
		if sent[0].Content != "my name is Sam" || sent[2].Content != "who am I?" {
			t.Errorf("wrong transcript replayed: %+v", sent)
		}
		cached, _ := loadSessionCache(path)
		if len(cached.History) != 4 {
			t.Errorf("history should have grown to 4, got %d", len(cached.History))
		}
	})

	t.Run("a failed turn is not persisted", func(t *testing.T) {
		path := tempCache(t)
		saveSessionCache(path, sessionCache{
			SessionID: sessionID, ModelID: "0xmodel", OpenedAt: time.Now().Unix(),
			Duration: 600, ExpiresAt: exp.Unix(),
		})
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"POST /v1/chat/completions": statusRoute(http.StatusTooManyRequests, `{"error":"over rate limit"}`),
		})
		withStdin(t, "this will fail\nexit\n")
		if err := interactiveLoop(c, newCfg(path), sessionID, exp, nil); err != nil {
			t.Fatal(err)
		}
		cached, _ := loadSessionCache(path)
		if len(cached.History) != 0 {
			t.Errorf("a failed turn must not be stored, got %+v", cached.History)
		}
	})

	t.Run("--no-history writes nothing back", func(t *testing.T) {
		path := tempCache(t)
		saveSessionCache(path, sessionCache{
			SessionID: sessionID, ModelID: "0xmodel", OpenedAt: time.Now().Unix(),
			Duration: 600, ExpiresAt: exp.Unix(),
		})
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"POST /v1/chat/completions": jsonRoute(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`),
		})
		cfg := newCfg(path)
		cfg.noHistory = true
		withStdin(t, "something private\nexit\n")
		if err := interactiveLoop(c, cfg, sessionID, exp, nil); err != nil {
			t.Fatal(err)
		}
		cached, _ := loadSessionCache(path)
		if len(cached.History) != 0 {
			t.Errorf("--no-history must not write conversation text, got %+v", cached.History)
		}
	})

	t.Run("blank lines are skipped and quit ends the loop", func(t *testing.T) {
		path := tempCache(t)
		saveSessionCache(path, sessionCache{
			SessionID: sessionID, ModelID: "0xmodel", OpenedAt: time.Now().Unix(),
			Duration: 600, ExpiresAt: exp.Unix(),
		})
		calls := 0
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"POST /v1/chat/completions": func(w http.ResponseWriter, r *http.Request) {
				calls++
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
			},
		})
		withStdin(t, "\n   \nreal question\nquit\nnever read\n")
		if err := interactiveLoop(c, newCfg(path), sessionID, exp, nil); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Errorf("got %d inference calls, want 1 (blanks skipped, quit honoured)", calls)
		}
	})
}

// TestChatSendsSessionHeaders checks the wire contract the router relies on.
func TestChatSendsSessionHeaders(t *testing.T) {
	var gotSession, gotModelID string
	var gotBody struct {
		Model    string    `json:"model"`
		Messages []message `json:"messages"`
		Stream   bool      `json:"stream"`
	}
	c, _ := newMockRouter(t, map[string]http.HandlerFunc{
		"POST /v1/chat/completions": func(w http.ResponseWriter, r *http.Request) {
			gotSession = r.Header.Get("session_id")
			gotModelID = r.Header.Get("model_id")
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":" hi there "}}]}`))
		},
	})
	reply, err := c.chat("0xsess", "kimi-k2.5", "0xmodel", []message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "hi there" {
		t.Errorf("reply = %q, want it trimmed to %q", reply, "hi there")
	}
	if gotSession != "0xsess" {
		t.Errorf("session_id header = %q", gotSession)
	}
	if gotModelID != "0xmodel" {
		t.Errorf("model_id header = %q", gotModelID)
	}
	if gotBody.Model != "kimi-k2.5" || gotBody.Stream {
		t.Errorf("body = %+v, want model kimi-k2.5 and stream false", gotBody)
	}
}

func TestChatEmptyChoicesIsAnError(t *testing.T) {
	c, _ := newMockRouter(t, map[string]http.HandlerFunc{
		"POST /v1/chat/completions": jsonRoute(`{"choices":[]}`),
	})
	if _, err := c.chat("0xsess", "m", "0xmodel", []message{{Role: "user", Content: "hi"}}); err == nil {
		t.Fatal("want an error when the router returns no choices")
	}
}

// --- session visibility -----------------------------------------------------

func TestSessionListItemStatus(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		name     string
		item     sessionListItem
		want     string
		wantLive bool
	}{
		{
			name:     "open and in its window",
			item:     sessionListItem{EndsAt: now + 300},
			want:     "LIVE",
			wantLive: true,
		},
		{
			name: "past its end but not settled -- still holds stake",
			item: sessionListItem{EndsAt: now - 300},
			want: "expired",
		},
		{
			name: "closed early",
			item: sessionListItem{EndsAt: now + 300, ClosedAt: now - 10},
			want: "closed",
		},
		{
			name: "closed and expired reports closed",
			item: sessionListItem{EndsAt: now - 300, ClosedAt: now - 100},
			want: "closed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.item.status(); got != tc.want {
				t.Errorf("status() = %q, want %q", got, tc.want)
			}
			if got := tc.item.live(); got != tc.wantLive {
				t.Errorf("live() = %v, want %v", got, tc.wantLive)
			}
		})
	}
}

// TestUserSessionsPrecision is the reason Stake is json.Number: the router
// sends it as a JSON number in wei, and real values exceed what a float64
// holds exactly. Decoding via float64 would silently corrupt the amount.
func TestUserSessionsPrecision(t *testing.T) {
	c, _ := newMockRouter(t, map[string]http.HandlerFunc{
		"GET /blockchain/sessions/user": jsonRoute(`{"sessions":[
			{"Id":"0xaaa","ModelAgentId":"0xmodel","Stake":5688807051347138757,"PricePerSecond":27777777777777,"OpenedAt":1788939567,"EndsAt":1788940166,"ClosedAt":1788940185},
			{"Id":"0xbbb","ModelAgentId":"0xmodel","Stake":123456789012345678901234567890,"PricePerSecond":1,"OpenedAt":1,"EndsAt":2,"ClosedAt":0}
		]}`),
	})
	got, err := c.userSessions("0xwallet", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2", len(got))
	}
	// 5688807051347138757 wei, exact to 4dp.
	if s := formatWei(got[0].Stake.String()); s != "5.6888" {
		t.Errorf("stake = %q, want 5.6888", s)
	}
	if got[0].Stake.String() != "5688807051347138757" {
		t.Errorf("stake digits were altered: %q", got[0].Stake.String())
	}
	// Larger than int64: must survive rather than overflow.
	if got[1].Stake.String() != "123456789012345678901234567890" {
		t.Errorf("oversized stake was corrupted: %q", got[1].Stake.String())
	}
}

func TestWalletAddress(t *testing.T) {
	t.Run("reads the router wallet", func(t *testing.T) {
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /wallet": jsonRoute(`{"address":"0xffd9883a","kind":"mnemonic","derivationPath":"0"}`),
		})
		addr, err := c.walletAddress()
		if err != nil {
			t.Fatal(err)
		}
		if addr != "0xffd9883a" {
			t.Errorf("address = %q", addr)
		}
	})

	t.Run("no wallet is an explained error", func(t *testing.T) {
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /wallet": jsonRoute(`{}`),
		})
		_, err := c.walletAddress()
		if err == nil {
			t.Fatal("want an error when the router has no wallet")
		}
		if !strings.Contains(err.Error(), "wallet") {
			t.Errorf("error should mention the wallet, got: %v", err)
		}
	})
}

func TestShowSessions(t *testing.T) {
	now := time.Now().Unix()

	t.Run("queries by the router's own wallet address", func(t *testing.T) {
		var gotQuery string
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /wallet": jsonRoute(`{"address":"0xffd9883a"}`),
			"GET /blockchain/sessions/user": func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.RawQuery
				_, _ = w.Write([]byte(`{"sessions":[]}`))
			},
		})
		if err := showSessions(c, config{}); err != nil {
			t.Fatal(err)
		}
		// The user never has to supply their own address, unlike mor-cli.
		if !strings.Contains(gotQuery, "user=0xffd9883a") {
			t.Errorf("query = %q, want it to carry the wallet address", gotQuery)
		}
	})

	t.Run("an empty list is not an error", func(t *testing.T) {
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /wallet":                   jsonRoute(`{"address":"0xffd9883a"}`),
			"GET /blockchain/sessions/user": jsonRoute(`{"sessions":[]}`),
		})
		if err := showSessions(c, config{}); err != nil {
			t.Errorf("a wallet with no sessions must not error: %v", err)
		}
	})

	t.Run("names are not resolved unless asked", func(t *testing.T) {
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /wallet": jsonRoute(`{"address":"0xffd9883a"}`),
			"GET /blockchain/sessions/user": jsonRoute(`{"sessions":[
				{"Id":"0xaaa","ModelAgentId":"0x1111111111111111111111111111111111111111111111111111111111111111","Stake":1000000000000000000,"EndsAt":` +
				strconv.FormatInt(now+300, 10) + `}]}`),
			"GET /blockchain/models": func(w http.ResponseWriter, r *http.Request) {
				t.Error("the marketplace must not be fetched without --resolve-names")
			},
		})
		if err := showSessions(c, config{}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("--resolve-names fetches the marketplace", func(t *testing.T) {
		fetched := false
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /wallet": jsonRoute(`{"address":"0xffd9883a"}`),
			"GET /blockchain/sessions/user": jsonRoute(`{"sessions":[
				{"Id":"0xaaa","ModelAgentId":"0x1111111111111111111111111111111111111111111111111111111111111111","Stake":1000000000000000000,"EndsAt":` +
				strconv.FormatInt(now+300, 10) + `}]}`),
			"GET /blockchain/models": func(w http.ResponseWriter, r *http.Request) {
				fetched = true
				modelsRoute()(w, r)
			},
		})
		if err := showSessions(c, config{resolveNames: true}); err != nil {
			t.Fatal(err)
		}
		if !fetched {
			t.Error("--resolve-names should fetch the marketplace")
		}
	})

	t.Run("an unlistable marketplace still lists the sessions", func(t *testing.T) {
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /wallet": jsonRoute(`{"address":"0xffd9883a"}`),
			"GET /blockchain/sessions/user": jsonRoute(`{"sessions":[
				{"Id":"0xaaa","ModelAgentId":"0xmodel","Stake":1000000000000000000,"EndsAt":` +
				strconv.FormatInt(now+300, 10) + `}]}`),
			"GET /blockchain/models": statusRoute(http.StatusTooManyRequests, `{"error":"over rate limit"}`),
		})
		if err := showSessions(c, config{resolveNames: true}); err != nil {
			t.Errorf("a name-lookup failure must not lose the session list: %v", err)
		}
	})

	t.Run("a failed session query is reported", func(t *testing.T) {
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /wallet":                   jsonRoute(`{"address":"0xffd9883a"}`),
			"GET /blockchain/sessions/user": statusRoute(http.StatusTooManyRequests, `{"error":"over rate limit"}`),
		})
		err := showSessions(c, config{})
		if err == nil {
			t.Fatal("want an error when the session query fails")
		}
		var re *routerError
		if !errors.As(err, &re) {
			t.Errorf("error should unwrap to *routerError for translation, got %T", err)
		}
	})
}

// --- diagnose ---------------------------------------------------------------

// rpcServer stands in for a Base RPC endpoint.
func rpcServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

func okRPC(chainHex string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + chainHex + `"}`))
	}
}

// healthyRoutes is a router that passes every check, which each test then
// breaks in exactly one way.
func healthyRoutes(rpcURL string) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /healthcheck": jsonRoute(`{"status":"healthy","version":"v7.14.0","uptime":"1h0m0s",
			"components":{"badgerdb":"healthy, size=1.0MB"},
			"models":[{"modelId":"0xaaa","status":"healthy","hasActiveBid":true}]}`),
		"GET /wallet": jsonRoute(`{"address":"0xffd9883a"}`),
		"GET /config": jsonRoute(`{"Version":"v7.14.0","DerivedConfig":{"WalletAddress":"0xffd9883a","ChainID":8453,
			"EthNodeURLs":["` + rpcURL + `"]},"Config":{"Marketplace":{"DiamondContractAddress":"0xdiamond"}}}`),
		"GET /blockchain/balance":       jsonRoute(`{"eth":"1000000000000000000","mor":"50000000000000000000"}`),
		"GET /blockchain/allowance":     jsonRoute(`{"allowance":"14482038714754728536"}`),
		"GET /blockchain/stakes/onhold": jsonRoute(`{"available":"0","hold":"0"}`),
	}
}

func TestDiagnoseHealthy(t *testing.T) {
	rpc := rpcServer(t, okRPC("0x2105"))
	c, _ := newMockRouter(t, healthyRoutes(rpc))
	r := runChecks(c, config{})
	if r.failed() {
		for _, chk := range r.checks {
			if chk.state == "fail" {
				t.Errorf("unexpected failure: %s -- %s", chk.name, chk.detail)
			}
		}
	}
	if r.unreachable {
		t.Error("the router was reachable")
	}
	for _, want := range []string{"router", "credentials", "chain id", "rpc reachable", "balance", "allowance", "stake on hold", "model health"} {
		if _, ok := r.find(want); !ok {
			t.Errorf("no %q check in the report", want)
		}
	}
}

func TestDiagnoseFailures(t *testing.T) {
	t.Run("router down stops after the first check", func(t *testing.T) {
		c, srv := newMockRouter(t, map[string]http.HandlerFunc{})
		srv.Close()
		r := runChecks(c, config{})
		if !r.unreachable {
			t.Error("want unreachable set")
		}
		if len(r.checks) != 1 {
			t.Errorf("want a single check when the router is down, got %d", len(r.checks))
		}
		if err := diagnose(c, config{}); err == nil {
			t.Error("diagnose must report an error when the router is down")
		}
	})

	t.Run("a degraded component is a failure with a fix", func(t *testing.T) {
		rpc := rpcServer(t, okRPC("0x2105"))
		routes := healthyRoutes(rpc)
		routes["GET /healthcheck"] = statusRoute(http.StatusServiceUnavailable,
			`{"status":"degraded","version":"v7.14.0","uptime":"1h",
			  "components":{"badgerdb":"unhealthy: manifest corrupted"}}`)
		c, _ := newMockRouter(t, routes)
		r := runChecks(c, config{})
		// The 503 body must still be parsed -- it carries the diagnosis.
		chk, ok := r.find("component badgerdb")
		if !ok {
			t.Fatal("the component detail from a 503 body was lost")
		}
		if chk.state != "fail" || !strings.Contains(chk.detail, "corrupted") {
			t.Errorf("component check = %+v", chk)
		}
		if chk.fix == "" {
			t.Error("a corrupt store should come with a suggested fix")
		}
		if !r.failed() {
			t.Error("a degraded router must fail the diagnosis")
		}
	})

	t.Run("a stale cookie fails credentials but keeps checking", func(t *testing.T) {
		rpc := rpcServer(t, okRPC("0x2105"))
		routes := healthyRoutes(rpc)
		routes["GET /wallet"] = statusRoute(http.StatusUnauthorized, `{"error":"invalid username or password"}`)
		routes["GET /config"] = statusRoute(http.StatusUnauthorized, `{"error":"invalid username or password"}`)
		c, _ := newMockRouter(t, routes)
		r := runChecks(c, config{})

		creds, _ := r.find("credentials")
		if creds.state != "fail" {
			t.Errorf("credentials check = %+v, want fail", creds)
		}
		if !strings.Contains(creds.fix, ".cookie") {
			t.Errorf("fix should point at the cookie, got %q", creds.fix)
		}
		// The point of using the unauthenticated healthcheck first: the router
		// and model health are still reported despite the bad cookie.
		if router, _ := r.find("router"); router.state != "ok" {
			t.Errorf("router check = %+v, want ok even with a bad cookie", router)
		}
		if _, ok := r.find("model health"); !ok {
			t.Error("model health should still be reported with a bad cookie")
		}
	})

	t.Run("wrong chain id is a failure", func(t *testing.T) {
		rpc := rpcServer(t, okRPC("0xaa36a7")) // Sepolia
		routes := healthyRoutes(rpc)
		routes["GET /config"] = jsonRoute(`{"DerivedConfig":{"ChainID":11155111,"EthNodeURLs":["` + rpc + `"]},
			"Config":{"Marketplace":{"DiamondContractAddress":"0xdiamond"}}}`)
		c, _ := newMockRouter(t, routes)
		r := runChecks(c, config{})
		chk, ok := r.find("chain id")
		if !ok || chk.state != "fail" {
			t.Fatalf("chain id check = %+v, want fail", chk)
		}
		if !strings.Contains(chk.fix, "8453") {
			t.Errorf("fix should name the expected chain, got %q", chk.fix)
		}
	})

	t.Run("a rate-limited RPC is called out specifically", func(t *testing.T) {
		rpc := rpcServer(t, statusRoute(http.StatusTooManyRequests, `{"message":"over rate limit"}`))
		c, _ := newMockRouter(t, healthyRoutes(rpc))
		r := runChecks(c, config{})
		chk, ok := r.find("rpc reachable")
		if !ok || chk.state != "fail" {
			t.Fatalf("rpc check = %+v, want fail", chk)
		}
		if !strings.Contains(chk.detail, "429") {
			t.Errorf("detail should name the throttling, got %q", chk.detail)
		}
		if !strings.Contains(chk.fix, "dedicated RPC") {
			t.Errorf("fix should suggest a dedicated RPC, got %q", chk.fix)
		}
	})

	t.Run("a dead RPC endpoint is distinguished from a busy one", func(t *testing.T) {
		rpc := rpcServer(t, statusRoute(http.StatusGone, `gone`))
		c, _ := newMockRouter(t, healthyRoutes(rpc))
		r := runChecks(c, config{})
		chk, _ := r.find("rpc reachable")
		if chk.state != "fail" || !strings.Contains(chk.detail, "410") {
			t.Errorf("rpc check = %+v, want a 410-specific failure", chk)
		}
	})

	t.Run("an RPC on the wrong chain warns", func(t *testing.T) {
		rpc := rpcServer(t, okRPC("0x1")) // Ethereum mainnet
		c, _ := newMockRouter(t, healthyRoutes(rpc))
		r := runChecks(c, config{})
		chk, _ := r.find("rpc reachable")
		if chk.state != "warn" {
			t.Errorf("rpc check = %+v, want warn when the RPC chain disagrees", chk)
		}
	})

	t.Run("low MOR warns rather than fails", func(t *testing.T) {
		rpc := rpcServer(t, okRPC("0x2105"))
		routes := healthyRoutes(rpc)
		routes["GET /blockchain/balance"] = jsonRoute(`{"eth":"1000000000000000000","mor":"1000000000000000000"}`)
		c, _ := newMockRouter(t, routes)
		r := runChecks(c, config{})
		chk, _ := r.find("balance")
		if chk.state != "warn" {
			t.Errorf("balance check = %+v, want warn at 1 MOR", chk)
		}
		// A low balance is a heads-up, not a blocker.
		if r.failed() {
			t.Error("a low balance alone must not fail the diagnosis")
		}
	})

	t.Run("a zero allowance is a failure", func(t *testing.T) {
		rpc := rpcServer(t, okRPC("0x2105"))
		routes := healthyRoutes(rpc)
		routes["GET /blockchain/allowance"] = jsonRoute(`{"allowance":"0"}`)
		c, _ := newMockRouter(t, routes)
		r := runChecks(c, config{})
		chk, _ := r.find("allowance")
		if chk.state != "fail" {
			t.Errorf("allowance check = %+v, want fail at 0", chk)
		}
	})

	t.Run("claimable stake is surfaced as a warning", func(t *testing.T) {
		rpc := rpcServer(t, okRPC("0x2105"))
		routes := healthyRoutes(rpc)
		routes["GET /blockchain/stakes/onhold"] = jsonRoute(`{"available":"2500000000000000000","hold":"0"}`)
		c, _ := newMockRouter(t, routes)
		r := runChecks(c, config{})
		chk, _ := r.find("stake on hold")
		if chk.state != "warn" || !strings.Contains(chk.detail, "2.5000") {
			t.Errorf("stake check = %+v, want a warning naming the amount", chk)
		}
		if !strings.Contains(chk.fix, "--claim-stakes") {
			t.Errorf("fix should name the claim command, got %q", chk.fix)
		}
	})

	t.Run("a router without the stake endpoint says so", func(t *testing.T) {
		rpc := rpcServer(t, okRPC("0x2105"))
		routes := healthyRoutes(rpc)
		delete(routes, "GET /blockchain/stakes/onhold") // pre-7.11.0 router: 404
		c, _ := newMockRouter(t, routes)
		r := runChecks(c, config{})
		chk, ok := r.find("stake on hold")
		if !ok {
			t.Fatal("the stake check must still appear, not be silently dropped")
		}
		if chk.state != "warn" || !strings.Contains(chk.fix, "v7.11.0") {
			t.Errorf("stake check = %+v, want a warning naming v7.11.0", chk)
		}
	})

	t.Run("an unreadable stake endpoint is still reported", func(t *testing.T) {
		rpc := rpcServer(t, okRPC("0x2105"))
		routes := healthyRoutes(rpc)
		routes["GET /blockchain/stakes/onhold"] = statusRoute(
			http.StatusInternalServerError, `{"error":"rpc failure"}`)
		c, _ := newMockRouter(t, routes)
		r := runChecks(c, config{})
		// Dropping this check would make a wallet with stuck stake look clean.
		if _, ok := r.find("stake on hold"); !ok {
			t.Error("a failed stake read must still produce a check line")
		}
	})

	t.Run("rate-limited models are reported", func(t *testing.T) {
		rpc := rpcServer(t, okRPC("0x2105"))
		routes := healthyRoutes(rpc)
		routes["GET /healthcheck"] = jsonRoute(`{"status":"healthy","version":"v7.14.0","uptime":"1h",
			"components":{"badgerdb":"healthy"},
			"models":[
			  {"modelId":"0xaaa","status":"unhealthy","errorKind":"rate_limited","httpStatus":429},
			  {"modelId":"0xbbb","status":"healthy"}]}`)
		c, _ := newMockRouter(t, routes)
		r := runChecks(c, config{})
		chk, _ := r.find("model health")
		if chk.state != "warn" || !strings.Contains(chk.detail, "rate-limited") {
			t.Errorf("model health = %+v, want a rate-limit warning", chk)
		}
	})
}

func TestProbeRPC(t *testing.T) {
	t.Run("reads the chain id", func(t *testing.T) {
		var gotBody string
		url := rpcServer(t, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x2105"}`))
		})
		chain, status, err := probeRPC(url, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if chain != "0x2105" || status != 200 {
			t.Errorf("got chain=%q status=%d", chain, status)
		}
		if !strings.Contains(gotBody, "eth_chainId") {
			t.Errorf("want an eth_chainId call, got %q", gotBody)
		}
	})

	t.Run("reports the status for a throttled endpoint", func(t *testing.T) {
		url := rpcServer(t, statusRoute(http.StatusTooManyRequests, `over rate limit`))
		_, status, err := probeRPC(url, 5*time.Second)
		if err == nil {
			t.Fatal("want an error")
		}
		if status != 429 {
			t.Errorf("status = %d, want 429 so the caller can distinguish it", status)
		}
	})

	t.Run("surfaces a JSON-RPC error object", func(t *testing.T) {
		url := rpcServer(t, jsonRoute(`{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"limit exceeded"}}`))
		_, _, err := probeRPC(url, 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "limit exceeded") {
			t.Errorf("want the RPC error surfaced, got %v", err)
		}
	})
}

// --- stake awareness --------------------------------------------------------

func TestStakesOnHoldPredicates(t *testing.T) {
	cases := []struct {
		name          string
		s             stakesOnHold
		wantAny       bool
		wantClaimable bool
	}{
		{name: "nothing on hold", s: stakesOnHold{Available: "0", Hold: "0"}},
		{name: "empty response fields", s: stakesOnHold{}},
		{
			name:          "claimable now",
			s:             stakesOnHold{Available: "2500000000000000000", Hold: "0"},
			wantAny:       true,
			wantClaimable: true,
		},
		{
			name:    "locked but not yet claimable",
			s:       stakesOnHold{Available: "0", Hold: "2500000000000000000"},
			wantAny: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.any(); got != tc.wantAny {
				t.Errorf("any() = %v, want %v", got, tc.wantAny)
			}
			if got := tc.s.claimable(); got != tc.wantClaimable {
				t.Errorf("claimable() = %v, want %v", got, tc.wantClaimable)
			}
		})
	}
}

func TestStakesRead(t *testing.T) {
	t.Run("reads both figures", func(t *testing.T) {
		var gotQuery string
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /blockchain/stakes/onhold": func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.RawQuery
				_, _ = w.Write([]byte(`{"available":"2500000000000000000","hold":"1000000000000000000"}`))
			},
		})
		s, err := c.stakes()
		if err != nil {
			t.Fatal(err)
		}
		if formatWei(s.Available) != "2.5000" || formatWei(s.Hold) != "1.0000" {
			t.Errorf("got available=%s hold=%s", formatWei(s.Available), formatWei(s.Hold))
		}
		if !strings.Contains(gotQuery, "iterations=255") {
			t.Errorf("query = %q, want the contract iteration cap", gotQuery)
		}
	})

	t.Run("an older router is explained, not crashed on", func(t *testing.T) {
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{})
		_, err := c.stakes() // the mock 404s unknown routes, like a pre-7.11.0 router
		if err == nil {
			t.Fatal("want an error from a router without the endpoint")
		}
		var re *routerError
		if !errors.As(err, &re) {
			t.Fatalf("want a *routerError, got %T", err)
		}
		d, ok := re.friendly()
		if !ok {
			t.Fatal("a 404 on the stake endpoint should be translated")
		}
		if !strings.Contains(d.fix, "v7.11.0") {
			t.Errorf("fix should name the version that added it, got %q", d.fix)
		}
	})
}

// TestClaimStakesRefusesWithoutConfirm and the cases below cover the gating on
// the one command that spends gas. No test ever reaches a real router.
func TestClaimStakes(t *testing.T) {
	// withdrawCalled is the thing that must not happen unless every gate passes.
	newRouter := func(t *testing.T, available, hold string, withdrawCalled *bool) *client {
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /blockchain/stakes/onhold": jsonRoute(
				`{"available":"` + available + `","hold":"` + hold + `"}`),
			"POST /blockchain/stakes/withdraw": func(w http.ResponseWriter, r *http.Request) {
				*withdrawCalled = true
				var body struct {
					Iterations int `json:"iterations"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body.Iterations != 255 {
					t.Errorf("iterations = %d, want 255", body.Iterations)
				}
				_, _ = w.Write([]byte(`{"tx":"0xdeadbeef"}`))
			},
		})
		return c
	}

	t.Run("refuses without --confirm and sends nothing", func(t *testing.T) {
		called := false
		c := newRouter(t, "2500000000000000000", "0", &called)
		err := claimStakes(c, config{claimStakes: true})
		if err == nil {
			t.Fatal("want a refusal without --confirm")
		}
		if !strings.Contains(err.Error(), "--confirm") {
			t.Errorf("the refusal should name the flag, got: %v", err)
		}
		if called {
			t.Error("no transaction may be sent without --confirm")
		}
	})

	t.Run("refuses when there is nothing to claim", func(t *testing.T) {
		called := false
		c := newRouter(t, "0", "0", &called)
		// Even with --confirm: burning gas on a revert is not a service.
		err := claimStakes(c, config{claimStakes: true, confirm: true})
		if err == nil {
			t.Fatal("want a refusal when nothing is claimable")
		}
		if called {
			t.Error("no transaction may be sent with nothing to claim")
		}
	})

	t.Run("explains a lock that has not elapsed", func(t *testing.T) {
		called := false
		c := newRouter(t, "0", "2500000000000000000", &called)
		err := claimStakes(c, config{claimStakes: true, confirm: true})
		if err == nil {
			t.Fatal("want a refusal while the stake is still locked")
		}
		if !strings.Contains(err.Error(), "24h") {
			t.Errorf("the refusal should explain the lock, got: %v", err)
		}
		if called {
			t.Error("no transaction may be sent while the stake is locked")
		}
	})

	t.Run("claims with --confirm on non-interactive stdin", func(t *testing.T) {
		called := false
		c := newRouter(t, "2500000000000000000", "0", &called)
		// A file-backed stdin is not a terminal, so --confirm alone proceeds:
		// this is the scripted path.
		withStdin(t, "")
		if err := claimStakes(c, config{claimStakes: true, confirm: true}); err != nil {
			t.Fatal(err)
		}
		if !called {
			t.Error("the withdrawal should have been sent")
		}
	})

	t.Run("a withdrawal already in flight is explained", func(t *testing.T) {
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /blockchain/stakes/onhold": jsonRoute(`{"available":"2500000000000000000","hold":"0"}`),
			"POST /blockchain/stakes/withdraw": statusRoute(http.StatusConflict,
				`{"error":"operation exists or journal unavailable"}`),
		})
		withStdin(t, "")
		err := claimStakes(c, config{claimStakes: true, confirm: true})
		if err == nil {
			t.Fatal("want an error on 409")
		}
		var re *routerError
		if !errors.As(err, &re) {
			t.Fatalf("want a *routerError, got %T", err)
		}
		d, ok := re.friendly()
		if !ok || !strings.Contains(d.cause, "already in flight") {
			t.Errorf("409 should be translated, got ok=%v %+v", ok, d)
		}
	})
}

// --- friendly error translation --------------------------------------------

func TestRouterErrorFriendly(t *testing.T) {
	cases := []struct {
		name     string
		err      routerError
		wantOK   bool
		inCause  []string
		inFix    []string
		notCause []string
	}{
		{
			name:    "429 is read as RPC throttling",
			err:     routerError{status: 429, reason: "429 Too Many Requests", body: `{"error":"request failed"}`},
			wantOK:  true,
			inCause: []string{"rate-limiting"},
			inFix:   []string{"ETH_NODE_ADDRESS"},
		},
		{
			name:    "an over-rate-limit body is caught even behind a 500",
			err:     routerError{status: 500, reason: "500 Internal Server Error", body: `{"error":"internal error: 429 Too Many Requests: {\"message\":\"over rate limit\"}"}`},
			wantOK:  true,
			inCause: []string{"rate-limiting"},
			inFix:   []string{"dedicated RPC"},
		},
		{
			name:    "410 is a dead endpoint, not a busy one",
			err:     routerError{status: 410, reason: "410 Gone", body: `{"error":"gone"}`},
			wantOK:  true,
			inCause: []string{"dead"},
			inFix:   []string{"ETH_NODE_ADDRESS"},
		},
		{
			name:    "an upstream 410 reported through a 500 is still recognised",
			err:     routerError{status: 500, reason: "500 Internal Server Error", body: `{"error":"eth node returned 410 Gone"}`},
			wantOK:  true,
			inCause: []string{"dead"},
		},
		{
			name:    "insufficient balance",
			err:     routerError{status: 500, reason: "500 Internal Server Error", body: `{"error":"execution reverted: ERC20: insufficient allowance"}`},
			wantOK:  true,
			inCause: []string{"MOR"},
			inFix:   []string{"--duration"},
		},
		{
			name:    "no bids for the model",
			err:     routerError{status: 400, reason: "400 Bad Request", body: `{"error":"no bid found for model"}`},
			wantOK:  true,
			inCause: []string{"provider"},
			inFix:   []string{"--list-models"},
		},
		{
			name:    "provider disconnect mid-session",
			err:     routerError{status: 502, reason: "502 Bad Gateway", body: `{"error":"post to provider failed: unexpected EOF"}`},
			wantOK:  true,
			inCause: []string{"dropped the connection"},
			inFix:   []string{"still open"},
		},
		{
			name:    "401 points at cookie rotation",
			err:     routerError{status: 401, reason: "401 Unauthorized", body: `{"error":"unauthorized"}`},
			wantOK:  true,
			inCause: []string{"credentials"},
			inFix:   []string{".cookie", "restarts"},
		},
		{
			name:    "403 points at scoped permissions",
			err:     routerError{status: 403, reason: "403 Forbidden", body: `{"error":"method not allowed"}`},
			wantOK:  true,
			inCause: []string{"not allowed"},
			inFix:   []string{"permission"},
		},
		{
			name:    "a 404 on the stake endpoints means the router is too old",
			err:     routerError{status: 404, path: "/blockchain/stakes/onhold", reason: "404 Not Found", body: `404 page not found`},
			wantOK:  true,
			inCause: []string{"stake endpoints"},
			inFix:   []string{"v7.11.0"},
		},
		{
			name:    "a 409 on withdraw means one is already running",
			err:     routerError{status: 409, path: "/blockchain/stakes/withdraw", reason: "409 Conflict", body: `{"error":"operation exists or journal unavailable"}`},
			wantOK:  true,
			inCause: []string{"already in flight"},
		},
		{
			name:   "an unrecognised failure is not given a made-up cause",
			err:    routerError{status: 418, reason: "418 I'm a teapot", body: `{"error":"short and stout"}`},
			wantOK: false,
		},
		{
			name:   "a plain 404 elsewhere is not treated as a stake problem",
			err:    routerError{status: 404, path: "/blockchain/models", reason: "404 Not Found", body: `404 page not found`},
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.err.friendly()
			if ok != tc.wantOK {
				t.Fatalf("friendly() ok = %v, want %v (got %+v)", ok, tc.wantOK, got)
			}
			if !ok {
				return
			}
			for _, want := range tc.inCause {
				if !strings.Contains(got.cause, want) {
					t.Errorf("cause %q should contain %q", got.cause, want)
				}
			}
			for _, want := range tc.inFix {
				if !strings.Contains(got.fix, want) {
					t.Errorf("fix %q should contain %q", got.fix, want)
				}
			}
			if got.cause == "" || got.fix == "" {
				t.Error("a translated error must carry both a cause and a fix")
			}
		})
	}
}

func TestRenderError(t *testing.T) {
	throttled := &routerError{
		method: http.MethodPost, path: "/blockchain/models/0xabc/session",
		status: 429, reason: "429 Too Many Requests",
		body: `{"error":"over rate limit"}`,
	}

	t.Run("default output shows the cause and the fix", func(t *testing.T) {
		var buf strings.Builder
		renderError(&buf, fmt.Errorf("open session: %w", throttled), config{})
		out := buf.String()
		if !strings.Contains(out, "rate-limiting") {
			t.Errorf("want a translated cause, got:\n%s", out)
		}
		if !strings.Contains(out, "ETH_NODE_ADDRESS") {
			t.Errorf("want a suggested fix, got:\n%s", out)
		}
		// The status is still there for orientation, and --verbose is offered.
		if !strings.Contains(out, "429") || !strings.Contains(out, "--verbose") {
			t.Errorf("want the status and a pointer to --verbose, got:\n%s", out)
		}
	})

	t.Run("--verbose keeps the raw response", func(t *testing.T) {
		var buf strings.Builder
		renderError(&buf, fmt.Errorf("open session: %w", throttled), config{verbose: true})
		out := buf.String()
		if !strings.Contains(out, "over rate limit") {
			t.Errorf("--verbose must show the raw body, got:\n%s", out)
		}
		if !strings.Contains(out, "rate-limiting") {
			t.Errorf("--verbose must keep the translation too, got:\n%s", out)
		}
	})

	t.Run("--json keeps the raw response", func(t *testing.T) {
		var buf strings.Builder
		renderError(&buf, throttled, config{jsonOut: true})
		if !strings.Contains(buf.String(), "over rate limit") {
			t.Errorf("--json must show the raw body, got:\n%s", buf.String())
		}
	})

	t.Run("an untranslatable error is printed verbatim", func(t *testing.T) {
		var buf strings.Builder
		renderError(&buf, errors.New("router unreachable at http://localhost:8082"), config{})
		out := buf.String()
		if !strings.Contains(out, "router unreachable") {
			t.Errorf("want the original message, got:\n%s", out)
		}
		if strings.Contains(out, "→") {
			t.Errorf("no fix should be invented for an unknown error, got:\n%s", out)
		}
	})
}

// TestTranslationReachesTheRealErrorPath checks the wiring, not just the table:
// a throttled router response must come back as a *routerError through the
// wrapping that devFlow applies.
func TestTranslationReachesTheRealErrorPath(t *testing.T) {
	const modelID = "0x1111111111111111111111111111111111111111111111111111111111111111"
	c, srv := newMockRouter(t, map[string]http.HandlerFunc{
		"GET /blockchain/models": modelsRoute(),
		"POST /blockchain/models/" + modelID + "/session": statusRoute(
			http.StatusTooManyRequests, `{"error":"over rate limit"}`),
	})
	cfg := config{modelID: modelID, duration: 600, dev: true, cacheFile: tempCache(t), host: srv.URL}
	err := devFlow(c, cfg)
	if err == nil {
		t.Fatal("want an error from a throttled session open")
	}
	var re *routerError
	if !errors.As(err, &re) {
		t.Fatalf("error should unwrap to *routerError, got %T: %v", err, err)
	}
	if re.status != 429 {
		t.Errorf("status = %d, want 429", re.status)
	}
	var buf strings.Builder
	renderError(&buf, err, cfg)
	if !strings.Contains(buf.String(), "rate-limiting") {
		t.Errorf("the real error path must be translated, got:\n%s", buf.String())
	}
}

func TestClientBalance(t *testing.T) {
	c, _ := newMockRouter(t, map[string]http.HandlerFunc{
		"GET /blockchain/balance": jsonRoute(`{"eth":"399745950320894","mor":"5000000000000000000"}`),
	})
	bal, err := c.balance()
	if err != nil {
		t.Fatal(err)
	}
	if formatWei(bal.MOR) != "5.0000" {
		t.Errorf("MOR = %q, want 5.0000", formatWei(bal.MOR))
	}
	if formatWei(bal.ETH) != "0.0004" {
		t.Errorf("ETH = %q, want 0.0004", formatWei(bal.ETH))
	}
}

func TestClientSendsBasicAuth(t *testing.T) {
	var gotUser, gotPass string
	var ok bool
	c, _ := newMockRouter(t, map[string]http.HandlerFunc{
		"GET /blockchain/balance": func(w http.ResponseWriter, r *http.Request) {
			gotUser, gotPass, ok = r.BasicAuth()
			_, _ = w.Write([]byte(`{"eth":"0","mor":"0"}`))
		},
	})
	if _, err := c.balance(); err != nil {
		t.Fatal(err)
	}
	if !ok || gotUser != "admin" || gotPass != "test-pass" {
		t.Errorf("basic auth not sent correctly: ok=%v user=%q", ok, gotUser)
	}
}

func TestClientNon2xxIsAnError(t *testing.T) {
	c, _ := newMockRouter(t, map[string]http.HandlerFunc{
		"GET /blockchain/balance": statusRoute(http.StatusUnauthorized, `{"error":"unauthorized"}`),
	})
	_, err := c.balance()
	if err == nil {
		t.Fatal("want an error on 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should name the status, got: %v", err)
	}
}

func TestReachable(t *testing.T) {
	t.Run("any HTTP response means the router is up", func(t *testing.T) {
		// Even a 401 proves something is listening and speaking HTTP.
		c, _ := newMockRouter(t, map[string]http.HandlerFunc{
			"GET /healthcheck": statusRoute(http.StatusUnauthorized, `{}`),
		})
		if err := c.reachable(); err != nil {
			t.Errorf("401 should still count as reachable, got %v", err)
		}
	})

	t.Run("a dead address is unreachable", func(t *testing.T) {
		c, srv := newMockRouter(t, map[string]http.HandlerFunc{})
		srv.Close() // simulate the router not running
		if err := c.reachable(); err == nil {
			t.Error("want a transport error against a closed server")
		}
	})
}
