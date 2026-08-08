# morctl

A single-binary CLI for the Morpheus proxy-router. Turns the manual
"open session → copy session ID → run inference" chain into one command,
with a preflight check (router reachable, wallet balance) and a multi-turn
interactive chat mode.

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

`morctl --dev` does both steps in one command, with the session ID
captured in memory — no copy-pasting.

## Build

Requires Go 1.21+.

```bash
go build -o morctl .
```

Cross-compile for Windows from Linux/WSL/macOS:

```bash
GOOS=windows GOARCH=amd64 go build -o morctl.exe .
```

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

**Developer flow** — opens a session and runs one inference call:
```bash
./morctl --dev --prompt "Hello!"
```

**Interactive** — keeps a session open across multiple prompts, with full
conversation history resent each turn (the router does not retain context
on its own — see [Notes](#notes)):
```bash
./morctl --dev --interactive
```
Type `exit` or `quit` to end. The session's stake refunds automatically at
its natural expiry — exiting the loop does not close it early.

**Force a new session**, ignoring any cached one:
```bash
./morctl --dev --fresh
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `--host` | `localhost:8082` | Router API host:port |
| `--model` | `kimi-k2.5` | Model name for the completion request |
| `--model-id` | *(kimi-k2.5's ID)* | On-chain model ID (`0x...`) |
| `--prompt` | `"Hello!"` | Prompt to send in `--dev` mode |
| `--duration` | `600` | Session duration in seconds |
| `--dev` | off | Open a session and run inference |
| `--interactive` | off | Multi-turn chat mode (implies `--dev`) |
| `--fresh` | off | Ignore any cached session, open a new one |
| `--json` | off | Also print raw request/response JSON |
| `--cookie` | `./.cookie` | Path to the router `.cookie` file |

## Session caching

`morctl --dev` writes the opened session's ID and expiry to
`.morctl-session.json` next to the binary. Subsequent runs against
the same model reuse that session (if it still has enough time left) instead
of opening — and staking for — a new one. Use `--fresh` to bypass this.

## Credentials

The router's API password lives in its `.cookie` file. `morctl`
reads it from, in order:
1. `MORPHEUS_ROUTER_AUTH` environment variable (`user:pass`)
2. The path given by `--cookie`, or `COOKIE_FILE_PATH` if set, or `./.cookie`

Credentials are **never** accepted via a command-line flag (they'd leak into
shell history / process lists). Treat the `.cookie` file as a secret — don't
commit it, don't paste it into chat tools or logs.

## Router setup

`morctl` doesn't manage the router's lifecycle — it just talks to
whichever router is already running.

- **Desktop App**: launch it normally; the router auto-starts and its
  `.cookie` lives at `%APPDATA%\morpheus-app\services\.cookie` (Windows) or
  the equivalent app-data path on macOS/Linux.
- **Headless**: run `proxy-router` directly with a `.env` configuring
  `ETH_NODE_ADDRESS`, chain ID, contract addresses, etc. Provide a wallet via
  `WALLET_PRIVATE_KEY` or the `POST /wallet/privateKey` endpoint at startup.

## Notes

- **Conversation context is not automatic.** The router does not retain
  chat history between requests, even within one session — `--interactive`
  handles this client-side by resending the full message history every
  turn. A raw `/v1/chat/completions` call with only the latest message will
  not have any memory of earlier turns.
- **Session close vs. session end.** Typing `exit` in `--interactive` stops
  the local loop only; it does not close the session on-chain. Your stake
  returns automatically ~1 minute after the session's natural expiry, same
  as any Morpheus session. This avoids the early-close on-hold penalty.

## Known limitations / status

- Tested against router v7.3.0 on Windows, against multiple models, with
  session reuse, balance reporting, and graceful handling of router-down /
  invalid-model-ID / mid-session provider-disconnect failure cases.
- No scoped-user support yet (currently uses whatever credentials you give
  it, typically the router's admin cookie). For shared/multi-user setups,
  create a scoped `open_session`+`chat` user via the router's `/auth/users`
  endpoint and pass those credentials instead.
- Health-check is router-level only (`/healthcheck`); it does not check
  individual model/provider health.