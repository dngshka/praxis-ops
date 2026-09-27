# Capacity benchmark

Living record of `bench/staircase.sh` runs against the real host. Append a
new dated section per run rather than overwriting — the point is to see how
the number moves as tickets/images/host load change over time.

## How to read a run

`bench/staircase.sh` climbs `PRAXIS_CAPACITY_WEIGHT` one step at a time
against a single ticket, holding every previously-spawned sandbox alive
while adding more, until a real stop condition trips (PSI full avg60 >10%,
storage <20% free, an OOM kill, spawn p95 >20s, neighbour health >2000ms) or
it reaches the script's own `MAX_WEIGHT` ceiling first. Reaching
`MAX_WEIGHT` without tripping anything is **not** the same as finding the
box's real limit — it means the box held at least that many, and the run
didn't ask further. Say so explicitly whenever it happens; don't round it
up to "the limit."

---

## Known host constraint: podman userns/idmap ceiling (~60-64 containers)

**A separate, later finding exists further down this document
("2026-09-14/15 — a second, real, distinct leak"). It does not explain
this section's ceiling** — that section's own opening paragraph retracts
an earlier claim that it did, once the widened pool this section describes
was confirmed still live on 2026-09-15. Read both; don't assume one
resolves the other. This section's ~60-64 ceiling and its `65537:65537`
error text remain unexplained.

Discovered 2026-09-04/05, running the real SJN-01 staircase (`docs/capacity-benchmark.md`'s
own SJN-01 entry below). This is not one of `bench/staircase.sh`'s four
documented stop conditions (PSI, storage, OOM, neighbour) — it's a fifth,
previously-unknown failure mode that turned out to be the actual binding
constraint on this host, arriving well before any of the four monitored
conditions got close to tripping.

### Symptom

Every SJN-01 staircase run stopped at the exact same point — held 60
concurrent containers cleanly, failed on the 65th spawn attempt — across
three separate attempts, none of which moved the number even slightly:

1. Original run: `/etc/subuid`/`/etc/subgid` at the host's auto-assigned
   `165536:65536` (65,536 total delegated subordinate UIDs/GIDs for
   `praxis-sbx`). Failed at weight 65.
2. Widened `/etc/subuid`/`/etc/subgid` to `165536:1048576` (16x larger) and
   redeployed. **Failed at weight 65 again, identical error text.**
3. Ran `podman system migrate` (podman's own suggested remedy, printed in
   the error message itself) on top of the widened range. **Failed at
   weight 65 again, still identical error text.**

The orchestrator's log for the failing spawn (`journalctl --user -u
praxis-orchestrator`):

```
create failed err="create sbx-bench-...-064: Error response from daemon:
container create: creating container storage: creating an ID-mapped copy
of layer \"...\": creating copy of template layer \"...\" with ID \"...\":
potentially insufficient UIDs or GIDs available in user namespace
(requested 65537:65537 for /home/praxis-sbx/.local/share/containers/
storage/overlay/...): Check /etc/subuid and /etc/subgid if configured
locally and run \"podman system migrate\": chown ...: invalid argument"
```

The requested value (`65537:65537` — exactly `65536 + 1`) never changed
across any of the three attempts, despite the real subuid pool size
changing by 16x. That alone rules out subuid pool size as the actual
constraint, whatever the error message's own suggested remedy implies.

### What was ruled out, with real measurements

- **Not host resource pressure.** At the last held weight (60), real memory
  usage was ~1.6GB out of 14GB (~11%), storage was 52% free (well above the
  20% floor), PSI was `0.00`/`0.00`, and zero OOM kills — all four of
  `bench/staircase.sh`'s real stop conditions were nowhere close.
- **Not loop devices.** `losetup -a` showed exactly 1 active loop device at
  failure time, against a `max_loop` module parameter of 8 — nowhere near
  exhausted.
- **Not the kernel's global user-namespace limit.**
  `/proc/sys/user/max_user_namespaces` reported `50238` — vastly more than
  64.
- **Not the subuid/subgid pool size**, confirmed empirically as above
  (widening it 16x changed nothing).

### What it actually is

The error originates from podman/`containers-storage`'s "ID-mapped copy of
layer" mechanism — an optimization that uses the kernel's ID-mapped mounts
feature (`mount_setattr(MOUNT_ATTR_IDMAP)`) to let multiple `--userns=auto`
containers share the same underlying base image layer without a full
copy-on-write duplication per container. This is a distinct code path from
plain container spawning, and it's the thing that's actually failing here
— not container creation in general.

This is a real, if murky, class of podman/containers-storage behavior, not
specific to this host or this project's configuration. From public
reports of the same error class:

- [containers/podman discussion #20139](https://github.com/containers/podman/discussions/20139) —
  the closest match found: a user hit the identical error class (there, on
  `podman import`, triggered by a single file inside the image owned by a
  GID outside the available subordinate range). The discussion was never
  conclusively resolved upstream — the reporter's own summary after two
  weeks: *"I suspect that it either should have been filed as an issue, or
  nobody has any ideas."* A later commenter (months afterward) confirmed
  hitting the same thing with no fix either. The only workaround mentioned
  (`--storage-opt ignore_chown_errors=true`) applies to `podman import`
  specifically, not `container create`'s ID-mapped-copy path this project
  actually hit, and even its own reporter was unsure what it silently
  breaks.
- [containers/podman issue #12715](https://github.com/containers/podman/issues/12715) and
  [Red Hat Solution 7005221](https://access.redhat.com/solutions/7005221) —
  same error family on image pull; consistent with this being a known,
  recurring rough edge in how podman's rootless userns/idmap machinery
  behaves, not a one-off.

Given upstream itself hasn't nailed down a fix for the same error class,
further chasing this from the application/config side (this project has no
access to podman/containers-storage internals) has poor expected payoff.

### What this means for capacity planning

**On this host, with the currently-installed podman version, ~60 concurrent
`--userns=auto` containers is the real, binding ceiling — for any ticket,
not just SJN-01.** It's a property of container creation generally (the
ID-mapped layer copy path engages for every `--userns=auto` spawn sharing a
base layer), not of SJN-01's specific resource profile. CPT-01's own
staircase run may hit this same wall, independent of whatever memory/PSI
behavior it shows on its own.

This ceiling could plausibly move with a podman/containers-storage version
upgrade, or by disabling the ID-mapped-copy sharing optimization if a
config toggle for it exists and its performance/correctness trade-off is
understood — neither investigated here, since it's a genuinely separate
piece of work from ticket capacity planning and the upstream trail runs
cold. Worth a dedicated follow-up if headroom above ~60 concurrent real
assessments is ever actually needed.

### Second finding, 2026-09-10: the same defect also permanently leaks disk, and `save`/`system check`/`system migrate` make it worse

Discovered chasing an unrelated question ("why is `~praxis-sbx/.local`
using 19GB?" — real content across all four images is ~278.5MB, confirmed
via direct `du` on each image's `GraphDriver.Data.UpperDir`; the rest was
orphaned). Two separate, now-confirmed facts, not one:

**Fact 1 — the ID-mapped-copy mechanism leaks a full physical copy on every
spawn, permanently, regardless of container lifetime.** `podman rm` (via
the orchestrator's own `Destroy()`, confirmed identical) correctly removes
a container's own top read-write layer — that part of podman works fine,
`podman ps -a` came back empty after every teardown, every time. What it
never touches is the *separate* "ID-mapped copy of the shared base layer"
each container privately created to get a properly-owned view of read-only
content under its own unique subordinate UID range. That copy survives the
container that made it, forever. 148 orphaned ~150MB directories were
found on disk, matching almost exactly the total container-spawn count
across the whole benchmarking week (SJN-01 ≈60 + CPT-01 ≈55 + dozens of
verification/hardening-check spawns) — this is a leak from **normal
orchestrator operation**, not a diagnostic-tooling artifact. Every real
candidate session will leak one of these in production, permanently,
independent of TTL or `PRAXIS_CAPACITY_WEIGHT`.

Ruled out as an explanation: `dmesg -T | grep -i -E "overlay|idmap|loop"`
returned nothing at the time this was investigated, despite the mechanism
having fired 148+ times — no kernel-level idmap failure is being logged.
The copy-fallback appears to be a decision inside podman/containers-storage's
own userspace code, not a kernel/filesystem-level failure being caught and
worked around (an earlier draft of this section suspected the loop-mounted
storage volume specifically; the dmesg check doesn't support that, and no
further evidence was gathered either way).

**Fact 2 — `podman save`, `podman system check`, and `podman system
migrate` are NOT safe read-only operations on this host; all three
independently made things worse.** Established via direct on-disk
timestamp evidence (`find -printf '%T@'` on the orphaned directories),
which ruled out the two more obvious suspects: not the two `system
migrate` runs from days earlier (wrong time), and not spread across the
whole benchmarking week (wrong time too) — every single flagged
directory's mtime clustered within minutes of a `podman save
praxis/ops-base praxis/ops-systemd praxis/sjn-01 praxis/cpt-01` command
run specifically to make a pre-repair backup:

- `podman save` given multiple image names silently resolved **all four**
  to the same wrong blob (`ops-base`'s) — every save/load round-trip
  produced a tarball where `sjn-01`/`cpt-01`/`ops-systemd`'s tags pointed
  at `ops-base`'s content. Recovered without data loss only because the
  real, correctly-content-addressed images were still present in storage
  under their original IDs, just untagged — re-tagging by hand fixed it.
  A save/load cycle trusted at face value would have silently lost track
  of three of four real images.
- `podman system check` (run immediately after, no `--repair`/`--force`)
  reported 145 of 148 overlay directories as "damaged" — including **all
  four of the real, currently-tagged, already-live-verified images**, not
  just the genuine orphans. Every file within a flagged layer showed the
  identical "before" mtime shifting to the identical "after" value,
  consistent with the check's own verification walk triggering the same
  broken copy mechanism merely by reading a layer, then reporting the
  metadata drift it just caused as pre-existing "damage". `--repair`'s own
  description ("Remove inconsistent images") would very likely have
  deleted all four real images along with the actual garbage, given they
  were flagged identically to the orphans.
- Direct `du` measurement (bypassing podman's own reporting) also caught
  `podman images`/`system df` reporting `ops-base` at 2.04GB against a
  real on-disk size of 125MB — a ~16x inflation, and `sjn-01` similarly
  off by ~55x (19.53MB reported vs 352KB real). Podman's own size
  bookkeeping is unreliable on this host too, a further reason not to
  trust its self-reported numbers for capacity decisions going forward —
  use `du`/`df` directly instead.

**The only operation validated as safe here: `podman system reset`**,
which wipes both the files and podman's own database consistently, rather
than trusting any surgical/partial operation to know what's safe to touch.
See `bootstrap/90-storage-reset-rebuild.sh` — a scripted reset-and-rebuild,
refusing to run against a live session, restoring from source
(`bootstrap/60-build-base.sh`, `61-build-systemd-base.sh`, each ticket's
`Containerfile`/`seed.sh`) rather than from any backup. **Do not run
`podman save`, `system check`, or `system migrate` on this host outside of
that script's own controlled use** — diagnose disk/image state with `df`,
`du`, `podman ps -a`, and `podman inspect` on a specific known ID instead.

**Run and validated for real, 2026-09-11/12**: `bootstrap/90-storage-reset-rebuild.sh`
reclaimed **18,426MB** (25GB volume: 19GB used / 82% → 815MB used / 4%),
confirming the entire ~19GB really was the leak, not legitimate content.
Both base images and both ticket images rebuilt clean; containment
re-verified at the identical bar as the original build — `50-verify.sh`
28/28 on both `ops-base` and `ops-systemd`, `verify-shell-isolation.sh`
7/7. New ticket digests (content changed because the rebuild is a fresh
debootstrap, not because anything about the tickets' seeded behavior
changed): `praxis/sjn-01@sha256:31a20c68...5925bd4`,
`praxis/cpt-01@sha256:59664841...e9d2115` — recorded in each ticket's
`scenario.yaml`. Not re-run: Stage-4-style behavioral verification (does
SJN-01's writer process actually start, is CPT-01's nginx actually seeded
disabled) — recommended before trusting these two specific images for
anything beyond structural/containment purposes.

The real fix — a targeted cleanup in the orchestrator's own `Destroy()`
path that removes a container's specific leaked copy instead of
periodically resetting everything — is a genuine, separate piece of work,
deferred rather than built here; see `ROADMAP.md`.

---

## 2026-09-14/15 — a second, real, distinct leak: cumulative subordinate-UID slice exhaustion. It does NOT explain "Known host constraint" above — that stays unresolved

**Correction, not a supersession — retracting part of what this section
originally claimed.** A first pass at this (written 2026-09-14, since
edited) claimed the per-spawn slice leak below was "the same mechanism" as
the concurrency ceiling documented in "Known host constraint" above, and
that it "fully explain[ed]" why widening the subuid pool 16x didn't move
that ceiling. Both claims did not survive one direct check the next day
(2026-09-15): `cat /etc/subuid /etc/subgid` on the live host shows
`praxis-sbx:165536:1048576` — **the 16x-widened pool from the original
benchmark week is still live; it was never reverted by the 09-11/12
`system reset`.** That reset wipes podman's own storage/database, not this
host-level OS file.

This matters because the *original* widened-pool test — documented above,
"Failed at weight 65 again, identical error text" — was run against this
exact same 1,048,576-wide pool, not the original 65,536-wide one. If the
mechanism below (a 1024-wide slice consumed per spawn, never reclaimed)
were what caused that failure, it should have taken roughly 1024 fresh
spawns to exhaust a pool this size, not 65. **It failed at 65 anyway, with
16x the room the slice-leak mechanism would have needed.** That is
sufficient to retract the unification: these are two separate findings
that happen to land near the same-sounding number, not one mechanism with
one explanation. "Known host constraint"'s own ~60-64 ceiling and its
`65537:65537` error text remain exactly as unresolved as they were before
this section was first written — still real, still not explained by
anything found this week. Do not read the rest of this section as closing
that question.

### What is actually confirmed here, on its own terms

Prompted by re-running small, controlled spawn/destroy probes against the
live orchestrator (`leak-probe-01`/`02` sequential, then 8 concurrent
probes) rather than a full staircase — deliberately cheaper than the
original benchmark, since this was diagnostic, not capacity-finding:

- Every spawn's `GraphDriver.Data.LowerDir` has 5 entries. 4 of them are
  byte-identical across every probe, sequential or concurrent — real,
  working layer sharing, exactly as overlay is supposed to behave.
- The 5th (topmost, "template") layer is not reliably shared. Two
  sequential spawns deduped onto the same copy; of 8 concurrent spawns, 7
  each raced into creating their own private one instead. **The
  discriminating variable is not yet confirmed to be concurrency itself**
  — it may instead be whether a prior container holding that same layer is
  still *alive* at spawn time (the two sequential spawns never destroyed
  the first before the second ran). This also better fits the original
  week's ~148 orphans from a *mostly-sequential* staircase, which
  shouldn't have leaked much at all under a pure
  concurrency-vs-sequential read. Unresolved; see the hostmon gauge
  proposal below for a way to answer this from normal traffic instead of
  paying for more probes.
- **All 7 private copies survive `Destroy()`/`podman rm`, forever** — same
  leak class as the 2026-09-10 finding above, now reproduced live and
  narrowed to the specific layer responsible.
- Their **numeric ownership** is real evidence of a *different* kind of
  exhaustion than the one above: `find -printf '%U'` on three of them
  returned `166560`, `167584`, `168608` — each exactly 1024 apart,
  `bootstrap/30-podman-policy.sh`'s own default slice size. A fourth, from
  the very first probe run (`9a6ecbd1...`), sits at `165536` — the pool's
  own opening offset. **Whether this 1024-wide-slice number is the same
  quantity as the `65537:65537` requested in the original ceiling's error
  text is not established** — `65537` is suspiciously close to
  `65536 + 1` and may be a relative/internal-namespace value rather than
  an absolute host UID in the `165536`-based space measured here. Treat as
  two separate numbers until someone actually traces both through
  podman/containers-storage's own code, not just pattern-matches on
  round numbers — the retraction above is exactly what happens when that
  isn't done.

**The allocator hands out slices sequentially and never reclaims one**,
regardless of whether the container that received it is still running,
destroyed, or was destroyed minutes ago. That is a cumulative,
monotonically-increasing pointer into the pool as actually configured —
1,048,576 wide, confirmed live — so 1,048,576÷1024 = **roughly 1024 fresh
slices available per reset**, not 64. Correcting the number from the first
pass at this section, which computed it against the pre-widening pool size
this host no longer has.

**Leak size is per-ticket, not fixed.** SJN-01's private template-layer
copies measured **4.0K** each — near-empty, since SJN-01 shares almost
everything with the common base image and adds only a couple of small
scripts on top. That does not match the 2026-09-10 finding's ~150MB
average — the arithmetic actually points somewhere more specific than
"flagged, not yet measured": 18,426MB reclaimed, SJN-01's ~60 copies at
4KB contributing next to nothing, leaves ~88 copies (CPT-01's ~55 plus the
week's "dozens of verification/hardening-check spawns") averaging **~209MB
each** — likely **CPT-01** (systemd, root-in-sandbox, nginx plus its own
config), whose ticket-specific top layer is plausibly much larger than
SJN-01's couple of scripts. Still inferred, not directly measured — one
`du -sh` on a single CPT-01 leaked copy after one real spawn would confirm
or kill it, at a cost of one slice, spent deliberately.

### What this means for capacity planning (revised, corrected)

Two separate, real constraints, not one:

1. **"Known host constraint" above — ~60-64 *concurrent* containers,
   cause still not confirmed.** Real, reproducible, unresolved. Nothing
   found this week explains it; the unification claimed in this section's
   first pass was wrong.
2. **This section — roughly 1024 total spawns needing a fresh
   template-layer copy, since the last full `podman system reset`, not
   1024 concurrent containers.** A perfectly healthy production flow
   (spawn, run, respect TTL, destroy, repeat) burns through this budget
   exactly as fast as a pile of concurrent ones would. `PRAXIS_CAPACITY_WEIGHT=35`
   governs concurrent admission and remains sound on its own terms, but it
   does nothing to protect against this cumulative exhaustion — a host
   could sit at low concurrent weight indefinitely and still run out,
   just much later than 64 would have suggested (roughly 1024, per the
   pool actually configured).

Testing this second constraint itself has a real cost: today's probes (2
sequential + 8 concurrent) consumed 8 of the ~1024 slices available since
the last reset, purely to confirm the mechanism — real, non-renewing
budget, not a free diagnostic. Cheaper now that the number is ~1024
instead of ~64, but still worth spending deliberately, not incidentally.

The real fix target for constraint 2 (see `ROADMAP.md`'s MVP2 entry,
updated to match): reclaiming a container's own leaked copy in `Destroy()`
only helps if the freed slice becomes available for *reuse*, which
requires understanding how podman's allocator decides "next free slice"
well enough to either reset it safely per-container or replace it with
the orchestrator managing its own reusable pool of fixed UID ranges
directly. Neither has been attempted. Constraint 1 has no fix target at
all yet — it needs its own root cause before one can be proposed.

A cheaper path to more of this than further paid probing: a hostmon gauge
tracking the highest surviving owner UID under the overlay directory
(`praxis_userns_slices_used`), warning well before either constraint's
real ceiling, would surface both the sequential-vs-alive question above
and genuine exhaustion from normal traffic — at zero additional cost.
Proposed, not built.

---

## 2026-09-26 — the copies are a cache, and fuse-overlayfs removes them

Run from the praxis side (the UET demo stopped at 5-6 Medusa sandboxes) in
throwaway rootless stores as the `praxis` user, not in `praxis-sbx`'s store
and not through the orchestrator: same host, podman 5.7.0, kernel
7.0.0-34, the orchestrator's systemd-tier spawn flags (`container.go`
`spec()`), `mountopt=nodev`, and the real praxis MED-05/06 images. Full
write-up: praxis repo, `sandbox/docs/store-experiment-2026-09-26.md` on
branch `feat/sandbox-session`.

| | kernel overlay (current) | `mount_program = fuse-overlayfs` |
|---|---|---|
| 3 concurrent MED-06 spawns | 154 s each | 0.16-0.25 s |
| store growth per sandbox | ~115k inodes, ~1.4 GB at rest | none (+62 inodes for 3) |
| after `podman rm` of all | copies stay | nothing to stay |
| respawn on a used slice | 0.15 s, copy reused | 0.2 s |
| `podman rmi` of the image | removes every copy of it | n/a |
| 8 concurrent MED-06 | not run | all < 1 s, app healthy in ~24 s, ~0.5 GB RAM each |
| file reads inside (node_modules, warm) | 1.4 s | 4.4 s |

What it changes above:
- **Not a per-spawn leak.** A copy belongs to an (image, uid slice) pair. It
  outlives the container and is reused by the next container of that image
  on that slice, and `podman rmi` of the image removes all its copies. The
  store grows with images x peak concurrent slices; an image rebuild that
  removes the old image reclaims its copies. The 2026-09-10 orphans were
  found on a store that `podman save`/`system check` had also damaged; that
  is not reproduced here.
- **fuse-overlayfs makes no copies**, so the store stays at the size of its
  images, first spawns stop costing minutes, and the binding constraint
  becomes RAM/CPU. It is now the store default in
  `bootstrap/30-podman-policy.sh`, checked by `50-verify.sh`.
- Not measured: more than 8 concurrent sandboxes, the ~60-64 ceiling of
  "Known host constraint" under fuse-overlayfs (its error comes from the
  ID-mapped copy path, which fuse-overlayfs does not take), and
  `praxis-sbx`'s own store after the switch -- re-run `50-verify.sh`,
  `security/verify-shell-isolation.sh` and a staircase there.

---

## 2026-09-27 — a class launching together: fuse-overlayfs, then `shared_from_image`

Same setup as 2026-09-26 (throwaway store as `praxis`, never `praxis-sbx`'s),
plus this branch's orchestrator driving a podman API service on that store,
so launches go through `POST /instances` exactly as the portal sends them.
20 sandboxes launched at the same second; "ready" = the student can see the
ticket's problem (MED-05: Medusa failed its DB check; MED-06: Medusa up and
Postgres' slots held by the leaking report role), watched from inside by a
probe that forks almost nothing. Full write-up: praxis repo,
`sandbox/docs/launch-time-2026-09-27.md` on `feat/sandbox-session`.

| 20 at once, slowest ready | kernel overlay | fuse-overlayfs | fuse-overlayfs + `shared_from_image` |
|---|---|---|---|
| 10 MED-05 + 10 MED-06 | 23 min (copies made one after another, ~70 s each) | 35-41 s | 27-29 s |
| 20 MED-06 | not run | 63-68 s | 47-49 s |
| CPU for 20 MED-06 boots | -- | sandboxes ~200 s, fuse-overlayfs ~30 s | sandboxes ~160 s, fuse-overlayfs ~6-8 s |

- **Switching an existing store needs no reset.** A store made on the kernel
  driver works through fuse-overlayfs at once; `rmi` + `load` from the
  original tar reclaims an image's old copies and gives back the same image
  ID. The first fuse-overlayfs mount writes `overlay/.has-mount-program`, and
  the store keeps using fuse-overlayfs even with the line removed from
  `storage.conf`: going back needs `90-storage-reset-rebuild.sh`.
- **What is left is CPU.** A Medusa start costs ~5 CPU-seconds whatever the
  driver (Node compile cache, V8 flags and `NODE_ENV` changed nothing), so 20
  saturate 4 threads. Under fuse-overlayfs another ~15% goes to the daemon
  serving ~97k `node_modules` reads per boot.
- **`Runbook.SharedFromImage`** (this branch) takes those reads off fuse: the
  orchestrator bind-mounts `<PRAXIS_SHARED_DIR>/<image id><path>` read-only
  over the image's path, when the host has such a copy. The praxis loader
  (`load-sandbox-image.sh`) makes it from the loaded image itself. Inside the
  sandbox the tree is owned by `nobody` (no idmapped mounts rootless) and
  read-only. Every MED-05/06 fix and sabotage in the ticket's `fixes.yaml`
  grades as before on sandboxes started this way.
- `fsync=0,fast_ino=1` passed to fuse-overlayfs (`overlay.mountopt`) saved
  ~4 CPU-seconds of 230 and no wall time; not adopted.
- Not run here: `50-verify.sh` / `verify-shell-isolation.sh` on
  `praxis-sbx`'s store (need sudo), and the ~60-64 ceiling of "Known host
  constraint" under fuse-overlayfs.

---

## 2026-09-03 — SJN-01, weight 16 — SUPERSEDED, see 2026-09-04/05 below

**This entry does not measure SJN-01.** `bench/staircase.sh`'s `IMAGE` is
resolved completely independently of `RUNBOOK`/`scenario.yaml`, and no
ticket image had ever actually been built yet at this point — this run
spawned bare `praxis/ops-base` sixteen times (idle, no seeded fault, no
planted processes), not real SJN-01 containers. The resource *envelope*
below (memory/cpu/pids ceilings) was accurate to SJN-01's `scenario.yaml`,
but the *contents* were not. Kept for the record, not deleted — the lesson
about the benchmark script's own `IMAGE`/`RUNBOOK` independence is real and
worth remembering. Do not use any number in this section for capacity
planning; see the real results below.

- **Ticket:** SJN-01 (`ops-base` image, no systemd — the only ticket
  buildable right now; CPT-01 needs the still-missing `ops-systemd` tier).
- **Result file:** `bench/results/20260903T172522Z/`
- **Stopped because:** reached `MAX_WEIGHT=16` without tripping a stop
  condition. **This is a ceiling we chose, not one the host hit.** Nothing
  in the run — PSI, storage, OOM, latency, neighbour health — got
  meaningfully close to its stop threshold at weight 16. SJN-01 alone would
  almost certainly hold more; this run just didn't go looking for where.
- **Neighbour check:** `http://127.0.0.1:3010` (the portal team's own
  service, per [[portal_team_service]] — not GitLab as originally assumed
  when this URL was picked. Still a valid contention probe regardless of
  which service answers on it.)

### What the data actually shows

**Memory/PSI: a non-event.** `psi_some` and `psi_full` read `0.00` for
every sample across all 16 steps, and `oom_kills` stayed `0` throughout.
`slice_mem_bytes` grew from ~12.3–12.5MB at weight 1 to ~27MB at weight 16
— roughly 1–2MB per idle sandbox. SJN-01 has a 512MB per-container memory
*limit*, but an idling SJN-01 container uses a tiny fraction of it. Memory
was nowhere close to being the constraint in this run.

**Storage: declining smoothly, plenty of runway left.** `storage_free_pct`
dropped from 98% (step 1) to 90% (step 16) — roughly linear, ~0.5 points
per additional concurrent sandbox (each new container's overlay writable
layer). At that rate, reaching the 20%-free stop condition would take on
the order of another ~140 sandboxes held concurrently — storage is a real
eventual constraint but wasn't remotely close to binding at weight 16.

**Spawn latency: a step change, then flat — not a climb.** The raw
per-spawn latencies (`spawns.csv`):

| step | latency (s) |
|---|---|
| 1 | 0.24 |
| 2 | 0.20 |
| 3 | 5.34 |
| 4 | 4.79 |
| 5 | 4.74 |
| 6 | 9.54 |
| 7 | 7.03 |
| 8 | 4.69 |
| 9 | 4.72 |
| 10 | 4.66 |
| 11 | 4.74 |
| 12 | 4.98 |
| 13 | 4.79 |
| 14 | 4.82 |
| 15 | 4.85 |
| 16 | 4.76 |

Steps 1–2 spawn near-instantly (0.2s, empty/cold box), then latency jumps
to a ~4.7–5.0s plateau from step 3 onward and **stays flat through step
16** — it does not keep climbing as weight increases. That points to a
roughly fixed per-spawn cost (image/overlay setup, userns mapping, cgroup
creation) that shows up once a couple of containers already exist, rather
than contention that gets worse with concurrency. The two outliers (9.54s
at step 6, 7.03s at step 7) look like transient host jitter, not a trend —
step 8 immediately drops back to the 4.7s plateau. `spawn p95` in the
report (7.03s) is the 15th of 16 sorted samples (nearest-rank, floor
method), which is why it reads *below* the single 9.54s max.

### Caveat: this measures idle capacity, not active-candidate capacity

`bench/staircase.sh` spawns and holds — it doesn't simulate a candidate
actually typing commands, running builds, or generating load inside the
shell. An idle SJN-01 sandbox costs almost nothing in memory (confirmed
above). A real assessment session will cost more than this benchmark
measured, by an amount this run doesn't quantify. Treat the weight-16
result as a **lower bound on cost**, not a prediction of real production
load.

### Recommendation (superseded)

~~Set `PRAXIS_CAPACITY_WEIGHT=11`~~ — see the 2026-09-04/05 entries below
for the number actually used. The reasoning here (70% of a weight-16 floor
measured against an idle non-ticket) does not survive contact with the
real ticket images; kept only so the historical reasoning chain is visible.

---

## 2026-09-04/05 — SJN-01, real ticket, weight 60 (real ceiling, host-independent cause)

- **Ticket:** SJN-01, real image (`praxis/sjn-01@sha256:c561...b7c52`,
  built from `tickets/SJN-01/Containerfile` against `praxis/ops-base`).
- **Result files:** four attempts, all consistent —
  `bench/results/20260904T171525Z/` (invalid: hit the then-current
  `PRAXIS_CAPACITY_WEIGHT=11` admission gate, not a real limit — a
  methodology mistake, corrected for the next three),
  `bench/results/20260904T195341Z/`, `bench/results/20260904T211012Z/`
  (after widening `/etc/subuid`/`/etc/subgid` 16x), and
  `bench/results/20260905T001607Z/` (after `podman system migrate` on top
  of the widened range — the canonical, final result).
- **Host:** Intel Core i5-7500 @ 3.40GHz, 4 cores/4 threads, Linux
  7.0.0-31-generic (Ubuntu 26.04 "resolute").
- **Stopped because:** a podman/containers-storage userns/idmap internals
  ceiling, not a host resource limit — full diagnosis in "Known host
  constraint" above. **Confirmed real and reproducible**: identical failure
  at the identical weight across three attempts, including two real
  attempts to fix it (widening the subuid pool 16x, then `podman system
  migrate`), neither of which moved the number at all.

### What the data shows

**Host resources were healthy the whole time — this was not a resource
squeeze.** At the last held weight (60): memory ~1.6GB of 14GB (~11%),
storage 52% free (well above the 20% floor), PSI `0.00`/`0.00`, zero OOM
kills. The stop condition that actually fired isn't one of
`bench/staircase.sh`'s four monitored ones at all.

**Real per-instance memory cost is ~15x higher than the superseded
ops-base run suggested**, and it keeps climbing for as long as a session
runs. At weight 11 in an intermediate real-ticket attempt, `slice_mem_bytes`
was already ~275MB (~25MB/instance) versus the fake run's ~27MB *total* at
weight 16. Within a single held step (constant weight), memory climbed
continuously — e.g. 14.4MB→16.8MB over one 180s step at weight 1 — almost
certainly page cache for the actively-growing `/var/log/app/service.log`
file the planted writer never stops appending to. A real SJN-01 session
gets more expensive in memory terms the longer it runs unresolved, not
just with added concurrency.

**Spawn latency has a large, real cold-cache tax — and it's not what you'd
pay in steady-state production.** The first real-ticket attempt (image
never spawned before) plateaued at ~4.7–5.0s per spawn from step 3 onward.
Every subsequent attempt against the *same, now-cached* image spawned
consistently in **0.15–0.4s** — roughly 15–20x faster, including the very
first container of each of those later runs. Podman's local image/layer
cache being warm is the deciding factor, not something about the box
warming up generally. Since a real deployment reuses the same pinned
ticket image across every candidate session, **the realistic steady-state
spawn cost is ~0.2–0.4s, not the ~5s a cold first-ever spawn costs** — size
expectations (and any spawn-latency stop condition) around the warm number.

### Caveat

This is a podman-version/host-specific internals limit, not a law of
physics — it could plausibly move with a podman/containers-storage
upgrade. Treat 60 as "the real number for this deployment today," not an
architectural ceiling of the project itself.

---

## 2026-09-05 — CPT-01, real ticket, weight 50 (real ceiling, genuine host resource limit)

- **Ticket:** CPT-01, real image (`praxis/cpt-01@sha256:7e1f...9435`,
  built from `tickets/CPT-01/Containerfile` against the new
  `praxis/ops-systemd` base — `bootstrap/61-build-systemd-base.sh`).
- **Result file:** `bench/results/20260905T085953Z/` (two earlier attempts
  the same day, `20260905T085152Z`/`20260905T085656Z`, died before their
  first spawn due to a real `bench/staircase.sh` bug — a `set -e` pipeline
  failure whenever a ticket has no `entrypoint:` field, fixed same-day,
  commit `8d61556`).
- **Stopped because:** `container storage 19% free < 20%` — **the first
  genuinely host-resource-driven stop condition this whole benchmarking
  pass found.** This is real, not an artifact: storage actually crossed
  the configured floor.

### What the data shows

**CPT-01 is disk-bound well before it's memory-bound, and well before the
podman userns ceiling that capped SJN-01.** `storage_free_pct` declined
from 51% (weight 5) to 19% (weight 55) — **~0.64 points per additional
container**, meaningfully faster than SJN-01's ~0.45 points/instance. The
heavier `ops-systemd`-derived image (systemd + nginx-light + more seeded
files) means a bigger overlay writable layer per spawn, so storage runs out
at a *lower* concurrency (50) than the userns wall (~60) would have allowed.

**Memory is cheap and essentially flat per instance** — unlike SJN-01,
there's no runaway writer here (nginx is seeded disabled, per `seed.sh`'s
planted faults). `slice_mem_bytes` grew from ~121.5MB (weight 10) to
~315MB (weight 50): **~4.8MB per additional instance**, holding steady
within each step rather than climbing over time. PSI stayed `0.00`/`0.00`
and OOM stayed `0` throughout — memory was never close to binding.

**Spawn latency plateaus immediately, same shape as SJN-01, at a slightly
higher baseline.** After one cold first spawn (0.22s — already cache-warm
from the earlier failed attempts same day), every subsequent spawn held at
~5.4–6.3s through weight 55, `spawn p95` = 5.82s, no degradation trend with
concurrency. The ~0.6s-higher plateau than SJN-01's ~4.8–5.0s is consistent
with a bigger image (systemd + nginx-light) taking marginally longer to
instantiate even warm.

### Caveat

Disk is genuinely the limiter here, at a level the box's real 25GB
`praxis-sbx` storage volume can be a target for widening if more
concurrent CPT-01 capacity is ever needed — this is a real, fixable
capacity lever (bigger volume), unlike SJN-01's podman-internals wall.

---

## Phase D close-out: final recommendation (2026-09-05/06)

Two real tickets, two different real binding constraints, on real
hardware (Intel i5-7500, 4 cores, 14GB RAM, Linux 7.0.0-31-generic):

| Ticket | Real ceiling | Bound by |
|---|---|---|
| SJN-01 | ~60 | podman/containers-storage userns/idmap internals (host resources healthy) |
| CPT-01 | ~50 | host disk (`praxis-sbx`'s 25GB volume, real 20% floor crossed) |

Per this project's own stated benchmarking principle
(`docs/observability.md` §4: *"Benchmark against the worst-case mix, not
the average"*) — size against the heavier real ticket, CPT-01, not the
lighter one. **`PRAXIS_CAPACITY_WEIGHT=35`** (70% of CPT-01's 50, the
script's own stated rule), replacing the earlier placeholder progression
(2 → 11 → this). Since SJN-01's real ceiling (60) sits comfortably above
this value, staying under 35 keeps both tickets within their own real
limits automatically.

This number will move if either becomes true:
- The `praxis-sbx` storage volume is resized — CPT-01's ceiling is a real,
  fixable disk constraint, not an architectural one.
- A future ticket is heavier than CPT-01 on either axis — re-benchmark
  against it specifically, not against an average.
- The podman/containers-storage userns/idmap ceiling moves (version
  upgrade) — currently irrelevant to the binding number since CPT-01's
  disk limit (50) is already below it (60), but would matter if a disk
  upgrade ever pushed past ~60.

### Not addressed by this phase

The operator dashboard idea (`docs/session-03-plan.md`, deliberately
parked for a later, separate branch), SKN-01 benchmarking (never built —
its own fixed-content ticket profile is expected to be lighter than
either SJN-01 or CPT-01, not the worst case), and the bake pipeline
automation (`ROADMAP.md`, tracked separately).
