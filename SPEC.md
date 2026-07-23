# `morpheus-doctor` — Design Notes

Why `morpheus-doctor` exists and the decisions behind how it works. For usage,
see [`README.md`](./README.md) — this file is the design record, not the
manual.

## Origin

Manually testing inference against a Morpheus proxy-router meant three
separate `curl` calls with a session ID hand-copied between them (see
[`AUDIT.md`](../morpheus-internship/contributions/AUDIT.md), findings F3/F4).
`morpheus-doctor` collapses that into one command.

## Why Go

- Compiles to one static binary — no Python/Node runtime required to run it.
- Same language as the Lumerin node itself, keeping the door open to
  upstreaming pieces of it as a `contrib/` tool later.

## What it does (implemented, tested)

**Preflight** (no flags): checks the router is reachable, and — if
credentials are valid — reports the wallet's MOR/ETH balance with a low-MOR
warning under 5 MOR.

**`--dev`**: reuses a cached session for the requested model if one is still
valid (see [Session caching](#session-caching)), otherwise opens a new one,
then runs one inference call and prints the reply.

**`--dev --interactive`**: same session-open behavior, then loops on stdin,
maintaining the full message history client-side and resending it every
turn — see [Context handling](#context-handling).

```
$ morpheus-doctor --dev --model kimi-k2.5 --prompt "Hello!"
✔ router reachable
  wallet: 18.9442 MOR, 0.0021 ETH
> opening session (model kimi-k2.5, 600s)...
✔ session opened: 0x4c8e4022...
> running inference...
✔ inference ok
  Hello! How can I help you today?
✅ session 0x4c8e4022... live (9m57s remaining)
```

## Session caching

`--dev`/`--interactive` write the opened session's ID, model, and expiry to
`.morpheus-doctor-session.json` next to the binary. On the next run against
the same model, if that cached session still has enough time left (30s
safety margin), it's reused instead of opening — and staking for — a new
one. `--fresh` bypasses the cache unconditionally.

This was added after repeated manual testing kept opening redundant sessions
for what was really the same ongoing test — each one staking MOR that then
sat locked until natural expiry.

## Context handling

The router's `StoreChatContext`/`ForwardChatContext` config flags govern
chat *logging*, not conversational memory — confirmed by testing: repeated
requests against the same `session_id` with only the latest message have no
recollection of earlier turns. `--interactive` handles this the standard
OpenAI-compatible way: the client holds the message history and resends the
full transcript every turn. See issue #794 upstream for the documentation
gap this addresses.

## Config & secrets

- Router API auth: `.cookie` file (`admin:<random>`, default `./.cookie`;
  path via `--cookie` or `COOKIE_FILE_PATH`) or `MORPHEUS_ROUTER_AUTH` env.
  Never accepted via a flag — treat as a secret (AUDIT F9). Currently uses
  whatever credentials it's given, typically router admin; a scoped
  `chat`/`open_session` user (`POST /auth/users`) is safer for shared
  setups but not yet wrapped by this tool.
- Router API address: `--host` (default `localhost:8082`).
- Headless wallet: `WALLET_PRIVATE_KEY` on the router side (falls back to
  system keychain when unset) — `morpheus-doctor` doesn't touch the wallet
  directly, it only talks to whatever router process is already running.

## Design decisions

- **Router lifecycle:** `morpheus-doctor` assumes the router is already
  running and health-checks it; it does not start, stop, or manage the
  router process. On Windows, the canonical way to run the router is the
  Desktop App installer (Start-menu/desktop shortcut — the router
  auto-starts with it). There is no `.zip`/`mor-launch.exe` archive build in
  current releases (verified v7.1.12–v7.3.3; see upstream issue #796) —
  earlier drafts of this document assumed one existed and were wrong.
- **Wallet:** assumes a funded wallet already exists on the router side
  (Desktop App or `WALLET_PRIVATE_KEY`); this tool does not wrap wallet
  creation or private-key injection.
- **Balance check:** folded into default (no-flag) preflight rather than a
  separate flag, since it's cheap and directly prevents a real failure mode
  (opening a session that then fails on insufficient MOR — observed
  independently via `mor-cli`, see AUDIT F-series).

## Non-goals

- Not an installer for the Desktop App.
- Not a general-purpose replacement for `mor-cli` or the router's HTTP
  API — a thin, honest wrapper around the one specific painful workflow
  (session → inference), not a full admin CLI.
- Not a health-check for individual models/providers (router-level only).

## Next candidates (not yet built)

- `--local` beginner preflight track covering wallet/network sanity beyond
  router reachability (see original scaffold notes).
- Scoped-user creation helper, so shared/booth setups don't need the admin
  cookie.