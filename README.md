# morctl

*(Renamed from `morpheus-doctor` — see [REBRAND-AND-ROADMAP.md](./Rebrand%20and%20roadmap.md).
Old repo URL redirects here automatically.)*

A single-binary CLI for the Morpheus proxy-router. Turns the manual "open session → copy
session ID → run inference" chain into one command, with a preflight check (router
reachable, wallet balance) and a multi-turn interactive chat mode.

## Why

Manually testing inference against the Morpheus network normally means:

```bash
curl -X POST http://localhost:8082/blockchain/models/<id>/session \
  -u admin:<password> -d '{"sessionDuration": 600}'
# copy the sessionID from the response...

curl -X POST http://localhost:8082/v1/chat/completions \
  -u admin:<password> \
  -H "session_id: <paste it here>" -H "model_id: <id>" \
  -d '{"model":"...", "messages":[...]}'
```

`morctl --dev` does both steps in one command, with the session ID captured in memory —
no copy-pasting.

## Build

Requires Go 1.21+.

**Linux/macOS (native):**
```bash
go build -o morctl .
```

**Windows (native):**
```powershell
go build -o morctl.exe .
```

**Cross-compile for Windows from Linux/WSL/macOS:**
```bash
GOOS=windows GOARCH=amd64 go build -o morctl.exe .
```

**Cross-compile for macOS from Linux/Windows/WSL:**
```bash
# Intel (x86_64)
GOOS=darwin GOARCH=amd64 go build -o morctl .

# Apple Silicon (M1/M2/M3/M4)
GOOS=darwin GOARCH=arm64 go build -o morctl .
```

> macOS users: the resulting binary is unsigned. On first run, Gatekeeper may block it —
> right-click the binary → **Open** → **Open anyway**, or run
> `xattr -d com.apple.quarantine ./morctl` to clear the quarantine flag from the terminal.

## Prerequisites

- A running Morpheus proxy-router (Desktop App, or headless — see
  [Router setup](#router-setup) below) reachable at `localhost:8082`.
- A funded wallet on the router (MOR for sessions, ETH on Base for gas).
- The router's `.cookie` file, or a `MORPHEUS_ROUTER_AUTH=user:pass` value.

## Usage

**Preflight** — checks the router is up and shows your wallet balance:
```bash
./morctl
```

**List models** — see what's on the marketplace (narrow with `--model`):
```bash
./morctl --list-models
./morctl --list-models --model kimi
```

**Developer flow** — opens a session and runs one inference call. A model is
required; `morctl` will not pick one for you, because opening a session stakes MOR:
```bash
./morctl --dev --model-id 0xbb9e920d... --prompt "Hello!"

# or by name, which is resolved to an id (names are not unique, so an
# ambiguous name is refused with the candidate ids listed):
./morctl --dev --model kimi-k2.5 --prompt "Hello!"
```

**Interactive** — keeps a session open across multiple prompts, with full conversation
history resent each turn (the router does not retain context on its own — see
[Notes](#notes)):
```bash
./morctl --dev --interactive --model-id 0xbb9e920d...
```
Type `exit` or `quit` to end. The session's stake refunds automatically at its natural
expiry — exiting the loop does not close it early.

**Sessions** — list this wallet's recent sessions so overlapping ones are legible:
```bash
./morctl --sessions
./morctl --sessions --resolve-names   # adds a ~30s marketplace fetch for model names
```
```
> sessions for 0xffd9883a2245027b95a4f1801ac6b4e160b373e6

  0x4c8e4022... LIVE    kimi-k2.5                        5.0000 MOR  7m12s left
  0x2b5b7c29... closed  0xc8530921...                    5.6888 MOR  ended 2026-09-09 15:49

  2 session(s) listed, 1 live, 5.0000 MOR escrowed in them.
```
Status is `LIVE` (usable now), `expired` (past its window, stake still returning) or
`closed` (settled). The wallet address is read from the router, so you never have to
supply it — unlike `mor-cli listBlockchainSession`, which requires `--user`.

**Stake on hold** — MOR locked by sessions that closed early, and claiming it back:
```bash
./morctl --stakes                      # read-only
./morctl --claim-stakes --confirm      # sends a transaction
```
Closing a session early doesn't refund immediately: the contract parks the refund in
`userStakesOnHold` with a 24-hour lock (`releaseAt = closedAt + 1 day`). `--stakes` shows
what is claimable now versus still locked; claimable stake is also flagged during
preflight, since it's MOR you own that shows up nowhere else.

`--claim-stakes` **requires `--confirm`** and, on a terminal, a typed confirmation. It
refuses outright when there is nothing claimable rather than spending gas on a transaction
the contract would revert. **The router's own wallet signs it** — `morctl` never reads,
stores or transmits a private key. Needs node **v7.11.0+** (upstream issue #827); older
routers get a clear message saying so.

**Force a new session**, ignoring any cached one:
```bash
./morctl --dev --fresh --model-id 0xbb9e920d...
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `--host` | `localhost:8082` | Router API host:port |
| `--model-id` | *(none — required)* | On-chain model ID (`0x` + 64 hex). Exact; preferred |
| `--model` | *(none)* | Marketplace model name, resolved to an ID. Also narrows `--list-models` |
| `--list-models` | off | List live marketplace models and exit |
| `--prompt` | `"Hello!"` | Prompt to send in `--dev` mode |
| `--duration` | `600` | Session duration in seconds |
| `--dev` | off | Open a session and run inference |
| `--interactive` | off | Multi-turn chat mode (implies `--dev`) |
| `--fresh` | off | Ignore any cached session, open a new one |
| `--no-history` | off | Don't store or restore conversation history in the cache |
| `--json` | off | Also print raw request/response JSON |
| `--verbose` | off | Show the raw router response alongside translated errors |
| `--sessions` | off | List this wallet's recent sessions and exit |
| `--resolve-names` | off | Show model names in `--sessions` (adds a ~30s fetch) |
| `--stakes` | off | Show MOR stake on hold for this wallet and exit |
| `--claim-stakes` | off | Withdraw releasable stake (needs `--confirm`; sends a transaction) |
| `--confirm` | off | Confirm an action that sends a transaction |
| `--cookie` | `./.cookie` | Path to the router `.cookie` file |

### No default model

`--dev`, `--interactive` and `--prompt` require a model: pass `--model-id` (exact) or
`--model` (a name, resolved against the marketplace). There is deliberately **no**
default, because opening a session stakes MOR — a silent default meant staking against
a model the user never named.

Earlier versions defaulted to `kimi-k2.5`. **If you have scripts, aliases or guide steps
that relied on that, add `--model-id 0xbb9e920d94ad3fa2861e1e209d0a969dbe9e1af1cf1ad95c49f76d7b63d32d93`
explicitly.** Model names are not unique on the marketplace (58 of 517 live names were
duplicated when this was written), so `--model <name>` refuses an ambiguous name and
prints the candidate IDs instead of guessing. Whichever way the model is chosen, the run
prints the name, the ID and where the choice came from before opening anything.

## Session caching and conversation history

`morctl --dev` writes the opened session's ID, model and expiry to `.morctl-session.json`
next to the binary. Subsequent runs against the same model reuse that session (if it still
has enough time left) instead of opening — and staking for — a new one. Use `--fresh` to
bypass this.

`--interactive` also stores the **conversation history** in that file, saved after every
completed turn. Resuming a cached session restores the transcript, so the model still has
the context of what was said in an earlier run — without this, reusing a session
reconnected you to a model that had forgotten the conversation, since the router keeps no
conversational state of its own (see [Notes](#notes)).

- A new session always starts with empty history. `--fresh` opens a new session and starts
  a new conversation.
- A failed turn is not stored, so a rate-limit error doesn't leave a dangling question in
  the transcript.
- One-shot `--prompt` is deliberately stateless: it sends only that prompt, so scripted
  runs stay reproducible. It leaves any stored history untouched.

> **The cache file contains your conversation text.** It is written `0600` and is
> gitignored. Pass `--no-history` to keep conversations out of it entirely (the session ID
> is still cached, so session reuse keeps working).

## Error messages

Router failures are translated into a cause and a fix instead of a bare status line:

```
$ morctl --dev --model-id 0xbb9e920d...
> opening session (model kimi-k2.5, 600s)...
✖ the blockchain RPC endpoint is rate-limiting the router. The public Base RPCs are
  shared and throttle quickly.
  → Point the router at a dedicated RPC (ETH_NODE_ADDRESS, or POST /config/ethNode),
    then retry.
  (POST /blockchain/models/0xbb9e920d.../session -> 429 Too Many Requests; --verbose for
   the full response)
```

Covered: RPC rate-limiting (429, or an `over rate limit` body behind a 500), a dead RPC
endpoint (410 Gone), insufficient MOR to stake, no provider bids for the model, a provider
disconnecting mid-session, rejected credentials (401, usually a `.cookie` regenerated by a
router restart), missing permissions on a scoped user (403), a router too old for the stake
endpoints (404), and a withdrawal already in flight (409).

The **original error is never hidden**: `--verbose` or `--json` prints the raw response
alongside the translation, and an error with no matching rule is printed verbatim rather
than given an invented cause.

## Credentials

The router's API password lives in its `.cookie` file. `morctl` reads it from, in order:
1. `MORPHEUS_ROUTER_AUTH` environment variable (`user:pass`)
2. The path given by `--cookie`, or `COOKIE_FILE_PATH` if set, or `./.cookie`

Credentials are **never** accepted via a command-line flag (they'd leak into shell
history / process lists). Treat the `.cookie` file as a secret — don't commit it, don't
paste it into chat tools or logs.

## Router setup

`morctl` doesn't manage the router's lifecycle — it just talks to whichever router is
already running.

- **Desktop App**: launch it normally; the router auto-starts. The `.cookie` lives at
  `%APPDATA%\morpheus-app\services\.cookie` (Windows) or the equivalent app-data path on
  macOS/Linux.
- **Headless**: run `proxy-router` directly with a `.env` configuring
  `ETH_NODE_ADDRESS`, chain ID, contract addresses, etc. Provide a wallet via
  `WALLET_PRIVATE_KEY` or the `POST /wallet/privateKey` endpoint at startup.

## Notes

- **Conversation context is not automatic.** The router does not retain chat history
  between requests, even within one session — `--interactive` handles this client-side
  by resending the full message history every turn. A raw `/v1/chat/completions` call
  with only the latest message will not have any memory of earlier turns. (This is
  confirmed intended behavior, not a bug — the client is responsible for turn/history
  management.)
- **Session close vs. session end.** Typing `exit` in `--interactive` stops the local
  loop only; it does not close the session on-chain. Your stake returns automatically
  ~1 minute after the session's natural expiry, same as any Morpheus session. This
  avoids the early-close on-hold penalty.

## Known limitations / status

- Tested against router v7.3.0 on Windows, against multiple models, with session reuse,
  balance reporting, and graceful handling of router-down / invalid-model-ID /
  mid-session provider-disconnect failure cases.
- macOS/Linux native builds compile cleanly but have not yet had the same depth of
  live testing as the Windows build — treat as expected-to-work, not yet as
  thoroughly verified.
- No scoped-user support yet (currently uses whatever credentials you give it,
  typically the router's admin cookie). For shared/multi-user setups, create a scoped
  `open_session`+`chat` user via the router's `/auth/users` endpoint and pass those
  credentials instead.
- Health-check is router-level only (`/healthcheck`); it does not check individual
  model/provider health.

## Roadmap

A separate, future tool (`morpheus-doctor` v2) is planned to actually earn that name —
install, diagnose, and repair Morpheus installations. See
[REBRAND-AND-ROADMAP.md](./REBRAND-AND-ROADMAP.md) for the full scope. Not a current
priority; `morctl` remains focused on session/inference operations.