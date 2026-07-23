// Command morpheus-doctor diagnoses a local Morpheus proxy-router and, with --dev,
// runs the full session -> inference handshake in one step so you never have to
// copy-paste a session ID between commands.
//
// See SPEC.md for the design. Router API credentials are a secret: they are read
// from MORPHEUS_ROUTER_AUTH ("user:pass") or the router .cookie file, never from a
// command-line flag.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultHost       = "localhost:8082"
	defaultModel      = "kimi-k2.5"
	defaultModelID    = "0xbb9e920d94ad3fa2861e1e209d0a969dbe9e1af1cf1ad95c49f76d7b63d32d93"
	defaultPrompt     = "Hello!"
	defaultDuration   = 600 // seconds (10 minutes)
	defaultCookie     = "./.cookie"
	sessionCacheFile  = ".morpheus-doctor-session.json"
	lowMorWarningMOR  = 5.0 // heads-up threshold, not a hard block
	sessionSafetyMarg = 30 * time.Second
)

type config struct {
	host        string
	model       string
	modelID     string
	prompt      string
	duration    int
	dev         bool
	jsonOut     bool
	cookie      string
	timeout     time.Duration
	interactive bool
	fresh       bool
}

func main() {
	cfg := parseFlags()
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "✖ "+err.Error())
		os.Exit(1)
	}
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.host, "host", defaultHost, "proxy-router API host:port")
	flag.StringVar(&cfg.model, "model", defaultModel, "model name for the completion request")
	flag.StringVar(&cfg.modelID, "model-id", defaultModelID, "on-chain model id (0x...)")
	flag.StringVar(&cfg.prompt, "prompt", defaultPrompt, "prompt to send in --dev mode")
	flag.IntVar(&cfg.duration, "duration", defaultDuration, "session duration in seconds")
	flag.BoolVar(&cfg.dev, "dev", false, "developer flow: open a session and run inference")
	flag.BoolVar(&cfg.jsonOut, "json", false, "also print raw JSON responses")
	flag.StringVar(&cfg.cookie, "cookie", cookieDefault(), "path to the router .cookie file")
	flag.BoolVar(&cfg.interactive, "interactive", false, "keep the session open for multiple prompts (type 'exit' to end)")
	flag.BoolVar(&cfg.fresh, "fresh", false, "ignore any cached session and open a new one")
	flag.Parse()
	cfg.timeout = 60 * time.Second
	return cfg
}

func cookieDefault() string {
	if p := os.Getenv("COOKIE_FILE_PATH"); p != "" {
		return p
	}
	return defaultCookie
}

func (c config) baseURL() string {
	h := c.host
	if !strings.HasPrefix(h, "http://") && !strings.HasPrefix(h, "https://") {
		h = "http://" + h
	}
	return strings.TrimRight(h, "/")
}

// loadAuth returns the router API user and password from MORPHEUS_ROUTER_AUTH or
// the .cookie file (format "user:pass"). It never accepts credentials via a flag.
func loadAuth(cfg config) (user, pass string, err error) {
	raw := os.Getenv("MORPHEUS_ROUTER_AUTH")
	src := "MORPHEUS_ROUTER_AUTH"
	if raw == "" {
		b, e := os.ReadFile(cfg.cookie)
		if e != nil {
			return "", "", fmt.Errorf("no router credentials: set MORPHEUS_ROUTER_AUTH=user:pass, or point --cookie at the router .cookie file (%v)", e)
		}
		raw = string(b)
		src = cfg.cookie
	}
	u, p, ok := strings.Cut(strings.TrimSpace(raw), ":")
	if !ok || u == "" {
		return "", "", fmt.Errorf("credentials from %s are not in user:pass form", src)
	}
	return u, p, nil
}

type client struct {
	http *http.Client
	base string
	user string
	pass string
	json bool
}

func newClient(cfg config) (*client, error) {
	u, p, err := loadAuth(cfg)
	if err != nil {
		return nil, err
	}
	return &client{
		http: &http.Client{Timeout: cfg.timeout},
		base: cfg.baseURL(),
		user: u,
		pass: p,
		json: cfg.jsonOut,
	}, nil
}

// do performs an authenticated JSON request. out (if non-nil) is decoded from the
// response body. Any transport error propagates; any non-2xx status is an error.
func (c *client) do(method, path string, headers map[string]string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.user, c.pass)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if c.json {
		fmt.Printf("  %s %s -> %s\n  %s\n", method, path, resp.Status, snippet(data))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s -> %s: %s", method, path, resp.Status, snippet(data))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return nil
}

// reachable reports whether the router answers HTTP at all. Any HTTP response
// (even 401/404) means the router is up; only a transport error means it is down.
func (c *client) reachable() error {
	req, err := http.NewRequest(http.MethodGet, c.base+"/healthcheck", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.user, c.pass)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

type balanceResp struct {
	MOR string `json:"mor"`
	ETH string `json:"eth"`
}

// balance fetches the router wallet's on-chain MOR/ETH balance (wei strings).
func (c *client) balance() (*balanceResp, error) {
	var b balanceResp
	if err := c.do(http.MethodGet, "/blockchain/balance", nil, nil, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// weiToFloat parses a wei-denominated integer string (arbitrarily large) into a
// *big.Float scaled to whole tokens. Using math/big avoids the precision loss a
// plain float64 conversion would hit on 18-decimal wei values.
func weiToFloat(wei string) (*big.Float, error) {
	i, ok := new(big.Int).SetString(wei, 10)
	if !ok {
		return nil, fmt.Errorf("not a valid integer: %q", wei)
	}
	f := new(big.Float).SetInt(i)
	return f.Quo(f, big.NewFloat(1e18)), nil
}

// formatWei renders a wei string as a 4-decimal token amount, or the raw string
// if it can't be parsed (better to show something than to error the whole run).
func formatWei(wei string) string {
	f, err := weiToFloat(wei)
	if err != nil {
		return wei
	}
	return f.Text('f', 4)
}

func run(cfg config) error {
	c, err := newClient(cfg)
	if err != nil {
		return err
	}
	fmt.Printf("morpheus-doctor  ->  %s\n", c.base)

	if err := c.reachable(); err != nil {
		fmt.Println("✖ router not reachable")
		fmt.Println("  Start Morpheus first, then re-run:")
		fmt.Println("   - Installer build: open the app from the Start-menu/desktop shortcut (router auto-starts)")
		fmt.Println("   - Archive build:   run  mor-launch.exe  in the extracted folder")
		return fmt.Errorf("router unreachable at %s (%v)", c.base, err)
	}
	fmt.Println("✔ router reachable")

	if bal, err := c.balance(); err != nil {
		fmt.Printf("  (wallet balance unavailable: %v)\n", err)
	} else {
		fmt.Printf("  wallet: %s MOR, %s ETH\n", formatWei(bal.MOR), formatWei(bal.ETH))
		if morF, err := weiToFloat(bal.MOR); err == nil {
			if f, _ := morF.Float64(); f < lowMorWarningMOR {
				fmt.Printf("  ⚠ low MOR balance — sessions require a minimum stake (~%.0f MOR)\n", lowMorWarningMOR)
			}
		}
	}

	if !cfg.dev {
		fmt.Println("\nPreflight OK. Re-run with --dev to open a session and run inference.")
		return nil
	}
	return devFlow(c, cfg)
}

type sessionResp struct {
	SessionID string `json:"sessionID"`
}

type chatResp struct {
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// sessionCache is the on-disk record of the last session morpheus-doctor opened,
// stored as a small JSON file next to the binary so --dev/--interactive can reuse
// a still-live session across separate invocations instead of always opening (and
// staking for) a new one.
type sessionCache struct {
	SessionID string `json:"sessionID"`
	ModelID   string `json:"modelID"`
	Model     string `json:"model"`
	OpenedAt  int64  `json:"openedAt"` // unix seconds
	Duration  int    `json:"duration"` // seconds
}

func (s sessionCache) expiresAt() time.Time {
	return time.Unix(s.OpenedAt, 0).Add(time.Duration(s.Duration) * time.Second)
}

// valid reports whether the cached session is for the requested model and has
// enough time left to be worth reusing (a safety margin avoids handing back a
// session that expires mid-request).
func (s sessionCache) valid(modelID string) bool {
	if s.SessionID == "" || s.ModelID != modelID {
		return false
	}
	return time.Now().Add(sessionSafetyMarg).Before(s.expiresAt())
}

func loadSessionCache() (sessionCache, bool) {
	var s sessionCache
	b, err := os.ReadFile(sessionCacheFile)
	if err != nil {
		return s, false
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, false
	}
	return s, true
}

// saveSessionCache writes best-effort; a failure to cache shouldn't fail the run.
func saveSessionCache(s sessionCache) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(sessionCacheFile, b, 0o600)
}

// devFlow reuses a cached session if one is valid for this model (unless --fresh),
// otherwise opens a new one and caches it. Either way it then runs inference or
// hands off to the interactive loop -- replacing the manual open-session /
// copy-id / run-inference chain (AUDIT F3/F4).
func devFlow(c *client, cfg config) error {
	var sessionID string
	var exp time.Time

	if !cfg.fresh {
		if cached, ok := loadSessionCache(); ok && cached.valid(cfg.modelID) {
			sessionID = cached.SessionID
			exp = cached.expiresAt()
			fmt.Printf("\n✔ reusing cached session: %s (%s remaining)\n", short(sessionID), remaining(exp))
		}
	}

	if sessionID == "" {
		fmt.Printf("\n> opening session (model %s, %ds)...\n", cfg.model, cfg.duration)
		var sess sessionResp
		err := c.do(http.MethodPost, "/blockchain/models/"+cfg.modelID+"/session", nil,
			map[string]any{"sessionDuration": cfg.duration}, &sess)
		if err != nil {
			return fmt.Errorf("open session: %w", err)
		}
		if sess.SessionID == "" {
			return errors.New("open session: response contained no sessionID")
		}
		sessionID = sess.SessionID
		exp = time.Now().Add(time.Duration(cfg.duration) * time.Second)
		fmt.Printf("✔ session opened: %s\n", sessionID)
		saveSessionCache(sessionCache{
			SessionID: sessionID,
			ModelID:   cfg.modelID,
			Model:     cfg.model,
			OpenedAt:  time.Now().Unix(),
			Duration:  cfg.duration,
		})
	}

	if cfg.interactive {
		return interactiveLoop(c, cfg, sessionID)
	}

	fmt.Println("> running inference...")
	reply, err := c.chat(sessionID, cfg.model, cfg.modelID,
		[]map[string]string{{"role": "user", "content": cfg.prompt}})
	if err != nil {
		return fmt.Errorf("inference: %w", err)
	}
	fmt.Println("✔ inference ok")
	fmt.Printf("\n  %s\n", reply)
	fmt.Printf("\n✅ session %s live (%s remaining)\n", short(sessionID), remaining(exp))
	return nil
}

// interactiveLoop sends repeated prompts against the same session, maintaining
// the full message history client-side and resending it each turn (OpenAI-style
// chat semantics) -- the router does not retain conversation state on its own.
func interactiveLoop(c *client, cfg config, sessionID string) error {
	fmt.Printf("\n> session live. Type a message and press Enter (type 'exit' to end early).\n\n")
	scanner := bufio.NewScanner(os.Stdin)
	var history []map[string]string
	for {
		fmt.Print("you> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			break
		}
		history = append(history, map[string]string{"role": "user", "content": line})
		reply, err := c.chat(sessionID, cfg.model, cfg.modelID, history)
		if err != nil {
			fmt.Fprintln(os.Stderr, "✖ "+err.Error())
			history = history[:len(history)-1] // don't poison history with a failed turn
			continue
		}
		history = append(history, map[string]string{"role": "assistant", "content": reply})
		fmt.Printf("model> %s\n\n", reply)
	}
	fmt.Printf("✅ session %s ended locally (stake refunds automatically at natural expiry; cached for reuse until then)\n", short(sessionID))
	return nil
}

// chat sends the full message history against an already-open session and
// returns the latest reply text.
func (c *client) chat(sessionID, model, modelID string, messages []map[string]string) (string, error) {
	var chat chatResp
	err := c.do(http.MethodPost, "/v1/chat/completions",
		map[string]string{"session_id": sessionID, "model_id": modelID},
		map[string]any{
			"model":    model,
			"messages": messages,
			"stream":   false,
		}, &chat)
	if err != nil {
		return "", err
	}
	if len(chat.Choices) == 0 {
		return "", errors.New("empty response")
	}
	return strings.TrimSpace(chat.Choices[0].Message.Content), nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10] + "..."
	}
	return s
}

// remaining formats the time left until exp as "XmYs", or "expired" if past.
func remaining(exp time.Time) string {
	d := time.Until(exp)
	if d < 0 {
		return "expired"
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}
