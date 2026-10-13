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
