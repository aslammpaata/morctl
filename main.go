// Command morctl diagnoses a local Morpheus proxy-router and, with --dev,
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
	defaultPrompt     = "Hello!"
	defaultDuration   = 600 // seconds (10 minutes)
	defaultCookie     = "./.cookie"
	sessionCacheFile  = ".morctl-session.json"
	lowMorWarningMOR  = 5.0 // heads-up threshold, not a hard block
	sessionListLimit  = 20  // most recent sessions shown by --sessions
	sessionSafetyMarg = 30 * time.Second
	baseChainID       = int64(8453) // Base mainnet
	baseChainIDHex    = "0x2105"    // 8453, as an RPC reports it
)

type config struct {
	host         string
	model        string
	modelID      string
	prompt       string
	duration     int
	dev          bool
	jsonOut      bool
	cookie       string
	timeout      time.Duration
	interactive  bool
	fresh        bool
	listModels   bool
	noHistory    bool
	verbose      bool
	sessions     bool
	resolveNames bool
	stakes       bool
	claimStakes  bool
	confirm      bool
	diagnose     bool
	cacheFile    string
}

func main() {
	cfg := parseFlags()
	if err := run(cfg); err != nil {
		renderError(os.Stderr, err, cfg)
		os.Exit(1)
	}
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.host, "host", defaultHost, "proxy-router API host:port")
	flag.StringVar(&cfg.model, "model", "", "marketplace model name (resolved to an id; names are not unique)")
	flag.StringVar(&cfg.modelID, "model-id", "", "on-chain model id (0x..., exact -- preferred over --model)")
	flag.StringVar(&cfg.prompt, "prompt", defaultPrompt, "prompt to send in --dev mode")
	flag.IntVar(&cfg.duration, "duration", defaultDuration, "session duration in seconds")
	flag.BoolVar(&cfg.dev, "dev", false, "developer flow: open a session and run inference")
	flag.BoolVar(&cfg.jsonOut, "json", false, "also print raw JSON responses")
	flag.StringVar(&cfg.cookie, "cookie", cookieDefault(), "path to the router .cookie file")
	flag.BoolVar(&cfg.interactive, "interactive", false, "keep the session open for multiple prompts (type 'exit' to end)")
	flag.BoolVar(&cfg.fresh, "fresh", false, "ignore any cached session and open a new one")
	flag.BoolVar(&cfg.listModels, "list-models", false, "list marketplace models and exit (narrow with --model)")
	flag.BoolVar(&cfg.noHistory, "no-history", false, "do not store or restore conversation history in the session cache")
	flag.BoolVar(&cfg.verbose, "verbose", false, "show the raw router response alongside translated errors")
	flag.BoolVar(&cfg.sessions, "sessions", false, "list this wallet's recent sessions and exit")
	flag.BoolVar(&cfg.resolveNames, "resolve-names", false, "resolve model ids to names in --sessions (adds a ~30s marketplace fetch)")
	flag.BoolVar(&cfg.stakes, "stakes", false, "show MOR stake on hold for this wallet and exit")
	flag.BoolVar(&cfg.claimStakes, "claim-stakes", false, "withdraw releasable stake (requires --confirm; sends a transaction)")
	flag.BoolVar(&cfg.confirm, "confirm", false, "confirm an action that sends a transaction")
	flag.BoolVar(&cfg.diagnose, "diagnose", false, "check router, credentials, chain, RPC, funds and model health, then exit")
	flag.Parse()
	cfg.timeout = 60 * time.Second
	cfg.cacheFile = sessionCacheFile
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

	modelCache []modelEntry // populated lazily by models()
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
		return &routerError{
			method: method,
			path:   path,
			status: resp.StatusCode,
			reason: resp.Status,
			body:   snippet(data),
		}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return nil
}

// routerError is a non-2xx response from the proxy-router. It keeps the status
// and body intact so the raw detail is still available under --verbose/--json
// while the default output can show a plain-language cause and fix instead.
type routerError struct {
	method string
	path   string
	status int
	reason string // the HTTP status line, e.g. "429 Too Many Requests"
	body   string
}

func (e *routerError) Error() string {
	return fmt.Sprintf("%s %s -> %s: %s", e.method, e.path, e.reason, e.body)
}

// diagnosis is a plain-language reading of a router error: what actually went
// wrong, and what to do about it.
type diagnosis struct {
	cause string
	fix   string
}

// errorRules maps router failures to plain language. Matching is on status plus
// a body substring, never on an exact body, because the router's wording and
// the wording of whatever it is proxying both change independently of morctl.
// Order matters: the most specific rule wins.
var errorRules = []struct {
	// status 0 matches any status; needles are matched case-insensitively and
	// any one of them is enough.
	status  int
	needles []string
	pathHas string
	diagnosis
}{
	{
		status:  404,
		pathHas: "/blockchain/stakes/",
		diagnosis: diagnosis{
			cause: "this proxy-router does not have the stake endpoints.",
			fix:   "They were added in node v7.11.0 -- upgrade the router, or claim manually as the guides describe.",
		},
	},
	{
		status:  409,
		pathHas: "/blockchain/stakes/withdraw",
		diagnosis: diagnosis{
			cause: "a stake withdrawal is already in flight on this router.",
			fix:   "Wait for it to finish, then check the on-hold balance again before retrying.",
		},
	},
	{
		status:  429,
		needles: nil,
		diagnosis: diagnosis{
			cause: "the blockchain RPC endpoint is rate-limiting the router. The public Base RPCs are shared and throttle quickly.",
			fix:   "Point the router at a dedicated RPC (ETH_NODE_ADDRESS, or POST /config/ethNode), then retry.",
		},
	},
	{
		needles: []string{"over rate limit", "rate limit", "too many requests", "429"},
		diagnosis: diagnosis{
			cause: "the blockchain RPC endpoint is rate-limiting the router. The public Base RPCs are shared and throttle quickly.",
			fix:   "Point the router at a dedicated RPC (ETH_NODE_ADDRESS, or POST /config/ethNode), then retry.",
		},
	},
	{
		status: 410,
		diagnosis: diagnosis{
			cause: "an upstream RPC endpoint answered 410 Gone -- that endpoint is dead, not merely busy.",
			fix:   "Replace it: check ETH_NODE_ADDRESS on the router, or POST /config/ethNode with a working URL.",
		},
	},
	{
		needles: []string{"410 gone"},
		diagnosis: diagnosis{
			cause: "an upstream RPC endpoint answered 410 Gone -- that endpoint is dead, not merely busy.",
			fix:   "Replace it: check ETH_NODE_ADDRESS on the router, or POST /config/ethNode with a working URL.",
		},
	},
	{
		needles: []string{"insufficient", "not enough", "exceeds balance", "transfer amount exceeds"},
		diagnosis: diagnosis{
			cause: "the wallet does not hold enough MOR to stake for this session.",
			fix:   "Fund the wallet, shorten the session with --duration, or claim any stake on hold (morctl --stakes).",
		},
	},
	{
		needles: []string{"no bid", "bid not found", "no active bid", "no provider"},
		diagnosis: diagnosis{
			cause: "no provider is currently bidding on this model, so there is nothing to open a session against.",
			fix:   "Pick a different model (morctl --list-models), or check GET /healthcheck for this model's status.",
		},
	},
	{
		needles: []string{"provider disconnect", "provider unreachable", "connection refused", "dial tcp", "unexpected eof", "connection reset", "no such host"},
		diagnosis: diagnosis{
			cause: "the provider serving this session dropped the connection.",
			fix:   "Retry -- the session is still open and paid for. If it keeps failing, the provider is unhealthy; choose another model.",
		},
	},
	{
		status: 401,
		diagnosis: diagnosis{
			cause: "the router rejected the credentials.",
			fix:   "The .cookie file is regenerated when the router restarts: re-read it (--cookie / COOKIE_FILE_PATH) or re-export MORPHEUS_ROUTER_AUTH.",
		},
	},
	{
		status: 403,
		diagnosis: diagnosis{
			cause: "these credentials are not allowed to perform that action.",
			fix:   "A scoped router user needs the matching permission (open_session, chat, get_stakes_on_hold, withdraw_user_stakes).",
		},
	},
	{
		status: 402,
		diagnosis: diagnosis{
			cause: "the provider's own model backend refused the request for payment reasons.",
			fix:   "That is the provider's billing, not yours -- choose another model.",
		},
	},
}

// friendly returns a plain-language reading of the error, if one is known.
func (e *routerError) friendly() (diagnosis, bool) {
	body := strings.ToLower(e.body)
	for _, rule := range errorRules {
		if rule.status != 0 && rule.status != e.status {
			continue
		}
		if rule.pathHas != "" && !strings.Contains(e.path, rule.pathHas) {
			continue
		}
		if len(rule.needles) > 0 {
			matched := false
			for _, n := range rule.needles {
				if strings.Contains(body, n) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		return rule.diagnosis, true
	}
	return diagnosis{}, false
}

// renderError writes an error to stderr, translating router failures into a
// cause and a fix where one is known. The raw error is always shown under
// --verbose or --json, and is shown anyway when there is no translation, so
// nothing is ever hidden from the person debugging.
func renderError(w io.Writer, err error, cfg config) {
	var re *routerError
	if errors.As(err, &re) {
		if d, ok := re.friendly(); ok {
			fmt.Fprintf(w, "✖ %s\n", d.cause)
			fmt.Fprintf(w, "  → %s\n", d.fix)
			if cfg.verbose || cfg.jsonOut {
				fmt.Fprintf(w, "  raw: %v\n", err)
			} else {
				fmt.Fprintf(w, "  (%s %s -> %s; --verbose for the full response)\n", re.method, re.path, re.reason)
			}
			return
		}
	}
	fmt.Fprintf(w, "✖ %v\n", err)
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

// modelEntry is one marketplace model from GET /blockchain/models. Only the
// fields morctl needs are decoded. Note IsDeleted: this field was DeletedAt in
// older routers, which is why mor-cli's deleted-model filter matches nothing
// against a current router.
type modelEntry struct {
	ID        string `json:"Id"`
	Name      string `json:"Name"`
	ModelType string `json:"ModelType"`
	IsDeleted bool   `json:"IsDeleted"`
}

// models fetches the marketplace model list, drops deleted entries and caches
// the result for the process. The list is large (517 live models on mainnet
// when this was written) and the call takes tens of seconds, so it is only
// fetched when a name actually has to be resolved.
func (c *client) models() ([]modelEntry, error) {
	if c.modelCache != nil {
		return c.modelCache, nil
	}
	var resp struct {
		Models []modelEntry `json:"models"`
	}
	if err := c.do(http.MethodGet, "/blockchain/models", nil, nil, &resp); err != nil {
		return nil, err
	}
	live := make([]modelEntry, 0, len(resp.Models))
	for _, m := range resp.Models {
		if !m.IsDeleted {
			live = append(live, m)
		}
	}
	c.modelCache = live
	return live, nil
}

// looksLikeModelID reports whether s has the shape of an on-chain model id:
// 0x followed by 64 hex digits. Catching a typo here is cheaper than letting
// the router reject it once the session request is already in flight.
func looksLikeModelID(s string) bool {
	if len(s) != 66 || !strings.HasPrefix(s, "0x") {
		return false
	}
	for _, r := range s[2:] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// errNoModel is returned when neither --model nor --model-id was given. morctl
// deliberately has no default model: opening a session stakes MOR, so the
// choice has to be the user's (see SPEC.md, "No default model").
var errNoModel = errors.New("no model selected -- morctl will not pick one for you, because opening a session stakes MOR.\n" +
	"  Pass one of:\n" +
	"    --model-id 0x<64 hex>   on-chain model id (exact, preferred)\n" +
	"    --model <name>          marketplace name, resolved to an id (names are not unique)\n" +
	"  To see what is available:  morctl --list-models")

// resolveModel determines which model the session will run against and returns
// a short note on where that choice came from, so the run can say so out loud
// instead of quietly picking something the user did not ask for.
func resolveModel(c *client, cfg config) (model, modelID, why string, err error) {
	switch {
	case cfg.modelID != "":
		if !looksLikeModelID(cfg.modelID) {
			return "", "", "", fmt.Errorf("--model-id %q is not an 0x-prefixed 32-byte id", cfg.modelID)
		}
		if cfg.model != "" {
			// Both given: --model is only the label on the completion request,
			// so it is taken at face value rather than costing a lookup.
			return cfg.model, cfg.modelID, "--model-id, labelled with --model", nil
		}
		// Label the request with the real name when that is cheap to learn,
		// but never fail the run over what is a cosmetic field.
		if name, lookupErr := c.modelNameFor(cfg.modelID); lookupErr == nil && name != "" {
			return name, cfg.modelID, "--model-id, name looked up on the marketplace", nil
		}
		return cfg.modelID, cfg.modelID, "--model-id", nil

	case cfg.model != "":
		id, err := c.findModelID(cfg.model)
		if err != nil {
			return "", "", "", err
		}
		return cfg.model, id, fmt.Sprintf("--model %q resolved to an id", cfg.model), nil

	default:
		return "", "", "", errNoModel
	}
}

// modelNameFor returns the marketplace name for a model id, or "" if unknown.
func (c *client) modelNameFor(id string) (string, error) {
	ms, err := c.models()
	if err != nil {
		return "", err
	}
	for _, m := range ms {
		if strings.EqualFold(m.ID, id) {
			return m.Name, nil
		}
	}
	return "", nil
}

// findModelID resolves a marketplace model name to its on-chain id. Names are
// not unique -- 58 of 517 live names were duplicated on mainnet when this was
// written -- so an ambiguous name is an error, never a silent first match.
func (c *client) findModelID(name string) (string, error) {
	if c.modelCache == nil {
		fmt.Printf("> resolving model %q against the marketplace (this can take ~30s)...\n", name)
	}
	ms, err := c.models()
	if err != nil {
		return "", fmt.Errorf("could not list models to resolve --model %q: %w", name, err)
	}
	var matches []modelEntry
	for _, m := range ms {
		if strings.EqualFold(m.Name, name) {
			matches = append(matches, m)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].ID, nil
	case 0:
		var near []string
		lower := strings.ToLower(name)
		for _, m := range ms {
			if strings.Contains(strings.ToLower(m.Name), lower) {
				near = append(near, m.Name)
			}
			if len(near) == 5 {
				break
			}
		}
		if len(near) > 0 {
			return "", fmt.Errorf("no model is named exactly %q; close matches: %s", name, strings.Join(near, ", "))
		}
		return "", fmt.Errorf("no model named %q on the marketplace (try: morctl --list-models)", name)
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.ID)
		}
		return "", fmt.Errorf("%d live models are named %q -- pick one with --model-id:\n    %s",
			len(matches), name, strings.Join(ids, "\n    "))
	}
}

// listModels prints the live marketplace models, narrowed by --model if given.
func listModels(c *client, cfg config) error {
	ms, err := c.models()
	if err != nil {
		return err
	}
	filter := strings.ToLower(cfg.model)
	shown := 0
	for _, m := range ms {
		if filter != "" && !strings.Contains(strings.ToLower(m.Name), filter) {
			continue
		}
		kind := m.ModelType
		if kind == "" || kind == "UNKNOWN" {
			kind = "-"
		}
		fmt.Printf("  %-44s  %s  %s\n", truncate(m.Name, 44), m.ID, kind)
		shown++
	}
	if shown == 0 {
		fmt.Printf("  no live model name contains %q (%d models listed)\n", cfg.model, len(ms))
		return nil
	}
	fmt.Printf("\n  %d model(s). Use the id with --model-id.\n", shown)
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// sessionListItem is one on-chain session from GET /blockchain/sessions/user.
// The router marshals its Go struct without json tags, hence the capitalised
// keys. Stake and PricePerSecond are JSON *numbers* in wei and routinely
// exceed what a float64 holds exactly, so they are kept as json.Number and
// formatted through the same big.Int path as every other wei value.
type sessionListItem struct {
	ID             string      `json:"Id"`
	ModelAgentID   string      `json:"ModelAgentId"`
	Provider       string      `json:"Provider"`
	Stake          json.Number `json:"Stake"`
	PricePerSecond json.Number `json:"PricePerSecond"`
	OpenedAt       int64       `json:"OpenedAt"`
	EndsAt         int64       `json:"EndsAt"`
	ClosedAt       int64       `json:"ClosedAt"`
}

// live reports whether the session is still usable for inference.
func (s sessionListItem) live() bool {
	return s.ClosedAt == 0 && s.EndsAt > time.Now().Unix()
}

// status describes the session in the three states that matter to a user:
// usable now, finished but still holding stake, or settled.
func (s sessionListItem) status() string {
	switch {
	case s.ClosedAt > 0:
		return "closed"
	case s.EndsAt <= time.Now().Unix():
		return "expired"
	default:
		return "LIVE"
	}
}

// walletAddress returns the address of the wallet the router is using. It is
// needed because the session endpoints are queried by user address, and asking
// the user to supply their own address (as mor-cli does) is a needless step.
func (c *client) walletAddress() (string, error) {
	var w struct {
		Address string `json:"address"`
	}
	if err := c.do(http.MethodGet, "/wallet", nil, nil, &w); err != nil {
		return "", err
	}
	if w.Address == "" {
		return "", errors.New("the router reported no wallet address -- is a wallet set up?")
	}
	return w.Address, nil
}

func (c *client) userSessions(addr string, limit int) ([]sessionListItem, error) {
	var resp struct {
		Sessions []sessionListItem `json:"sessions"`
	}
	path := fmt.Sprintf("/blockchain/sessions/user?user=%s&limit=%d&order=DESC", addr, limit)
	if err := c.do(http.MethodGet, path, nil, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

// showSessions lists this wallet's recent sessions with the three facts that
// make overlapping sessions legible: which model, how long is left, and how
// much MOR is escrowed in each.
func showSessions(c *client, cfg config) error {
	addr, err := c.walletAddress()
	if err != nil {
		return fmt.Errorf("look up wallet: %w", err)
	}
	sessions, err := c.userSessions(addr, sessionListLimit)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	fmt.Printf("\n> sessions for %s\n\n", addr)
	if len(sessions) == 0 {
		fmt.Println("  none on record for this wallet.")
		return nil
	}

	// Names cost a full marketplace fetch, so they are opt-in.
	names := map[string]string{}
	if cfg.resolveNames {
		fmt.Println("  (resolving model names, this can take ~30s...)")
		if ms, err := c.models(); err == nil {
			for _, m := range ms {
				names[strings.ToLower(m.ID)] = m.Name
			}
		} else {
			fmt.Println("  (model names unavailable; showing ids)")
		}
	}

	var liveCount int
	escrowed := new(big.Int)
	for _, s := range sessions {
		model := short(s.ModelAgentID)
		if n, ok := names[strings.ToLower(s.ModelAgentID)]; ok {
			model = truncate(n, 28)
		}
		timing := remaining(time.Unix(s.EndsAt, 0))
		if s.status() != "LIVE" {
			timing = "ended " + time.Unix(s.EndsAt, 0).Format("2006-01-02 15:04")
		} else {
			timing += " left"
			liveCount++
			if v, ok := new(big.Int).SetString(s.Stake.String(), 10); ok {
				escrowed.Add(escrowed, v)
			}
		}
		fmt.Printf("  %-13s %-7s %-28s %10s MOR  %s\n",
			short(s.ID), s.status(), model, formatWei(s.Stake.String()), timing)
	}

	fmt.Printf("\n  %d session(s) listed, %d live", len(sessions), liveCount)
	if liveCount > 0 {
		fmt.Printf(", %s MOR escrowed in them", formatWei(escrowed.String()))
	}
	fmt.Println(".")
	if !cfg.resolveNames {
		fmt.Println("  (--resolve-names shows model names instead of ids)")
	}
	if len(sessions) == sessionListLimit {
		fmt.Printf("  (showing the %d most recent; there may be more)\n", sessionListLimit)
	}
	return nil
}

// stakesOnHold is GET /blockchain/stakes/onhold. Both figures are wei strings.
//
// Available is stake whose 24h lock has elapsed and can be withdrawn now;
// Hold is stake still inside that window. The lock exists because closing a
// session early pushes the refund into userStakesOnHold with a releaseAt of
// closedAt + 1 day (SessionRouter.sol) -- which is exactly why morctl lets
// sessions expire naturally rather than closing them.
type stakesOnHold struct {
	Available string `json:"available"`
	Hold      string `json:"hold"`
}

// any reports whether there is any stake worth mentioning.
func (s stakesOnHold) any() bool {
	return s.Available != "" && s.Available != "0" || s.Hold != "" && s.Hold != "0"
}

func (s stakesOnHold) claimable() bool {
	return s.Available != "" && s.Available != "0"
}

// stakes reads the consumer stake held in the Diamond for the router's wallet.
// This needs no private key and no direct RPC: the router exposes it (node
// v7.11.0 and later, upstream issue #827). A 404 means an older router.
func (c *client) stakes() (stakesOnHold, error) {
	var s stakesOnHold
	err := c.do(http.MethodGet, "/blockchain/stakes/onhold?iterations=255", nil, nil, &s)
	return s, err
}

// withdrawStakes claims releasable stake. The router's own wallet signs the
// transaction -- morctl never reads, stores or transmits a private key.
func (c *client) withdrawStakes() (string, error) {
	var res struct {
		Tx string `json:"tx"`
	}
	err := c.do(http.MethodPost, "/blockchain/stakes/withdraw", nil,
		map[string]any{"iterations": 255}, &res)
	if err != nil {
		return "", err
	}
	return res.Tx, nil
}

// showStakes reports stake on hold for this wallet.
func showStakes(c *client, cfg config) error {
	s, err := c.stakes()
	if err != nil {
		return fmt.Errorf("read stakes on hold: %w", err)
	}
	fmt.Println()
	fmt.Printf("  claimable now: %s MOR\n", formatWei(s.Available))
	fmt.Printf("  still locked:  %s MOR\n", formatWei(s.Hold))
	switch {
	case s.claimable():
		fmt.Println("\n  Claim it with:  morctl --claim-stakes --confirm")
	case s.Hold != "" && s.Hold != "0":
		fmt.Println("\n  Locked stake is released 24h after the session that produced it closed early.")
	default:
		fmt.Println("\n  Nothing on hold. Sessions left to expire naturally refund without a 24h lock.")
	}
	return nil
}

// claimStakes withdraws releasable stake. This is the one morctl command that
// sends a state-changing, gas-spending transaction, so it is gated twice: an
// explicit --confirm flag, and -- on a terminal -- a typed confirmation. It
// refuses outright when there is nothing to claim, rather than burning gas on
// a transaction the contract would revert.
func claimStakes(c *client, cfg config) error {
	s, err := c.stakes()
	if err != nil {
		return fmt.Errorf("read stakes on hold before claiming: %w", err)
	}
	fmt.Println()
	fmt.Printf("  claimable now: %s MOR\n", formatWei(s.Available))
	fmt.Printf("  still locked:  %s MOR\n", formatWei(s.Hold))

	if !s.claimable() {
		fmt.Println()
		if s.Hold != "" && s.Hold != "0" {
			return errors.New("nothing to claim yet: the stake on hold is still inside its 24h lock")
		}
		return errors.New("nothing to claim: this wallet has no stake on hold")
	}

	if !cfg.confirm {
		fmt.Println()
		return fmt.Errorf("refusing to send a transaction without confirmation: re-run with --claim-stakes --confirm to claim %s MOR", formatWei(s.Available))
	}

	// On a terminal, make the person say it out loud. Piped input (CI, scripts)
	// is taken as already deliberate, since --confirm had to be passed.
	if isTerminal(os.Stdin) {
		fmt.Printf("\n  This sends a transaction from the router's wallet and costs gas.\n")
		fmt.Printf("  Type 'claim' to withdraw %s MOR: ", formatWei(s.Available))
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "claim" {
			return errors.New("cancelled -- nothing was sent")
		}
	}

	fmt.Println("\n> withdrawing stake...")
	tx, err := c.withdrawStakes()
	if err != nil {
		return fmt.Errorf("withdraw stake: %w", err)
	}
	fmt.Printf("✔ withdrawal submitted\n  tx: %s\n", tx)
	fmt.Println("  The balance updates once the transaction confirms on Base.")
	return nil
}

// isTerminal reports whether f is an interactive terminal, so prompts are only
// shown where someone can answer them. Uses the file mode rather than a
// dependency on a tty package.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// --- diagnose ---------------------------------------------------------------

// healthResp is GET /healthcheck. The endpoint is unauthenticated, so it still
// answers when the cookie is stale -- which is what makes it the right first
// check in a diagnosis. It answers 503 when degraded, so the body has to be
// read regardless of status.
type healthResp struct {
	Status     string            `json:"status"`
	Version    string            `json:"version"`
	Uptime     string            `json:"uptime"`
	Components map[string]string `json:"components"`
	Models     []struct {
		ModelID      string `json:"modelId"`
		ModelName    string `json:"modelName"`
		Status       string `json:"status"`
		ErrorKind    string `json:"errorKind"`
		HasActiveBid bool   `json:"hasActiveBid"`
		HTTPStatus   int    `json:"httpStatus"`
	} `json:"models"`
}

// healthcheck reads /healthcheck, tolerating the 503 the router returns when a
// component is unhealthy -- the body is the diagnosis we want in that case.
func (c *client) healthcheck() (healthResp, int, error) {
	var h healthResp
	req, err := http.NewRequest(http.MethodGet, c.base+"/healthcheck", nil)
	if err != nil {
		return h, 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return h, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return h, resp.StatusCode, err
	}
	if err := json.Unmarshal(data, &h); err != nil {
		return h, resp.StatusCode, fmt.Errorf("unexpected /healthcheck body: %s", snippet(data))
	}
	return h, resp.StatusCode, nil
}

// routerConfig is the part of GET /config a diagnosis needs.
type routerConfig struct {
	Version       string
	DerivedConfig struct {
		WalletAddress string
		ChainID       int64
		EthNodeURLs   []string
	}
	Config struct {
		Marketplace struct {
			DiamondContractAddress string
		}
	}
}

func (c *client) config() (routerConfig, error) {
	var rc routerConfig
	err := c.do(http.MethodGet, "/config", nil, nil, &rc)
	return rc, err
}

// probeRPC asks an RPC endpoint for its chain id. This is the one request
// morctl makes that does not go to the router: a rate-limited or dead RPC is
// invisible from the router's own API but is the single most common cause of
// session failures, and the default config ships seven shared public endpoints.
func probeRPC(url string, timeout time.Duration) (chainID string, status int, err error) {
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`)
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", resp.StatusCode, fmt.Errorf("%s: %s", resp.Status, snippet(data))
	}
	var out struct {
		Result string `json:"result"`
		Error  struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", resp.StatusCode, fmt.Errorf("unexpected RPC body: %s", snippet(data))
	}
	if out.Error.Message != "" {
		return "", resp.StatusCode, errors.New(out.Error.Message)
	}
	return out.Result, resp.StatusCode, nil
}

// check is one line of a diagnosis.
type check struct {
	name   string
	state  string // "ok", "warn" or "fail"
	detail string
	fix    string
}

type report struct {
	checks []check
	// unreachable records that the router itself could not be contacted, so
	// the absence of the other checks is expected rather than a clean result.
	unreachable bool
}

// find returns the check with the given name, for assertions and lookups.
func (r *report) find(name string) (check, bool) {
	for _, c := range r.checks {
		if c.name == name {
			return c, true
		}
	}
	return check{}, false
}

func (r *report) add(name, state, detail, fix string) {
	r.checks = append(r.checks, check{name: name, state: state, detail: detail, fix: fix})
}

func (r *report) ok(name, detail string)        { r.add(name, "ok", detail, "") }
func (r *report) warn(name, detail, fix string) { r.add(name, "warn", detail, fix) }
func (r *report) fail(name, detail, fix string) { r.add(name, "fail", detail, fix) }
func (r *report) failed() bool {
	for _, c := range r.checks {
		if c.state == "fail" {
			return true
		}
	}
	return false
}

func (r *report) print(w io.Writer) {
	symbol := map[string]string{"ok": "✔", "warn": "⚠", "fail": "✖"}
	for _, c := range r.checks {
		fmt.Fprintf(w, "  %s %-22s %s\n", symbol[c.state], c.name, c.detail)
		if c.fix != "" {
			fmt.Fprintf(w, "      → %s\n", c.fix)
		}
	}
}

// diagnose runs the checks that explain why session/inference work is failing,
// in dependency order, and never stops at the first problem: a broken cookie
// should not hide a wrong chain id.
func diagnose(c *client, cfg config) error {
	fmt.Printf("\n> diagnosing %s\n\n", c.base)
	r := runChecks(c, cfg)
	r.print(os.Stdout)
	fmt.Println()
	switch {
	case r.unreachable:
		return errors.New("router unreachable -- no further checks possible")
	case r.failed():
		return errors.New("diagnosis found problems (see ✖ above)")
	}
	fmt.Println("  No blocking problems found.")
	return nil
}

// runChecks performs the diagnosis and returns the report, so the outcome can
// be asserted on rather than only printed.
func runChecks(c *client, cfg config) *report {
	r := &report{}

	// 1. Router process. Unauthenticated, so this works with a bad cookie.
	h, status, err := c.healthcheck()
	switch {
	case err != nil && status == 0:
		r.fail("router", "not reachable: "+err.Error(),
			"Start the Desktop App, or run the proxy-router with your .env.")
		// Nothing else can be checked without the router.
		r.unreachable = true
		return r
	case err != nil:
		r.warn("router", fmt.Sprintf("answered %d but the body was unreadable", status), err.Error())
	case h.Status == "healthy":
		r.ok("router", fmt.Sprintf("healthy, %s, up %s", h.Version, h.Uptime))
	default:
		r.fail("router", fmt.Sprintf("%s (%s, up %s)", h.Status, h.Version, h.Uptime),
			"See the component detail below.")
	}

	// 2. Router components, e.g. the badger store that has corrupted before.
	for name, state := range h.Components {
		if strings.HasPrefix(state, "unhealthy") {
			r.fail("component "+name, state,
				"Local state may be corrupt; stopping the router and clearing its data directory has fixed this before.")
		} else {
			r.ok("component "+name, state)
		}
	}

	// 3. Credentials. Any authenticated endpoint will do; /wallet is cheap and
	// its answer is needed by later checks anyway.
	addr, walletErr := c.walletAddress()
	if walletErr != nil {
		var re *routerError
		if errors.As(walletErr, &re) && (re.status == 401 || re.status == 403) {
			d, _ := re.friendly()
			r.fail("credentials", "rejected by the router", d.fix)
		} else {
			r.warn("credentials", "could not be verified", walletErr.Error())
		}
	} else {
		r.ok("credentials", "accepted (wallet "+addr+")")
	}

	// 4. Chain and RPC configuration.
	rc, cfgErr := c.config()
	if cfgErr != nil {
		r.warn("router config", "unavailable", "Needs credentials with the system_config permission.")
	} else {
		if rc.DerivedConfig.ChainID == baseChainID {
			r.ok("chain id", fmt.Sprintf("%d (Base)", rc.DerivedConfig.ChainID))
		} else {
			r.fail("chain id", fmt.Sprintf("%d, expected %d (Base mainnet)", rc.DerivedConfig.ChainID, baseChainID),
				"Set ETH_NODE_CHAIN_ID=8453 and point ETH_NODE_ADDRESS at a Base RPC.")
		}

		urls := rc.DerivedConfig.EthNodeURLs
		switch {
		case len(urls) == 0:
			r.fail("rpc endpoints", "none configured",
				"Set ETH_NODE_ADDRESS, or POST /config/ethNode with a working URL.")
		default:
			r.ok("rpc endpoints", fmt.Sprintf("%d configured", len(urls)))
			// Probe the first endpoint only: this is a diagnosis, not a sweep,
			// and hammering shared endpoints is the problem we are looking for.
			chainHex, st, probeErr := probeRPC(urls[0], 10*time.Second)
			switch {
			case st == 429:
				r.fail("rpc reachable", urls[0]+" is rate-limiting us (429)",
					"These are shared public endpoints. Use a dedicated RPC (ETH_NODE_ADDRESS, or POST /config/ethNode).")
			case st == 410:
				r.fail("rpc reachable", urls[0]+" answered 410 Gone -- the endpoint is dead",
					"Replace it via ETH_NODE_ADDRESS or POST /config/ethNode.")
			case probeErr != nil:
				r.fail("rpc reachable", urls[0]+": "+probeErr.Error(),
					"Check the URL and this machine's outbound network access.")
			case chainHex != baseChainIDHex:
				r.warn("rpc reachable", fmt.Sprintf("%s reports chain %s, not %s (Base)", urls[0], chainHex, baseChainIDHex),
					"The RPC and the configured chain id disagree.")
			default:
				r.ok("rpc reachable", fmt.Sprintf("%s (chain %s)", urls[0], chainHex))
			}
		}
	}

	// 5. Funds: balance, allowance and stake on hold.
	if walletErr == nil {
		if bal, err := c.balance(); err != nil {
			r.warn("balance", "unavailable", "Usually the same cause as any credential or RPC failure above.")
		} else {
			morF, _ := weiToFloat(bal.MOR)
			mor, _ := morF.Float64()
			detail := fmt.Sprintf("%s MOR, %s ETH", formatWei(bal.MOR), formatWei(bal.ETH))
			if mor < lowMorWarningMOR {
				r.warn("balance", detail, fmt.Sprintf("Sessions stake MOR; under ~%.0f MOR some will fail to open.", lowMorWarningMOR))
			} else {
				r.ok("balance", detail)
			}
		}

		if cfgErr == nil {
			if diamond := rc.Config.Marketplace.DiamondContractAddress; diamond != "" {
				var a struct {
					Allowance json.Number `json:"allowance"`
				}
				if err := c.do(http.MethodGet, "/blockchain/allowance?spender="+diamond, nil, nil, &a); err != nil {
					r.warn("allowance", "unavailable", "Sessions need an allowance for the Diamond contract.")
				} else if a.Allowance.String() == "0" || a.Allowance.String() == "" {
					r.fail("allowance", "0 MOR approved for the Diamond contract",
						"Approve an allowance before opening sessions (mor-cli approveAllowance, or the Desktop App).")
				} else {
					r.ok("allowance", formatWei(a.Allowance.String())+" MOR approved for the Diamond")
				}
			}
		}

		if s, err := c.stakes(); err != nil {
			var re *routerError
			switch {
			case errors.As(err, &re) && re.status == 404:
				r.warn("stake on hold", "not exposed by this router",
					"Added in node v7.11.0; upgrade to see and claim it.")
			default:
				// Report it either way: silently dropping the check would make
				// a wallet with stuck stake look clean.
				r.warn("stake on hold", "could not be read", err.Error())
			}
		} else if s.claimable() {
			r.warn("stake on hold", formatWei(s.Available)+" MOR claimable", "Claim it: morctl --claim-stakes --confirm")
		} else if s.any() {
			r.ok("stake on hold", formatWei(s.Hold)+" MOR locked (released 24h after an early close)")
		} else {
			r.ok("stake on hold", "none")
		}
	}

	// 6. Per-model health, which the router has reported since 7.5.0 and which
	// neither CLI used to surface.
	var unhealthy, throttled int
	for _, m := range h.Models {
		if m.Status == "unhealthy" {
			unhealthy++
		}
		if m.ErrorKind == "rate_limited" {
			throttled++
		}
	}
	switch {
	case len(h.Models) == 0:
		// Consumer nodes report no models; only providers probe their own.
		r.ok("model health", "no per-model reports (normal for a consumer node)")
	case throttled > 0:
		r.warn("model health", fmt.Sprintf("%d of %d model(s) rate-limited", throttled, len(h.Models)),
			"The provider's backend is throttling; prefer another model.")
	case unhealthy > 0:
		r.warn("model health", fmt.Sprintf("%d of %d model(s) unhealthy", unhealthy, len(h.Models)),
			"Pick a model whose provider is healthy (morctl --list-models).")
	default:
		r.ok("model health", fmt.Sprintf("%d model(s) reporting, none unhealthy", len(h.Models)))
	}

	return r
}

func run(cfg config) error {
	c, err := newClient(cfg)
	if err != nil {
		return err
	}
	fmt.Printf("morctl  ->  %s\n", c.base)

	// --diagnose runs its own reachability check (via the unauthenticated
	// /healthcheck, so it still reports something useful when the cookie is
	// stale) and replaces the normal preflight output.
	if cfg.diagnose {
		return diagnose(c, cfg)
	}

	if err := c.reachable(); err != nil {
		fmt.Println("✖ router not reachable")
		fmt.Println("  Start Morpheus first, then re-run:")
		fmt.Println("   - Desktop App: open it from the Start-menu/desktop shortcut (the router auto-starts)")
		fmt.Println("   - Headless:    run the proxy-router binary with your .env")
		return fmt.Errorf("router unreachable at %s (%v)", c.base, err)
	}
	fmt.Println("✔ router reachable")

	balanceOK := true
	if bal, err := c.balance(); err != nil {
		balanceOK = false
		// A balance failure is not fatal to a preflight, but it is usually the
		// first place a stale cookie or a throttled RPC shows up, so say what
		// it means rather than echoing the status line.
		var re *routerError
		if errors.As(err, &re) {
			if d, ok := re.friendly(); ok {
				fmt.Printf("  ⚠ wallet balance unavailable: %s\n    → %s\n", d.cause, d.fix)
			} else {
				fmt.Printf("  ⚠ wallet balance unavailable (%s)\n", re.reason)
			}
		} else {
			fmt.Printf("  ⚠ wallet balance unavailable: %v\n", err)
		}
	} else {
		fmt.Printf("  wallet: %s MOR, %s ETH\n", formatWei(bal.MOR), formatWei(bal.ETH))
		if morF, err := weiToFloat(bal.MOR); err == nil {
			if f, _ := morF.Float64(); f < lowMorWarningMOR {
				fmt.Printf("  ⚠ low MOR balance — sessions require a minimum stake (~%.0f MOR)\n", lowMorWarningMOR)
			}
		}
	}

	// Surface stake sitting on hold during preflight: it is MOR the user owns
	// but cannot see anywhere else, and claiming it is often what unblocks a
	// wallet that looks short of funds. Silently skipped on routers that
	// predate the endpoint, and never fatal.
	if balanceOK {
		if s, err := c.stakes(); err == nil && s.any() {
			fmt.Printf("  stake on hold: %s MOR claimable, %s MOR locked\n",
				formatWei(s.Available), formatWei(s.Hold))
			if s.claimable() {
				fmt.Println("  → claim it with: morctl --claim-stakes --confirm")
			}
		}
	}

	if cfg.listModels {
		fmt.Println()
		return listModels(c, cfg)
	}

	if cfg.sessions {
		return showSessions(c, cfg)
	}

	if cfg.claimStakes {
		return claimStakes(c, cfg)
	}

	if cfg.stakes {
		return showStakes(c, cfg)
	}

	if !cfg.dev {
		if !balanceOK {
			// The router is up but we could not read the wallet, so this is not
			// a clean bill of health -- don't claim one.
			fmt.Println("\nRouter is reachable, but the wallet could not be read (see above).")
			return nil
		}
		fmt.Println("\nPreflight OK. Re-run with --dev --model-id 0x... to open a session and run inference.")
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

// message is one turn of an OpenAI-style chat exchange. It is both the wire
// shape sent to /v1/chat/completions and the on-disk shape in the session
// cache, so a resumed conversation replays exactly what was sent before.
type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// sessionCache is the on-disk record of the last session morctl opened,
// stored as a small JSON file next to the binary so --dev/--interactive can reuse
// a still-live session across separate invocations instead of always opening (and
// staking for) a new one.
//
// It also carries the conversation history, because the router keeps no
// conversational state (see "Context handling" in SPEC.md): without this,
// resuming a cached session reconnected to a model that had forgotten
// everything said to it. The file therefore contains conversation text --
// it is written 0600, is gitignored, and --no-history opts out entirely.
type sessionCache struct {
	SessionID string `json:"sessionID"`
	ModelID   string `json:"modelID"`
	Model     string `json:"model"`
	OpenedAt  int64  `json:"openedAt"` // unix seconds
	Duration  int    `json:"duration"` // seconds
	// ExpiresAt is stored directly rather than only derived, so a changed
	// --duration on a later run cannot move an existing session's expiry.
	ExpiresAt int64     `json:"expiresAt,omitempty"` // unix seconds
	History   []message `json:"history,omitempty"`
}

func (s sessionCache) expiresAt() time.Time {
	if s.ExpiresAt > 0 {
		return time.Unix(s.ExpiresAt, 0)
	}
	// Fallback for cache files written before ExpiresAt was stored.
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

func loadSessionCache(path string) (sessionCache, bool) {
	var s sessionCache
	b, err := os.ReadFile(path)
	if err != nil {
		return s, false
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, false
	}
	return s, true
}

// saveSessionCache writes best-effort; a failure to cache shouldn't fail the
// run. 0600 because the file holds conversation text as well as a session id.
func saveSessionCache(path string, s sessionCache) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o600)
}

// devFlow reuses a cached session if one is valid for this model (unless --fresh),
// otherwise opens a new one and caches it. Either way it then runs inference or
// hands off to the interactive loop -- replacing the manual open-session /
// copy-id / run-inference chain (AUDIT F3/F4).
func devFlow(c *client, cfg config) error {
	// Resolve the model before anything else: this is the step that used to
	// fall back to a hardcoded default, quietly staking MOR against a model
	// the user never named.
	model, modelID, why, err := resolveModel(c, cfg)
	if err != nil {
		return err
	}
	cfg.model, cfg.modelID = model, modelID
	fmt.Printf("\n  model: %s\n     id: %s\n    via: %s\n", model, modelID, why)

	var sessionID string
	var exp time.Time
	var history []message

	if !cfg.fresh {
		if cached, ok := loadSessionCache(cfg.cacheFile); ok && cached.valid(cfg.modelID) {
			sessionID = cached.SessionID
			exp = cached.expiresAt()
			fmt.Printf("\n✔ reusing cached session: %s (%s remaining)\n", short(sessionID), remaining(exp))
			// The router holds no conversation state, so resuming the session
			// without its history would reconnect to a model that had
			// forgotten the conversation.
			if !cfg.noHistory && len(cached.History) > 0 {
				history = cached.History
				fmt.Printf("  restored %d earlier message(s) of context\n", len(history))
			}
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
		opened := time.Now()
		exp = opened.Add(time.Duration(cfg.duration) * time.Second)
		fmt.Printf("✔ session opened: %s\n", sessionID)
		// A new session starts with no history: nothing has been said to it.
		history = nil
		saveSessionCache(cfg.cacheFile, sessionCache{
			SessionID: sessionID,
			ModelID:   cfg.modelID,
			Model:     cfg.model,
			OpenedAt:  opened.Unix(),
			Duration:  cfg.duration,
			ExpiresAt: exp.Unix(),
		})
	}

	if cfg.interactive {
		return interactiveLoop(c, cfg, sessionID, exp, history)
	}

	// One-shot --prompt is deliberately stateless: it sends just this prompt
	// so scripted runs are reproducible, and it leaves any stored history
	// untouched. --interactive is the conversational mode.
	fmt.Println("> running inference...")
	reply, err := c.chat(sessionID, cfg.model, cfg.modelID,
		[]message{{Role: "user", Content: cfg.prompt}})
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
func interactiveLoop(c *client, cfg config, sessionID string, exp time.Time, history []message) error {
	fmt.Printf("\n> session live. Type a message and press Enter (type 'exit' to end early).\n\n")
	scanner := bufio.NewScanner(os.Stdin)

	// persist mirrors the in-memory history to the cache after every turn, so
	// an interrupted run loses at most the turn in flight.
	persist := func() {
		if cfg.noHistory {
			return
		}
		cached, ok := loadSessionCache(cfg.cacheFile)
		if !ok || cached.SessionID != sessionID {
			// The cache was replaced or removed underneath us; rebuild the
			// record rather than writing history against the wrong session.
			cached = sessionCache{
				SessionID: sessionID,
				ModelID:   cfg.modelID,
				Model:     cfg.model,
				OpenedAt:  time.Now().Unix(),
				Duration:  cfg.duration,
				ExpiresAt: exp.Unix(),
			}
		}
		cached.History = history
		saveSessionCache(cfg.cacheFile, cached)
	}

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
		history = append(history, message{Role: "user", Content: line})
		reply, err := c.chat(sessionID, cfg.model, cfg.modelID, history)
		if err != nil {
			renderError(os.Stderr, err, cfg)
			history = history[:len(history)-1] // don't poison history with a failed turn
			continue
		}
		history = append(history, message{Role: "assistant", Content: reply})
		persist()
		fmt.Printf("model> %s\n\n", reply)
	}
	fmt.Printf("✅ session %s ended locally (stake refunds automatically at natural expiry; cached for reuse until then)\n", short(sessionID))
	return nil
}

// chat sends the full message history against an already-open session and
// returns the latest reply text.
func (c *client) chat(sessionID, model, modelID string, messages []message) (string, error) {
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
