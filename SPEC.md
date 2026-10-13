# `morctl` — Design Notes

Why `morctl` exists and the decisions behind how it works. For usage,
see [`README.md`](./README.md) — this file is the design record, not the
manual.

## Origin

Manually testing inference against a Morpheus proxy-router meant three
separate `curl` calls with a session ID hand-copied between them (see
[`AUDIT.md`](../morpheus-internship/contributions/AUDIT.md), findings F3/F4).
`morctl` collapses that into one command.

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
$ morctl --dev --model kimi-k2.5 --prompt "Hello!"
✔ router reachable
  wallet: 18.9442 MOR, 0.0021 ETH

  model: kimi-k2.5
     id: 0xbb9e920d...
    via: --model "kimi-k2.5" resolved to an id
> opening session (model kimi-k2.5, 600s)...
✔ session opened: 0x4c8e4022...
> running inference...
✔ inference ok
  Hello! How can I help you today?
✅ session 0x4c8e4022... live (9m57s remaining)
```

## Session caching

`--dev`/`--interactive` write the opened session's ID, model, expiry and
conversation history to `.morctl-session.json` next to the binary. On the
next run against the same model, if that cached session still has enough
time left (30s safety margin), it's reused instead of opening — and
staking for — a new one. `--fresh` bypasses the cache unconditionally.

`ExpiresAt` is stored directly rather than only derived from
`openedAt + duration`, so passing a different `--duration` on a later run
cannot move an already-open session's expiry. Caches written before that
field existed still load, falling back to the derived value.

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

Because that transcript lives only in the client, it is persisted with the
session: `--interactive` writes history to the session cache after every
completed turn and restores it when a cached session is reused. Without
that, session reuse handed back a live session attached to a model that
had forgotten everything said to it — the cache saved MOR but lost the
conversation. A failed turn is rolled back out of history rather than
stored, so a rate-limited request leaves no dangling question behind.

Consequences worth being explicit about:

- The cache file holds conversation text. It is written `0600`, it is
  gitignored, and `--no-history` opts out of storing or restoring it
  while keeping session reuse working.
- One-shot `--prompt` stays stateless — it sends only that prompt and
  leaves stored history untouched — so scripted runs remain
  reproducible. `--interactive` is the conversational mode.

## Interactive mode

`--interactive` streams replies, shows a status line and takes slash
commands, and it does all of that with the standard library only.

**Why no TUI framework.** Streaming, a one-line status bar and slash
commands need `bufio` and `fmt`, nothing more. Adding bubbletea would
pull roughly twenty modules into a zero-dependency tool for the one
dimension where competing with `mor-cli` has least value — and
`mor-cli`'s chat bugs come precisely from that event loop: an error
assigned to a value-receiver copy and therefore never rendered, followed
by a permanent block on an unbuffered channel. The cost of the framework
there was correctness, not just size.

**Streaming** is a separate client path from the one-shot `chat`, and it
does two things `mor-cli`'s streaming does not:

- It checks the response status *before* reading the body. Without that,
  a 401 or 429 body is simply not SSE, no frames arrive, no error is
  raised, and the UI waits forever on an answer that was refused.
- It falls back to decoding an ordinary completion when the body turns
  out not to be SSE, because some providers ignore `stream:true`.
  Reporting an empty reply there would look like the model said nothing.

It also passes *deltas* to the caller rather than the accumulated text,
and treats a `[DONE]` payload as a terminator rather than content.

**The status line** is redrawn before each prompt and carries the two
facts that cost money: time remaining and escrowed stake (read once from
`GET /blockchain/sessions/{id}`, since it cannot change while the session
is open).

**Slash commands** are handled locally and never sent to the model, so a
mistyped command costs no tokens. `/new` clears the conversation but
keeps the session, and says so — a user must not fear that starting a
new conversation stakes more MOR. `/model` deliberately only *shows* the
model: switching needs a new session, which stakes MOR, so it tells the
user to leave and re-run rather than doing it silently.

**Leaving is explicit.** The farewell states that the session is still
open, how long is left, how much is escrowed, and that nothing was closed
early so there is no 24h on-hold penalty. "Session closed" would be both
wrong and expensive for a user to believe.

**Non-TTY runs** (piped stdin, CI) suppress the prompt, status line and
farewell and print replies plain, so the output is only the answers.

## Config & secrets

- Router API auth: `.cookie` file (`admin:<random>`, default `./.cookie`;
  path via `--cookie` or `COOKIE_FILE_PATH`) or `MORPHEUS_ROUTER_AUTH` env.
  Never accepted via a flag — treat as a secret (AUDIT F9). Currently uses
  whatever credentials it's given, typically router admin; a scoped
  `chat`/`open_session` user (`POST /auth/users`) is safer for shared
  setups but not yet wrapped by this tool.
- Router API address: `--host` (default `localhost:8082`).
- Headless wallet: `WALLET_PRIVATE_KEY` on the router side (falls back to
  system keychain when unset) — `morctl` doesn't touch the wallet
  directly, it only talks to whatever router process is already running.

## No default model

`--dev`/`--interactive` require `--model-id` (exact) or `--model` (a
marketplace name, resolved to an id). There is deliberately no default.

Earlier versions defaulted to `kimi-k2.5` with a matching hardcoded
model id. That was wrong for one blunt reason: **opening a session
stakes MOR.** A user who forgot the flag staked against a model they
never named, and because the name and the id were two separate flags
with two separate defaults, passing only one of them could silently
desync the pair.

Name resolution exists because an on-chain id is not something anyone
remembers, but it is deliberately strict: names are *not* unique (58 of
517 live names were duplicated on mainnet when this was written), so an
ambiguous name is an error listing the candidate ids, never a silent
first match. Deleted models are filtered out. The model list is fetched
only when a name actually needs resolving — it is a ~30s call against
a 517-entry marketplace — and cached for the process.

Whichever path is taken, the run states the name, the id and where the
choice came from before it opens anything.

## Design decisions

- **Router lifecycle:** `morctl` assumes the router is already
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

## Diagnose

`--diagnose` is the doctor-scope core: it answers "why can't I open a
session" in one command, checking router process and components,
credentials, chain id, RPC reachability, balance, allowance, stake on
hold and per-model health.

Three rules shape it:

- **Order by dependency, but never stop at the first failure.** A
  rejected cookie must not hide a wrong chain id, so every independent
  check still runs and the report shows them all. Only an unreachable
  router short-circuits, because nothing else is knowable then.
- **Start with the unauthenticated endpoint.** `/healthcheck` needs no
  credentials, so the router version, uptime, component health and
  per-model status are still reported when the cookie is stale —
  exactly the case a user is most likely to be debugging. It answers
  503 when degraded, so the body is parsed regardless of status: that
  body *is* the diagnosis.
- **One probe, not a sweep.** The RPC check calls `eth_chainId` against
  the first configured endpoint only. Hammering shared public endpoints
  is the very failure being diagnosed, and the router ships with seven
  of them by default.

Three severities, deliberately distinguished: `✖` blocks session work
(and exits 1, so scripts can gate on it), `⚠` is worth knowing (low
balance, claimable stake, a throttled model), `✔` passes. Every non-pass
carries a fix. This is also the one place morctl talks to something
other than the router — a rate-limited or dead RPC is invisible from the
router's own API yet is the most common cause of session failures.

Per-model health comes from the `models[]` block the router has exposed
since v7.5.0, which neither CLI previously surfaced. It is the
provider's self-report, not an independent probe, and it is reported as
such.

## Stake on hold, and claiming it

Closing a session early does not refund the stake immediately: the
contract pushes it into `userStakesOnHold` with
`releaseAt = closedAt + 1 day` (`SessionRouter.sol`). That is the
mechanism behind morctl's standing advice to let sessions expire
naturally rather than closing them — and the reason MOR can appear to
have vanished.

morctl reads `GET /blockchain/stakes/onhold` and can claim via
`POST /blockchain/stakes/withdraw`. Both arrived in node **v7.11.0**
(upstream issue #827); before that, recovery needed `cast` or an
equivalent, which is what the guides had to document.

Design points:

- **No private key, ever.** The claim is an HTTP call; the router's own
  wallet signs the transaction. morctl does not read, store or transmit
  a key, and does not talk to an RPC directly. An earlier sketch of this
  feature assumed a direct `getUserStakesOnHold` contract call, which
  would have required a signer for the withdrawal — the router
  endpoints removed that problem entirely.
- **Two gates on the one command that spends gas.** `--claim-stakes`
  requires `--confirm`, and on a terminal also a typed confirmation.
  Piped stdin is treated as already deliberate, so scripts work.
- **Refuse rather than revert.** With nothing claimable, morctl sends
  nothing and says why, instead of burning gas on a transaction the
  contract would reject. A still-locked balance is reported as such.
- **Claimable stake is surfaced during preflight**, because it is MOR
  the user owns that is invisible everywhere else, and claiming it is
  often what unblocks a wallet that looks short of funds. It is skipped
  silently on routers without the endpoint.

## Session visibility

`--sessions` answers the question the session cache cannot: what else is
open, and what is my MOR tied up in. It reads the wallet address from
`GET /wallet` and lists `GET /blockchain/sessions/user`, showing model,
status, escrowed stake and time remaining, plus a total of the stake held
in live sessions.

Two details that are easy to get wrong:

- `Stake` and `PricePerSecond` arrive as JSON *numbers* in wei, and real
  values exceed float64's exact range (a 5.6888 MOR stake is
  5688807051347138757 wei). They are decoded as `json.Number` and
  formatted through the same `big.Int` path as every other wei value;
  decoding them as numbers would silently corrupt the amount.
- Three states matter, not two: `LIVE` (usable), `expired` (past its
  window, stake on its way back) and `closed` (settled). Collapsing
  expired into closed hides stake that is still in flight.

Model names are opt-in (`--resolve-names`) because there is no
single-model lookup endpoint — a name costs a full ~30s marketplace
fetch, which is too much to impose on a listing command by default.

## Error translation

A non-2xx router response becomes a `routerError` carrying the method,
path, status and body, rather than a flattened string. Display is then
separate from detection: `renderError` looks the failure up in a
table of rules and prints a cause and a fix, while `--verbose`/`--json`
still show the raw response.

Three rules about the table, all learned from how these failures actually
present:

- **Match on status plus a body substring, never an exact body.** The
  interesting failures arrive second-hand — an RPC's `over rate limit`
  or `410 Gone` reaches us wrapped in the router's own 500 — so the
  status alone is often wrong and the exact wording belongs to whatever
  the router was proxying.
- **Never invent a cause.** An unmatched error prints verbatim. A
  confidently wrong diagnosis costs more than no diagnosis.
- **Keep the raw text reachable.** Anyone debugging needs the real
  response, so the translation is additive, never a replacement.

The rules encode the failures this project actually hit: public-RPC
throttling (the router ships with seven shared public Base endpoints by
default), dead RPC endpoints, insufficient stake, models with no bids,
provider disconnects, and cookies rotated by a router restart.

## Non-goals

- Not an installer for the Desktop App.
- Not a general-purpose replacement for `mor-cli` or the router's HTTP
  API — a thin, honest wrapper around the one specific painful workflow
  (session → inference), not a full admin CLI.
- Not an independent prober of model/provider health: `--diagnose` relays
  the provider self-reports the router already publishes, it does not
  test providers itself.

## Next candidates (not yet built)

- `--local` beginner preflight track covering wallet/network sanity beyond
  router reachability (see original scaffold notes).
- Scoped-user creation helper, so shared/booth setups don't need the admin
  cookie.