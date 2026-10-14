# Rebrand & Roadmap

> **Status (2026-10-13).** The rename is done: the GitHub repo is now
> `aslammpaata/morctl`, the `go.mod` module path, binary names and session-cache
> filename all match, and the old URL redirects. Step 6 below — the find-and-replace
> across `windows-setup-guide.md`, `AUDIT.md`, `STATUS.md`, `booth-runbook.md`,
> `demo-video-script.md` and the developer/headless guide — is **still outstanding**,
> and now carries a second change with it: `morctl` no longer has a default model, so
> any guide step running `--dev`/`--interactive` needs an explicit
> `--model-id 0xbb9e920d94ad3fa2861e1e209d0a969dbe9e1af1cf1ad95c49f76d7b63d32d93`.
>
> Part 2 has partly shipped, in `morctl` rather than as a separate tool: `--diagnose`
> now covers configuration and environment checking. **Install** and **repair** remain
> unbuilt. See the revised status at the end of Part 2.

## Part 1 — Naming & Migration Guide

### Recommended new name: `morctl`

Follows the established `kubectl`/`systemctl`/`etcdctl` convention — immediately signals
"control CLI for Morpheus" to any developer, without implying diagnosis/repair capability
the current tool doesn't have. Backups if unavailable: `mor-flow`, `mor-cli-plus`.

### Rationale

- **Accuracy:** the current tool automates session-open + inference and reports
  wallet/router health — it does not diagnose or repair anything. "Doctor" overpromises.
- **Ecosystem fit:** the tool is increasingly the de facto reliable CLI in the Morpheus
  ecosystem (official `mor-cli` has multiple confirmed, unresolved issues on Windows). A
  name in the `-ctl` family positions it correctly as a peer/operations tool, not a
  side diagnostic script.
- **Frees "Doctor"** for a tool that will actually earn the name — see Part 2.

### Impact on users and documentation

- Anyone with the old binary/repo name in scripts, aliases, or muscle memory needs a
  heads-up. Given adoption is still early (internal team + a handful of testers), impact
  is low if done now rather than later.
- Every doc referencing `morpheus-doctor` needs updating: `windows-setup-guide.md`,
  the developer/headless guide (in progress), STATUS.md, AUDIT.md, and the booth
  materials (`morpheus-doctor.exe` is named explicitly in the pre-provisioning checklist
  and demo video script).
- The GitHub repo itself should be renamed, not re-created — GitHub automatically
  redirects the old URL, preserving stars/history/issue links.

### Migration steps

1. **Rename the GitHub repo:** Settings → repository name → `morctl`. Old links redirect
   automatically.
2. **Rename the binary output** in build commands (`-o morctl` / `-o morctl.exe` instead
   of `-o morpheus-doctor` / `-o morpheus-doctor.exe`).
3. **Update `go.mod` module path** if it references the old name.
4. **Update the session-cache filename** in `main.go` from
   `.morpheus-doctor-session.json` to `.morctl-session.json` (cosmetic, but worth
   matching for consistency — see README diff below).
5. **Add a one-time deprecation note** to the old repo's README (now redirecting)
   pointing at the new name, for anyone who bookmarked it directly rather than via search.
6. **Global find-and-replace** `morpheus-doctor` → `morctl` across:
   `windows-setup-guide.md`, `AUDIT.md`, `STATUS.md`, `booth-runbook.md`,
   `demo-video-script.md`, and the developer/headless guide once drafted.
7. **Update any built `.exe`/binaries already staged** on booth laptops or team machines.

---

## Part 2 — Roadmap: `morpheus-doctor` v2 (future, not current priority)

### Concept

A genuinely diagnostic-and-remedial tool — the name earned properly this time. Distinct
from `morctl` (session/inference operations); this is an installer + health/repair layer
underneath it.

### Planned capabilities

- **Install Morpheus components** — desktop app, headless router, or `morctl` itself,
  detecting the user's OS/environment and choosing the right path automatically.
- **Detect configuration issues** — wrong chain ID, stale/duplicated cookie paths (the
  exact class of bug found in official `mor-cli` on Windows), malformed `models-config.json`
  entries, mismatched RPC endpoints.
- **Repair broken installations** — clear corrupted local state (the badger-DB corruption
  this project hand-diagnosed and fixed manually more than once), regenerate a stale
  cookie, reset to known-good config defaults.
- **Bridge API-based and native/local deployments** — a single tool aware of both the
  hosted API Gateway and the native/P2P proxy-router paths, guiding the user to whichever
  fits their situation rather than assuming one.
- **Validate environment paths, dependencies, runtime configuration** — Go version,
  network reachability, wallet presence, disk space for local models — before a user
  hits a failure mid-setup instead of before it.

### Why this is credible, not speculative

Every capability above maps directly to a real failure this project already diagnosed
and fixed by hand at least once: stale cookie after rotation, testnet/mainnet
misconfiguration, corrupted local badger data, Windows long-path failures during cleanup,
and the mor-cli path-duplication bug. v2's spec can be written almost entirely from this
project's own incident history rather than new research — low-risk to scope accurately
whenever it becomes a priority.

### Status

**Revised 2026-10-13.** Part of this has shipped inside `morctl` instead of waiting for
a separate tool, because the diagnostic half needed no new architecture — it is reading
endpoints the router already exposes.

| Planned capability | Where it stands |
|---|---|
| Detect configuration issues | **Shipped** as `morctl --diagnose`: chain ID (expects 8453), RPC reachability and rate-limiting, stale cookie, allowance, balance, stake on hold, per-model health, badger store health |
| Validate environment / runtime config | **Shipped**, same command — minus Go version and disk space, which aren't `morctl`'s business |
| Install Morpheus components | **Not built.** Still the strongest case for a separate tool; `morctl` assumes a running router by design |
| Repair broken installations | **Not built.** `--diagnose` identifies a dead or throttled RPC but only advises; the repair action (`POST /config/ethNode`) is a candidate behind explicit confirmation |
| Bridge API-gateway and native/local deployments | **Not built** |

What this means for the plan: a `morpheus-doctor` v2 would now be an **installer and
repair tool**, not a diagnostic one. The diagnosis lives in `morctl` and is cheaper to
keep there, next to the session code whose failures it explains. If v2 is ever built, it
should call `morctl --diagnose` rather than reimplement it.