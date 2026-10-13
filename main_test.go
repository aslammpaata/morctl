package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
