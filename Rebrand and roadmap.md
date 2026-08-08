# Rebrand & Roadmap

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

Not a current priority. Tracked here so it isn't lost, and so `morctl`'s scope is
understood as deliberately narrower by design, not by oversight.