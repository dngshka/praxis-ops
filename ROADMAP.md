# Roadmap

Top-level tracker for praxis-ops: what's done, what's in flight, what's next.
Detailed rationale lives in `docs/`; this file is the map, not the territory
— update it when a phase closes or a milestone's status changes, don't
duplicate the docs' content into it.

## Done

- **Phase A — Base image.** `praxis/ops-base`, built locally into
  `praxis-sbx`'s store (no registry, no egress). `docs/session-02-plan.md`.
- **Phase B — Containment verification.** `security/hardening-check.sh`,
  `security/verify-shell-isolation.sh` — both pass clean against the real
  host. Bidirectional filesystem isolation between `praxis` and
  `praxis-sbx` confirmed empirically.
- **Phase C — Shell access.** `podman exec` proxied through the
  orchestrator's `/shell` (raw HTTP hijack, not RFC 6455 — see below), no
  sshd. `security/preflight-ticket.sh`.
- **Phase D (upgraded) — Orchestrator hardening + observability, all 6
  stages:**
  1. Digest-pinned images (`Runbook.Validate` / `localImageRef`).
  2. Live spawn/get/destroy through the HTTP API, reaper survives a
     restart and rebuilds from container labels alone.
  3. `UsernsMode: auto` — closed a real root-maps-to-`praxis-sbx` bug,
     confirmed live before and after.
  4. Weighted admission (`Runbook.Weight` / `PRAXIS_CAPACITY_WEIGHT`),
     replacing a flat concurrency cap.
  5. `hostmon` — independent, unauthenticated, no-shared-state metrics
     view; `praxis-sbx.slice` as a persistent cgroup parent; `pxoctl` for
     day-to-day ops.
  6. `bench/staircase.sh` real capacity benchmark, rewritten from scratch
     against the real API.
  - **Closed out for real 2026-09-06**, after an earlier 2026-09-03 close-out
    turned out to be invalid (see below). This pass additionally built the
    `ops-systemd` base tier, built and live-verified both SJN-01 and CPT-01
    as real images (not bare `ops-base`), added a per-container disk cap
    (`Runbook.DiskLimit`, closing a real gap where nothing previously
    stopped a `root_in_sandbox` ticket writing unboundedly), and ran real
    capacity benchmarks against both real tickets. Full writeup and final
    numbers: `docs/capacity-benchmark.md`.
    `PRAXIS_CAPACITY_WEIGHT` moved `2` (guess) → `11` (invalid, measured
    against fake data) → **`35`** (real, CPT-01-driven, 70% of its measured
    ceiling of 50).
  - **The 2026-09-03 close-out was invalid** and is kept in
    `docs/capacity-benchmark.md` marked as superseded, not deleted:
    `bench/staircase.sh`'s `IMAGE` resolves independently of `RUNBOOK`, so
    that run spawned bare `praxis/ops-base` sixteen times, not real SJN-01
    — no ticket image had actually been built yet at that point. Caught by
    the user asking directly whether a real ticket had ever been baked into
    the benchmark; it hadn't.
  - Everything in this phase was verified against the real host at every
    step, not just against what compiled. Real bugs this pass found, worth
    remembering: `container.go`'s spec() always overrides an image's own
    baked `CMD` (via `Entrypoint` or a hardcoded `sleep infinity` fallback),
    which meant SJN-01 never actually ran its planted writer process until
    `scenario.yaml` explicitly set `entrypoint:`; `bench/staircase.sh` had a
    `set -e`-under-`pipefail` bug that silently killed the whole script for
    any ticket with no `entrypoint:` field (CPT-01, SKN-01) with zero error
    output; `nginx-light` needed `debootstrap --components=main,universe`,
    not just `--include`; and the real ~60-concurrent-container ceiling
    turned out to be a podman/containers-storage internals limit, not a
    host resource one (see below) — none of these were visible from code
    review, only from real spawns.

Repo pushed to `github.com/d-clz/praxis-ops`; `upgraded-phase-d` merging
into `main` closes this phase.

- **Operator dashboard + browser shell.** Branch `operator-dashboard`, off
  `main` per this doc's own earlier instruction. Scoped in
  `docs/session-03-plan.md`, functional contract in
  `docs/dashboard-spec.md`, API contract in `internal/dashboard/static/openapi.yaml`.
  Five stages, all real-verified (not just built): the API contract
  written first; a real RFC 6455 WebSocket shell (`GET
  .../shell/ws`, `github.com/coder/websocket` — this project's first new
  Go dependency) sitting *alongside* the existing raw-hijack `/shell`,
  not replacing it; `cmd/mockorchestrator` + `internal/mockbackend`, a
  real, runnable, no-podman stand-in wired through the *same*
  `api.New()`/`Routes()` production runs, "for integration ready" per
  explicit request; and the dashboard itself (`internal/dashboard`,
  `go:embed`, served at `/ui/`), checked against every one of
  `docs/dashboard-spec.md`'s acceptance criteria in a real Chrome tab —
  login gating on a real API call, session list matching live data with
  visible staleness, a real xterm.js terminal round-tripping real
  keystrokes, and a visible disconnect banner when a session vanishes
  mid-session.
  - **A real, unrelated bug surfaced along the way**: the very first
    `/shell` test this whole project ever ran used a plain `curl -N -X
    POST` with no `--data` flag, which structurally could never have
    forwarded live keystrokes — the "typing does nothing" it produced was
    a test-tool artifact, not a defect in `ExecShell`/the relay. Confirmed
    by dialing the new real-WebSocket endpoint with an actual
    bidirectional client (a Go test, then a real browser); both worked
    cleanly first try.
  - **Deliberately descoped from the original sketch**: hostmon's
    independent view is not surfaced here — see `docs/dashboard-spec.md`'s
    Non-goals for why folding it in would undermine the two-view model's
    whole point.
  - **Re-verified 2026-09-12 against the real production orchestrator**,
    not just `cmd/mockorchestrator` — a real SJN-01 spawn, a real
    interactive shell confirming every planted process (`cache-warmer`,
    `log-monitor`, `audit-writer`) genuinely running, and a real destroy
    correctly surfacing the disconnect banner. No bugs found this pass;
    full writeup with screenshots: `docs/dashboard-verification-report.md`.

## In flight / keep an eye on

- **The `teardown()`-vs-hostmon-poll race scales with teardown size, not
  just a fixed small chance.** Originally seen twice at low concurrency (1
  sandbox "survived," always a confirmed false positive). At CPT-01's real
  weight-55 teardown, **19 of 55** were reported as surviving — still a
  confirmed false positive (`podman ps -a` empty immediately after), but
  the much larger fraction at higher concurrency suggests the race gets
  worse as concurrency grows, not that it's a fixed rare glitch. Still
  low-priority (never once found a real leaked container across four
  occurrences now), but worth an actual fix before running at
  even-higher real concurrency, rather than continuing to re-verify by
  hand each time.
- **Podman/containers-storage's userns/idmap ceiling (~60 concurrent
  containers) is a real, currently-below-the-radar host constraint, cause
  still not confirmed.** Full diagnosis in `docs/capacity-benchmark.md`'s
  "Known host constraint" section. Confirmed via three separate real
  attempts (original 65,536-UID subuid pool, a 16x-widened pool, and after
  `podman system migrate`) all failing at the identical weight with the
  identical error; this is a known, unresolved-upstream class of
  podman/containers-storage behavior
  ([containers/podman #20139](https://github.com/containers/podman/discussions/20139)).
  **A later finding (below) was briefly thought to explain this and does
  not** — the widened pool from the second attempt above was confirmed
  still live on the host as of 2026-09-15, and the below mechanism would
  need ~1024 fresh spawns to exhaust a pool that size, not 65. Retracted in
  `docs/capacity-benchmark.md`; treat the two as separate until someone
  actually traces both error paths through podman/containers-storage's own
  code.
- **Separate, confirmed finding, 2026-09-14/15: cumulative subordinate-UID
  slice exhaustion, ~1024 total spawns per reset (not concurrent, and not
  the ceiling above).** Live spawn/destroy probing (2 sequential + 8
  concurrent, against the real orchestrator) proved each fresh spawn that
  can't dedupe its template layer permanently claims one 1024-UID slice
  from the subuid pool (confirmed via numeric ownership: `165536`,
  `166560`, `167584`, `168608` — each exactly 1024 apart) and the allocator
  never reclaims a slice, whether or not the container that used it is
  later destroyed. The live pool is confirmed 1,048,576-wide
  (`/etc/subuid`: `165536:1048576` — the widening from the attempt above
  survived the 2026-09-11/12 reset, since `system reset` wipes podman's own
  storage/database, not this host-level file), so 1,048,576÷1024 = roughly
  1024 fresh slices *per reset*, not 64. A perfectly healthy production
  flow (spawn, respect TTL, destroy, repeat) burns this budget exactly as
  fast as holding everything alive at once. Today's confirming probes
  alone spent 8 of the ~1024 slices available since the last reset — real,
  non-renewing cost, cheaper now than the original (wrong) ~64 estimate
  suggested, but not free.
- **The same slice-leak defect permanently leaks disk on every real
  container spawn — a bigger, separate finding from 2026-09-10, distinct
  from either ceiling above.** Leak size is per-ticket, not fixed: SJN-01's
  leaked copies measured 4.0K (near-empty, shares almost everything with
  the common base); CPT-01 (systemd, root-in-sandbox, nginx) is the likely
  source of the ~150MB average documented below — the arithmetic (18,426MB
  reclaimed ÷ ~88 non-SJN-01 copies ≈ 209MB each) points at CPT-01
  specifically, though this was never split per-ticket at the time
  (continuous staircase, no before/after per step) and remains inferred,
  not directly measured. `podman rm`/the orchestrator's `Destroy()` never
  reclaims the private "ID-mapped copy of layer" a container creates to
  view a shared base image under its own UID range — confirmed live,
  ~19GB of orphaned directories survived with zero containers running,
  count matching the week's total spawns, not concurrency. This will
  happen from **normal production operation**, not just benchmarking —
  every real candidate session leaks one, permanently, forever, regardless
  of TTL or `PRAXIS_CAPACITY_WEIGHT`. **No real fix in this MVP — an
  emergency escape hatch only**: `bootstrap/90-storage-reset-rebuild.sh`
  revokes and reinitializes (`podman system reset` — every image, every
  container, podman's whole database — then rebuilds from source). It is
  NOT a maintenance tool or a pruner and must not be run casually or on a
  schedule; it exists only because disk had already hit a real wall once
  and there was nothing safer validated yet. Run and validated for real
  2026-09-11/12: reclaimed 18,426MB, containment re-verified clean at the
  same bar as the original build.
  **The actual fix — deferred to MVP2, but the pattern is now observed
  (2026-09-14)**: of a spawn's 5 `LowerDir` entries, 4 are reliably shared
  across every container from the same image (safe to never touch); the
  5th (topmost, "template") layer is the one that sometimes gets privately
  copied instead of deduped, and it's specifically that copy that leaks.
  Distinguishing "this container's private copy" from "the real,
  must-never-delete shared layer" is therefore a same-image sibling
  comparison, not a guess. But deleting the directory alone is only half
  the fix: the leaked copy also permanently holds a 1024-UID slice out of
  a ~1024-slice-per-reset budget (see the corrected finding above), and
  removing the directory does not by itself return that slice to podman's
  allocator for reuse — confirmed unattempted, not confirmed safe. Also
  learned the hard way and now documented as a hard rule: **never run
  `podman save`, `system check`, or `system migrate` on this host** — all
  three independently made storage *worse* (see
  `docs/capacity-benchmark.md`'s "Second finding" section) despite
  looking like safe, read-only diagnostics.
- **No destroy reason survives past the container itself.** Confirmed by
  reading `internal/api/server.go`'s `get()`: it returns a flat `404 {"error":
  "no such instance"}` whether the attempt_id never existed, expired on TTL,
  just tripped the new disk cap (`praxis.disk-limit-bytes`), or was explicitly
  destroyed. The only place a reason exists at all is a log line and
  `praxis_destroy_total{reason=...}` — a global counter, not a per-attempt
  record. This is consistent with the orchestrator's deliberate statelessness
  ("if the portal loses an attempt record, the container still dies on
  schedule" — `cmd/orchestrator/main.go`'s reaper comment) but it means a
  future portal cannot tell a candidate *why* their session is gone: TTL
  expiry, disk abuse, and a plain typo'd attempt_id are indistinguishable.
  Sketch for later, not built: a short-lived, explicitly-expiring in-memory
  tombstone map (`attempt_id -> {reason, destroyed_at}`, a few minutes'
  window) that `get()` checks before falling through to "no such instance,"
  returning `410 Gone` with the reason instead when a tombstone is still
  live. Deliberately NOT a persistent event log — that would be new state
  the orchestrator has to carry, the opposite of the statelessness the
  reaper's whole design leans on; a bounded, in-memory, best-effort map
  degrades safely back to today's behavior on restart rather than becoming
  another thing that can disagree with reality.

## Next milestones

1. **Bake pipeline.** SJN-01 and CPT-01 now have real, hand-built local
   images (`praxis/sjn-01`, `praxis/cpt-01`) with their real digests
   recorded in `scenario.yaml`, but nothing automates build → digest →
   pin the way a real pipeline would; SKN-01 still has an unbuilt
   `substrate_image: REPLACE_AT_BAKE`. `docs/ops-ticket-spec.md`.
2. **Scoring envelope.** Not started. Grading/check-script execution
   against a submitted attempt.
3. **The portal.** Separate team's deliverable; this repo exposes the
   `X-Praxis-Token`-gated HTTP API for it to integrate against
   (`orchestrator/README.md`) but the portal itself isn't this repo's work.
   **Integration is starting now, not hypothetical** — apidoc access was
   handed over 2026-09-13. Go-forward plan, prioritized by who pays when
   it goes wrong: `docs/portal-integration-plan.md`.
4. **Update 2026-09-26: largely addressed by fuse-overlayfs, pending a
   run on `praxis-sbx`'s store.** Measured in throwaway stores on this
   host (`docs/capacity-benchmark.md`, "the copies are a cache"): after
   every container was removed, the next spawn got the first slice again
   (`0:1:1024`) and reused its copy in 0.15 s, and `podman rmi` removed
   every copy of the image. With `mount_program = fuse-overlayfs`, now the
   default in `bootstrap/30-podman-policy.sh`, no copy is made at all. What
   remains of the entry below: confirm it on `praxis-sbx`'s store (switch,
   `90-storage-reset-rebuild.sh`, containment checks, a staircase).

   **MVP2: real fix for cumulative subordinate-UID slice exhaustion**
   (2026-09-10 disk-leak finding, confirmed live 2026-09-14/15 as a
   ~1024-spawns-per-reset budget — a separate constraint from the
   "concurrency ceiling," not the same mechanism; an earlier draft of this
   entry claimed otherwise and was wrong, see "In flight" above and
   `docs/capacity-benchmark.md`). The `LowerDir` pattern is now
   observed: a container's private, leaked copy is its topmost of 5 layers,
   identifiable by not matching a same-image sibling's copy of the same
   layer. That narrows "delete the leaked directory in `Destroy()`" from a
   guess to a known-safe comparison — but the harder half is unaddressed:
   each leaked copy also permanently consumes one of only ~1024 1024-UID
   slices available per `podman system reset` (pool confirmed
   1,048,576-wide, `/etc/subuid`, 2026-09-15), and deleting the directory
   doesn't confirm the slice becomes reusable. The real fix needs to
   either prove slice reuse works after directory deletion, or bypass
   podman's own `--userns=auto` allocator entirely in favor of the
   orchestrator managing a fixed, explicitly-reusable pool of UID ranges
   itself. Either way, replacing `bootstrap/90-storage-reset-rebuild.sh`'s
   full-nuke escape hatch with something safe enough to run routinely.
   Explicitly out of this MVP's scope; the reset-rebuild script is the
   accepted stopgap until this lands, and further live probing of the
   mechanism itself now competes with real usage for the same ~1024-slice
   budget, so should not be done casually either. Sequenced in
   `docs/portal-integration-plan.md`'s P2 (waits on the still-unresolved
   original ceiling and on proving slice reuse, neither of which this
   MVP has answered yet).
5. **Root cause of the original ~60-64 concurrency ceiling — genuinely
   unresolved, not just deprioritized.** `docs/capacity-benchmark.md`'s
   "Known host constraint" section. Confirmed real via three independent
   remediation attempts, all failing identically; confirmed *not*
   explained by item 4's slice-exhaustion mechanism (2026-09-15 — the
   widened pool item 4 measures against was already live during the
   original failing test). Needs someone to trace the `65537:65537`
   request through podman/containers-storage's own code; nothing found
   from this project's side has resolved it.
6. **Every ticket still leaves `Runbook.Weight` unset (flat weight=1),
   despite now having real comparative cost data.** SJN-01 and CPT-01 have
   measurably different real resource profiles (CPT-01 hits a disk ceiling
   at 50, SJN-01 doesn't until a podman internals limit at 60) — weighted
   admission exists specifically to let heavier tickets consume more of
   `PRAXIS_CAPACITY_WEIGHT` per instance, but nothing has ever set a
   non-default weight to make use of that. Candidate follow-up, not
   scoped further here.
