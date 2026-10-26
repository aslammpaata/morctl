# Running morctl's checks by hand (PowerShell)

## Why the script didn't run

`scripts/live-test.sh` is a **bash** script. PowerShell can't execute a `.sh` file —
`.\scripts\live-test.sh` either opens it in an editor or errors on the shebang. Run it
through bash instead:

```bash
bash scripts/live-test.sh
```

Git Bash is installed at `C:\Program Files\Git\bin\bash.exe`, so that works as-is. I just
ran it that way: 23 checks passed, net change +0.0000 MOR.

Everything below is the same set of checks, done by hand in PowerShell, so you can watch
each one.

## Setup

```powershell
cd C:\Users\ADMIN\Morpheus-Internship\morctl
$env:COOKIE_FILE_PATH = "C:\Users\ADMIN\AppData\Roaming\morpheus-app\services\.cookie"
```

Everything in part 1 is **read-only — nothing opens a session, nothing spends MOR.**

---

# Part 1 — read-only checks

## 1.1 Diagnose

```powershell
.\morctl.exe --diagnose
```

**Expect** ten lines, all `✔`, ending `No blocking problems found.` The lines to confirm:
`router` healthy with a version, `component badgerdb`, `credentials accepted`,
`chain id 8453 (Base)`, `rpc endpoints`, `rpc reachable`, `balance`, `allowance`,
`stake on hold`, `model health`.

Exit code should be 0:

```powershell
$LASTEXITCODE
```

## 1.2 The no-model guard — the important one

This is the safety property from the breaking change: with no model named, nothing may be
opened.

```powershell
.\morctl.exe --dev
```

**Expect** exit 1 and a refusal beginning `no model selected`, naming `--model-id`,
`--model` and `--list-models`. **Critically, no `session opened` line** — it refuses
before any session request reaches the router.

```powershell
$LASTEXITCODE    # must be 1
```

## 1.3 A malformed model id is rejected locally

```powershell
.\morctl.exe --dev --model-id 0xnope
```

**Expect** exit 1, a complaint about `--model-id`, and again **no** `session opened`. The
shape is checked before any request is sent.

## 1.4 A stale cookie is translated, not dumped

```powershell
$env:MORPHEUS_ROUTER_AUTH = "admin:deliberately-wrong"
.\morctl.exe
Remove-Item Env:\MORPHEUS_ROUTER_AUTH
```

**Expect** `the router rejected the credentials` with a `→` line about the `.cookie` being
regenerated on restart — **not** a raw 401 dump. And it must **not** claim `Preflight OK`;
it should say the router is reachable but the wallet could not be read.

Don't skip the `Remove-Item` — otherwise every later command uses the wrong password.

## 1.5 Sessions listing

```powershell
.\morctl.exe --sessions
```

**Expect** `sessions for 0xffd9883a…` (the address is read from the router, you never
supply it), then a row per session with status `LIVE` / `expired` / `closed`, the escrowed
MOR, and timing. Exit 0.

## 1.6 Stake on hold

```powershell
.\morctl.exe --stakes
```

**Expect** `claimable now:` and `still locked:` lines. Right now both read `0.0000 MOR`
with `Nothing on hold.` — correct, because nothing has been closed early.

## 1.7 Claim refuses without confirmation

```powershell
.\morctl.exe --claim-stakes
```

**Expect** exit 1, a refusal, and **no** `withdrawal submitted`. With nothing claimable it
stops on that fact rather than on the missing flag — either way no transaction is sent.

## 1.8 Model listing

```powershell
.\morctl.exe --list-models --model kimi
```

**Expect** a list of matching models with their ids, and a count. Takes ~30s — it fetches
the whole marketplace.

---

# Part 2 — the one that costs MOR (optional)

Only if you want the full `--dev` path on a real session. Read
[`../upstream-patches/VERIFY-LIVE.md`](../upstream-patches/VERIFY-LIVE.md) first for the
cost model; the short version:

- **Your MOR balance is the binding limit, not your allowance.** The router tops
  the allowance up on demand: if it is below `stake × 3`
  (`allowanceReserveMultiplier`), the router calls `increaseAllowance(stake)`
  before opening, and the contract's `safeTransferFrom` then draws it back down.
  Measured on a real open: allowance 10.5190 → 10.5190 (unchanged), balance
  20.0000 → 11.1740. A zero allowance is therefore not a blocker.
- **Never go below 300 seconds.** The contract's `MIN_SESSION_DURATION` is
  5 minutes; below it `openSession` reverts with `SessionTooShort` and the gas is
  spent for nothing. 600s is the shortest length worth using and is also
  `mor-cli`'s default. Every session this wallet has ever opened was ≥ 599s.
- Use a **single-bid** model so the stake is deterministic, and quote the stake
  fresh rather than trusting a number in a doc — prices move:

```bash
AUTH=$(cat "/c/Users/ADMIN/AppData/Roaming/morpheus-app/services/.cookie") && curl -s -u "$AUTH" "http://localhost:8082/blockchain/models/<MODEL_ID>/session/estimate?sessionDuration=600" | python -m json.tool
```

That endpoint is read-only — it opens nothing. Stake scales linearly with
duration, so a 600s session costs 5× a (hypothetical) 120s one.

```powershell
.\morctl.exe --dev --fresh --duration 600 `
  --model-id 0x93c1c300f2eca67c61d80f28e5ae44cb9d6b64c5cfa70db8ec92ed180d86414a `
  --prompt "Reply with exactly: live-ok"
```

**Expect** the model and id echoed with `via:`, then `session opened`, `inference ok`, the
reply, and the time remaining.

Then, to confirm reuse and history (free — reuses that session):

```powershell
.\morctl.exe --dev --duration 600 `
  --model-id 0x93c1c300f2eca67c61d80f28e5ae44cb9d6b64c5cfa70db8ec92ed180d86414a `
  --prompt "second turn"
```

**Expect** `reusing cached session` and **no** second `session opened` — no new stake.

And interactive, with history persisting across runs:

```powershell
.\morctl.exe --dev --interactive --duration 600 `
  --model-id 0x93c1c300f2eca67c61d80f28e5ae44cb9d6b64c5cfa70db8ec92ed180d86414a
```

Tell it your name, ask for it back, `/exit`. Re-run the same command and ask again — it
should remember, and say `restored N earlier message(s) of context` on the way in.

Afterwards:

```powershell
.\morctl.exe --sessions
.\morctl.exe --stakes
```

**Expect** the session `LIVE` then `expired`, and stake on hold staying `0.0000` — nothing
was closed early. Wait ~3 minutes and the stake returns to your balance.

**Never close a session early** — that is what triggers the 24-hour on-hold lock.

---

# If a check disagrees

Send me the command and its output. Checks 1.2, 1.3 and 1.7 are the ones that matter most:
they are the guards that stop `morctl` from staking MOR you did not ask it to.
