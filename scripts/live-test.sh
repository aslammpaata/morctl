#!/usr/bin/env bash
#
# live-test.sh -- exercise morctl against a real proxy-router and a real wallet.
#
# These are the paths that cannot be covered by the mock-router test suite,
# because they need an actual on-chain session: opening one, reusing it,
# restoring its conversation, and claiming stake back.
#
# SAFETY MODEL
#   Phase 1 (read-only)  runs by default and spends nothing.
#   Phase 2 (--spend)    opens ONE session. This stakes MOR.
#   Phase 3 (--claim)    withdraws claimable stake. This sends a transaction.
#   Phase 4 (--make-onhold) closes a session EARLY. NO LONGER NEEDED: stake is
#                        day-locked on ANY close, so a normal session already
#                        produces claimable stake at 00:00 UTC the next day.
#                        Closing early only forfeits time you paid for.
#
# Nothing in phase 2-4 runs without the matching flag, and each prints what it
# is about to do and waits for you to type 'yes'.
#
# USAGE
#   ./scripts/live-test.sh                      # read-only checks
#   ./scripts/live-test.sh --spend              # + open a session, run inference
#   ./scripts/live-test.sh --spend --claim      # + claim stake if any is released
#   ./scripts/live-test.sh --make-onhold        # deprecated, see above
#
#   --model-id 0x...   model to use (default: kimi-k2.5)
#   --host host:port   router API (default: localhost:8082)
#   --yes              skip the interactive confirmations (for an unattended run)
#
# Run it from the repo root. Output is also written to ./live-test-<stamp>.log

set -uo pipefail

# ---------------------------------------------------------------- configuration

MODEL_ID="0xbb9e920d94ad3fa2861e1e209d0a969dbe9e1af1cf1ad95c49f76d7b63d32d93"
HOST="localhost:8082"
DO_SPEND=0
DO_CLAIM=0
DO_ONHOLD=0
ASSUME_YES=0

while [ $# -gt 0 ]; do
  case "$1" in
    --spend)       DO_SPEND=1 ;;
    --claim)       DO_CLAIM=1 ;;
    --make-onhold) DO_ONHOLD=1 ;;
    --yes|-y)      ASSUME_YES=1 ;;
    --model-id)    MODEL_ID="${2:-}"; shift ;;
    --host)        HOST="${2:-}"; shift ;;
    -h|--help)     sed -n '2,33p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1 (try --help)" >&2; exit 2 ;;
  esac
  shift
done

STAMP="$(date +%Y%m%d-%H%M%S)"
LOG="./live-test-$STAMP.log"

# Find the binary without assuming a platform.
MORCTL=""
for candidate in ./morctl.exe ./morctl ./dist/morctl-windows-amd64.exe; do
  [ -x "$candidate" ] && MORCTL="$candidate" && break
done
if [ -z "$MORCTL" ]; then
  echo "No morctl binary found. Build one first:  make build" >&2
  exit 1
fi

PASS=0
FAIL=0
SKIP=0
FAILED_STEPS=""

# ------------------------------------------------------------------- utilities

log()  { printf '%s\n' "$*" | tee -a "$LOG"; }
head1() { log ""; log "=============================================================="; log "$*"; log "=============================================================="; }
head2() { log ""; log "--- $* ---"; }

pass() { PASS=$((PASS+1)); log "  PASS  $*"; }
fail() { FAIL=$((FAIL+1)); FAILED_STEPS="$FAILED_STEPS
  - $*"; log "  FAIL  $*"; }
skip() { SKIP=$((SKIP+1)); log "  SKIP  $*"; }

# confirm prompts unless --yes. Returns non-zero if declined.
confirm() {
  [ "$ASSUME_YES" = "1" ] && return 0
  printf '\n  >>> %s\n  >>> Type "yes" to continue: ' "$1"
  read -r answer
  [ "$answer" = "yes" ]
}

# run <description> -- <command...>
# Captures output to the log and leaves it in $RUN_OUT / $RUN_RC.
RUN_OUT=""
RUN_RC=0
run() {
  local desc="$1"; shift
  [ "$1" = "--" ] && shift
  log ""
  log "  \$ $*"
  # stdin from /dev/null: this helper captures stdout, so a command that
  # stops to prompt would wait on input the user cannot see it asking for.
  # That is exactly how two real --claim-stakes attempts were lost.
  RUN_OUT="$("$@" 2>&1 </dev/null)"
  RUN_RC=$?
  printf '%s\n' "$RUN_OUT" | sed 's/^/      /' | tee -a "$LOG" >/dev/null
  printf '%s\n' "$RUN_OUT" | sed 's/^/      /'
  log "      [exit $RUN_RC]"
}

# expect_out <description> <pattern>
expect_out() {
  if printf '%s' "$RUN_OUT" | grep -qi -- "$2"; then pass "$1"; else fail "$1 (expected to see: $2)"; fi
}
expect_not_out() {
  if printf '%s' "$RUN_OUT" | grep -qi -- "$2"; then fail "$1 (did NOT expect: $2)"; else pass "$1"; fi
}
expect_rc() {
  if [ "$RUN_RC" = "$2" ]; then pass "$1"; else fail "$1 (exit was $RUN_RC, wanted $2)"; fi
}

# ------------------------------------------------- direct router reads (wei)
# The cookie is read into a variable and never printed.

ROUTER="http://$HOST"
COOKIE_PATH="${COOKIE_FILE_PATH:-}"
if [ -z "$COOKIE_PATH" ]; then
  COOKIE_PATH="$(curl -s --max-time 5 "$ROUTER/auth/cookie/path" 2>/dev/null \
    | python -c 'import sys,json;print(json.load(sys.stdin)["path"])' 2>/dev/null || true)"
fi
[ -z "$COOKIE_PATH" ] && COOKIE_PATH="./.cookie"
AUTH=""
[ -r "$COOKIE_PATH" ] && AUTH="$(cat "$COOKIE_PATH")"
if [ -n "${MORPHEUS_ROUTER_AUTH:-}" ]; then AUTH="$MORPHEUS_ROUTER_AUTH"; fi

# wei_field <url> <json-path> -- prints the raw wei integer, or "" on failure.
#
# Retries once with a generous timeout: these reads go through the router to a
# shared public Base RPC, which intermittently takes 20s+ to answer. A missed
# read here would show up as a "?" balance delta, which is the one number worth
# trusting during a spending run.
wei_field() {
  local attempt out
  for attempt in 1 2; do
    out="$(curl -s -u "$AUTH" --max-time 45 "$1" 2>/dev/null \
      | python -c "
import sys,json
try:
    d=json.load(sys.stdin)
    for k in '$2'.split('.'):
        d=d[k]
    print(d)
except Exception:
    print('')
" 2>/dev/null)"
    if [ -n "$out" ]; then printf '%s' "$out"; return 0; fi
    sleep 2
  done
  printf ''
}

mor_balance()    { wei_field "$ROUTER/blockchain/balance" "mor"; }
stake_available() { wei_field "$ROUTER/blockchain/stakes/onhold?iterations=255" "available"; }
stake_hold()      { wei_field "$ROUTER/blockchain/stakes/onhold?iterations=255" "hold"; }

# fmt_wei <wei> -- 4dp token amount, via integers so nothing is lost
fmt_wei() {
  python -c "
import sys
v='''$1'''.strip()
if not v.isdigit():
    print('?'); sys.exit()
q,r=divmod(int(v),10**18)
print('%d.%04d' % (q, r//10**14))
"
}

delta_wei() {
  python -c "
a='''$1'''.strip(); b='''$2'''.strip()
if not (a.isdigit() and b.isdigit()):
    print('?'); raise SystemExit
d=int(b)-int(a)
sign='+' if d>=0 else '-'
d=abs(d); q,r=divmod(d,10**18)
print('%s%d.%04d' % (sign,q,r//10**14))
"
}

# ======================================================================= start

log "morctl live test -- $STAMP"
log "binary:   $MORCTL"
log "router:   $ROUTER"
log "model-id: $MODEL_ID"
log "log file: $LOG"
log ""
log "phases: read-only=yes spend=$DO_SPEND claim=$DO_CLAIM make-onhold=$DO_ONHOLD"

head1 "PHASE 0 -- preconditions"

run "router healthcheck" -- curl -s --max-time 10 "$ROUTER/healthcheck"
expect_out "router answers /healthcheck" "status"
ROUTER_VERSION="$(printf '%s' "$RUN_OUT" | python -c 'import sys,json;print(json.load(sys.stdin).get("version",""))' 2>/dev/null || true)"
log "      router version: ${ROUTER_VERSION:-unknown}"

if [ -z "$AUTH" ]; then
  log ""
  log "  No credentials found. Set MORPHEUS_ROUTER_AUTH=user:pass or COOKIE_FILE_PATH,"
  log "  or make sure the router's .cookie is readable. Balance deltas will be skipped."
fi

MOR_START="$(mor_balance)"
log "      wallet MOR at start: $(fmt_wei "${MOR_START:-}") ($([ -n "${MOR_START:-}" ] && echo "$MOR_START wei" || echo "unavailable"))"

# ============================================================== PHASE 1 (free)

head1 "PHASE 1 -- read-only (spends nothing)"

head2 "1.1 diagnose"
run "diagnose" -- "$MORCTL" --host "$HOST" --diagnose
expect_out "router reported healthy" "router "
expect_out "chain id checked" "chain id"
expect_out "rpc endpoint probed" "rpc reachable"
expect_out "balance reported" "balance"
expect_out "stake on hold reported" "stake on hold"

head2 "1.2 the no-model guard must refuse BEFORE opening a session"
run "dev with no model" -- "$MORCTL" --host "$HOST" --dev
expect_rc "exits non-zero" 1
expect_out "explains that no model was selected" "no model selected"
expect_not_out "no session was opened" "session opened"

head2 "1.3 a malformed model id is rejected without a request"
run "dev with a bad model id" -- "$MORCTL" --host "$HOST" --dev --model-id 0xnope
expect_rc "exits non-zero" 1
expect_not_out "no session was opened" "session opened"

head2 "1.4 stale credentials are translated, not dumped"
run "preflight with a wrong password" -- env MORPHEUS_ROUTER_AUTH="admin:deliberately-wrong" \
  "$MORCTL" --host "$HOST"
expect_out "explains the credential rejection" "rejected the credentials"
expect_not_out "does not claim preflight passed" "Preflight OK"
COOKIE_SECRET="$(printf '%s' "${AUTH:-}" | cut -d: -f2-)"
if [ -n "$COOKIE_SECRET" ]; then
  expect_not_out "never prints the real cookie value" "$COOKIE_SECRET"
else
  skip "cookie-leak check (no credentials available to compare against)"
fi

head2 "1.5 sessions listing"
run "sessions" -- "$MORCTL" --host "$HOST" --sessions
expect_rc "succeeds" 0
expect_out "reports the wallet it queried" "sessions for 0x"

head2 "1.6 stakes read"
run "stakes" -- "$MORCTL" --host "$HOST" --stakes
expect_rc "succeeds" 0
expect_out "reports claimable" "claimable now"
expect_out "reports locked" "still locked"

head2 "1.7 claim refuses without --confirm"
run "claim-stakes without --confirm" -- "$MORCTL" --host "$HOST" --claim-stakes
expect_rc "exits non-zero" 1
expect_not_out "no transaction was submitted" "withdrawal submitted"

head2 "1.8 model listing and resolution"
run "list-models (filtered)" -- "$MORCTL" --host "$HOST" --list-models --model kimi
expect_rc "succeeds" 0
expect_out "lists at least one model" "0x"

log ""
log "  Phase 1 complete. Nothing was spent."

# ============================================================= PHASE 2 (spends)

if [ "$DO_SPEND" = "1" ]; then
  head1 "PHASE 2 -- open a session and run inference (STAKES MOR)"
  log ""
  log "  This opens ONE session against model:"
  log "    $MODEL_ID"
  log "  for the default duration (600s), which escrows MOR until the session ends."
  log "  When it ends, the stake drawn against that UTC day's stipend is held until"
  log "  00:00 UTC the next day, then claimable with --claim-stakes --confirm."
  log "  That day lock applies to any close, early or at natural expiry."

  if confirm "Open a real session and stake MOR?"; then

    head2 "2.1 fresh session + one inference"
    run "dev --fresh" -- "$MORCTL" --host "$HOST" --dev --fresh --model-id "$MODEL_ID" \
      --prompt "Reply with exactly: live-test-ok"
    expect_rc "succeeds" 0
    expect_out "states which model was chosen" "model:"
    expect_out "opened a session" "session opened"
    expect_out "inference returned" "inference ok"
    expect_out "session reported live afterwards" "live ("

    SESSION_ID="$(printf '%s' "$RUN_OUT" | grep -oE 'session opened: 0x[0-9a-f]+' | head -1 | awk '{print $3}')"
    log "      session: ${SESSION_ID:-not captured}"

    head2 "2.2 the next run must REUSE that session (no new stake)"
    MOR_BEFORE_REUSE="$(mor_balance)"
    run "dev again, same model" -- "$MORCTL" --host "$HOST" --dev --model-id "$MODEL_ID" \
      --prompt "Reply with exactly: reuse-ok"
    expect_rc "succeeds" 0
    expect_out "reused the cached session" "reusing cached session"
    expect_not_out "did NOT open a second session" "session opened"
    MOR_AFTER_REUSE="$(mor_balance)"
    log "      MOR change across the reuse: $(delta_wei "${MOR_BEFORE_REUSE:-}" "${MOR_AFTER_REUSE:-}")"

    head2 "2.3 interactive, piped: two turns, history saved"
    INFILE="$(mktemp)"
    printf 'My name is Ada. Reply with exactly: noted\nWhat name did I give you?\n/exit\n' > "$INFILE"
    log ""
    log "  \$ $MORCTL --dev --interactive --model-id ... < (2 turns)"
    RUN_OUT="$("$MORCTL" --host "$HOST" --dev --interactive --model-id "$MODEL_ID" < "$INFILE" 2>&1)"
    RUN_RC=$?
    printf '%s\n' "$RUN_OUT" | sed 's/^/      /' | tee -a "$LOG"
    log "      [exit $RUN_RC]"
    rm -f "$INFILE"
    expect_rc "succeeds" 0
    expect_out "reused the session rather than opening another" "reusing cached session"
    # The second turn can only answer if the first turn's history was resent.
    expect_out "the model recalled the name from turn 1" "ada"

    head2 "2.4 history survives into a NEW invocation"
    INFILE2="$(mktemp)"
    printf 'What name did I give you earlier?\n/exit\n' > "$INFILE2"
    log ""
    log "  \$ $MORCTL --dev --interactive --model-id ... < (1 turn, new process)"
    RUN_OUT="$("$MORCTL" --host "$HOST" --dev --interactive --model-id "$MODEL_ID" < "$INFILE2" 2>&1)"
    RUN_RC=$?
    printf '%s\n' "$RUN_OUT" | sed 's/^/      /' | tee -a "$LOG"
    log "      [exit $RUN_RC]"
    rm -f "$INFILE2"
    expect_out "restored earlier context" "restored"
    expect_out "the model still recalled the name across processes" "ada"

    head2 "2.5 the live session is now visible with its escrow"
    run "sessions" -- "$MORCTL" --host "$HOST" --sessions
    expect_out "shows a LIVE session" "LIVE"
    expect_out "totals the escrowed stake" "escrowed in them"

    head2 "2.6 diagnose still clean with a session open"
    run "diagnose" -- "$MORCTL" --host "$HOST" --diagnose
    expect_rc "no blocking problems" 0

  else
    skip "phase 2 declined"
  fi
else
  log ""
  log "PHASE 2 skipped (pass --spend to open a real session)."
fi

# ======================================================== PHASE 4 (make on-hold)
# Deliberately numbered after the claim in the docs but run before it, because
# it is what makes a claim testable at all.

if [ "$DO_ONHOLD" = "1" ]; then
  head1 "PHASE 4 -- create claimable stake by closing a session EARLY"
  log ""
  log "  NOTE: this phase is no longer necessary. Stake is day-locked on ANY"
  log "  close -- releaseAt = startOfTheDay(sessionEnd) + 1 day -- so just letting"
  log "  a session end gives you claimable stake at 00:00 UTC the next day."
  log "  Closing early forfeits the time you paid for and gains nothing."
  log ""
  log "  It is kept only so an old run can be reproduced; prefer letting a session end."
  log "  This step uses mor-cli (the official CLI) to close the session, because"
  log "  morctl has no close command and should not grow one."
  log ""
  log "  Consequences: the remaining stake is locked for 24 HOURS, and closing"
  log "  costs gas. Only do this if you want to exercise --claim-stakes tomorrow."

  MORCLI="${MORCLI:-mor-cli}"
  if ! command -v "$MORCLI" >/dev/null 2>&1 && [ ! -x "$MORCLI" ]; then
    skip "mor-cli not found (set MORCLI=/path/to/mor-cli) -- cannot close a session"
  elif [ -z "${SESSION_ID:-}" ]; then
    skip "no session id captured in this run -- run with --spend in the same invocation"
  elif confirm "Close session ${SESSION_ID} early, forfeiting the time already paid for?"; then
    run "close the session" -- "$MORCLI" closeBlockchainSession --session "$SESSION_ID"
    log ""
    log "  Now wait ~24h, then run:  ./scripts/live-test.sh --claim"
    run "stakes immediately after closing" -- "$MORCTL" --host "$HOST" --stakes
    expect_out "something is now on hold" "still locked"
  else
    skip "phase 4 declined"
  fi
fi

# ============================================================== PHASE 3 (claim)

if [ "$DO_CLAIM" = "1" ]; then
  head1 "PHASE 3 -- claim released stake (SENDS A TRANSACTION)"

  AVAIL="$(stake_available)"
  HOLD="$(stake_hold)"
  log ""
  log "  claimable now: $(fmt_wei "${AVAIL:-}") MOR"
  log "  still locked:  $(fmt_wei "${HOLD:-}") MOR"

  if [ -z "${AVAIL:-}" ]; then
    skip "could not read the stake endpoints (router older than v7.11.0, or no credentials)"
  elif [ "$AVAIL" = "0" ]; then
    log ""
    log "  Nothing is claimable, which is the EXPECTED state when sessions are left"
    log "  to expire naturally -- stake only goes on hold after an early close."
    log "  To exercise the claim for real, run --make-onhold and come back in 24h."
    head2 "3.1 the guard must refuse cleanly with nothing to claim"
    run "claim-stakes --confirm" -- "$MORCTL" --host "$HOST" --claim-stakes --confirm
    expect_rc "exits non-zero" 1
    expect_out "says there is nothing to claim" "nothing to claim"
    expect_not_out "no transaction was submitted" "withdrawal submitted"
  else
    log ""
    log "  This withdraws $(fmt_wei "$AVAIL") MOR to the router's wallet and costs gas."
    if confirm "Submit the withdrawal transaction?"; then
      MOR_BEFORE_CLAIM="$(mor_balance)"
      run "claim-stakes --confirm" -- "$MORCTL" --host "$HOST" --claim-stakes --confirm
      expect_rc "succeeds" 0
      expect_out "reports a transaction hash" "tx:"
      log ""
      log "  Waiting 60s for the transaction to confirm on Base..."
      sleep 60
      MOR_AFTER_CLAIM="$(mor_balance)"
      log "      MOR change across the claim: $(delta_wei "${MOR_BEFORE_CLAIM:-}" "${MOR_AFTER_CLAIM:-}")"
      run "stakes after claiming" -- "$MORCTL" --host "$HOST" --stakes
      expect_out "claimable has gone down" "claimable now"
    else
      skip "phase 3 declined"
    fi
  fi
else
  log ""
  log "PHASE 3 skipped (pass --claim to withdraw released stake)."
fi

# ===================================================================== summary

MOR_END="$(mor_balance)"

head1 "SUMMARY"
log "  passed:  $PASS"
log "  failed:  $FAIL"
log "  skipped: $SKIP"
log ""
log "  wallet MOR start: $(fmt_wei "${MOR_START:-}")"
log "  wallet MOR end:   $(fmt_wei "${MOR_END:-}")"
log "  net change:       $(delta_wei "${MOR_START:-}" "${MOR_END:-}") MOR"
log ""
if [ "$DO_SPEND" = "1" ]; then
  log "  A session may still be OPEN on-chain. That is normal and intended:"
  log "  its stake returns automatically about a minute after natural expiry."
  log "  Check it with:  $MORCTL --sessions"
fi
log ""
log "  full log: $LOG"

if [ "$FAIL" -gt 0 ]; then
  log ""
  log "  FAILURES:$FAILED_STEPS"
  exit 1
fi
log ""
log "  All executed checks passed."
exit 0
