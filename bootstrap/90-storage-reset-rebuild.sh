#!/usr/bin/env bash
# 90-storage-reset-rebuild.sh -- EMERGENCY FULL RESET of praxis-sbx's podman
# storage. Run as root.
#
# THIS IS NOT A MAINTENANCE TOOL. IT IS NOT A PRUNER. It does not surgically
# remove garbage -- it revokes everything (podman system reset: every image,
# every container, podman's entire local database) and reinitializes from
# source. There is no partial/periodic mode. Do not schedule this, do not
# run it casually, and do not treat "storage is a bit full" as reason enough
# on its own -- this is a last-resort recovery procedure for when disk is
# genuinely critical, run deliberately, once, with eyes on the output.
#
# A real fix -- either a periodic pruner that safely reclaims just the
# orphaned layers, or a targeted per-Destroy() cleanup in the orchestrator
# that stops the leak at the source -- is real, separate, deferred work.
# Tracked in ROADMAP.md as MVP2 scope, not built here. This script exists
# only because that work isn't done yet and disk had already hit a real
# wall once (see below).
#
# Why this exists at all (full incident writeup: docs/capacity-benchmark.md,
# "Known host constraint"): podman's rootless --userns=auto "ID-mapped copy
# of layer" mechanism, on this podman version, does not behave as a
# lightweight share -- every container spawn leaves behind a full,
# untracked physical copy of whatever base layer it used, and `podman rm`
# never reclaims it. Confirmed live: after every container was destroyed
# and `podman ps -a` came back empty, ~19GB of orphaned overlay directories
# remained on disk. This is a real, permanent leak from NORMAL orchestrator
# operation -- every candidate session leaks one of these, forever,
# regardless of TTL or concurrency. There is no config-level fix; it's an
# unresolved-upstream podman/containers-storage defect (containers/podman
# discussion #20139 hit the same error class with no fix).
#
# What NOT to use to reclaim that leak, learned the hard way: `podman save`,
# `podman system check`, and `podman system migrate` all independently
# triggered WORSE corruption when run against this host's storage --
# `system check` reported nearly the entire layer store as "damaged" (mtime
# drift, not byte corruption -- real content stayed sane, confirmed via
# direct `du`), and `save` silently resolved multiple distinct image names
# to the same wrong blob. All three appear to invoke the identical broken
# copy mechanism merely by reading/verifying a layer. `podman system reset`
# is the one operation validated here as safe and predictable: it wipes
# storage AND its own bookkeeping consistently, rather than us guessing at
# which files are safe to touch by hand -- which is exactly why this script
# is a full nuke, not a smarter partial one: we don't yet have a version of
# "smarter" that's actually been proven safe on this host.
#
# Update 2026-09-26 (docs/capacity-benchmark.md, "the copies are a cache"):
# measured on podman 5.7.0, the copies belong to an (image, uid slice) pair,
# are reused on that slice, and `podman rmi` of the image removes all of
# them. With fuse-overlayfs as the store's mount program
# (30-podman-policy.sh) no copy is made at all. This script is still how a
# store switches to fuse-overlayfs, and how one that already holds copies
# from before gets rid of them.
#
# Trigger: watch hostmon's praxis_storage_free_bytes (unauthenticated,
# :9102/metrics). This is a manual, rare, deliberate call, not a monitored
# threshold with an automatic response -- storage operations on this host
# have already produced two unpredictable surprises; a human stays fully in
# the loop for this one, every time.
set -euo pipefail

SBX_USER="${SBX_USER:-praxis-sbx}"
SBX_UID="$(id -u "$SBX_USER")"
SBX_HOME="$(getent passwd "$SBX_USER" | cut -d: -f6)"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STORAGE_PATH="${PRAXIS_STORAGE_PATH:-$SBX_HOME/.local/share/containers}"

PASS=0; FAIL=0
ok()  { printf '  OK    %s\n' "$*"; PASS=$((PASS+1)); }
bad() { printf '  FAIL  %s\n' "$*"; FAIL=$((FAIL+1)); }

as_sbx() {
  ( cd / && runuser -u "$SBX_USER" -- env \
      XDG_RUNTIME_DIR="/run/user/$SBX_UID" \
      HOME="$SBX_HOME" \
      "$@" )
}

if [[ "$(id -u)" -ne 0 ]]; then
  echo "ERROR: run as root (sudo $0)" >&2
  exit 2
fi

echo "=== EMERGENCY: full storage reset and rebuild ==="
echo

# --- 1. refuse to run against a live session ----------------------------
echo "-- checking for live sessions --"
running="$(as_sbx podman ps -q 2>/dev/null | wc -l)"
if [[ "$running" -ne 0 ]]; then
  echo "ERROR: $running container(s) currently running under $SBX_USER." >&2
  echo "This script wipes ALL local storage, running or not. Refusing to" >&2
  echo "proceed -- wait for real sessions to end, or coordinate a real" >&2
  echo "maintenance window, before running this." >&2
  exit 1
fi
echo "  OK: no running containers"
echo

echo "-- disk usage before --"
df -h "$STORAGE_PATH" | sed 's/^/  /'
before_used="$(df --output=used "$STORAGE_PATH" | tail -1 | tr -d ' ')"
echo

# --- 2. stop the orchestrator so nothing spawns mid-reset ----------------
echo "-- stopping praxis-orchestrator --"
as_sbx systemctl --user stop praxis-orchestrator 2>/dev/null \
  && echo "  stopped" \
  || echo "  WARN: stop failed or unit not running -- continuing, but double-check nothing can spawn"

# Re-check after stop: a spawn already in flight when we checked above could
# land here. Belt and suspenders, not redundant -- Destroy()'s own race with
# hostmon's poll interval is a documented, real timing issue on this project
# (docs/capacity-benchmark.md / ROADMAP.md), so a settle pause before the
# second check is deliberate, not decorative.
sleep 5
running="$(as_sbx podman ps -q 2>/dev/null | wc -l)"
if [[ "$running" -ne 0 ]]; then
  echo "ERROR: a container appeared after stopping the orchestrator. Aborting" >&2
  echo "before touching storage -- investigate, do not re-run blindly." >&2
  exit 1
fi
echo

# --- 3. the actual reset --------------------------------------------------
echo "-- podman system reset --force --"
as_sbx podman system reset --force
echo

echo "-- disk usage after reset --"
df -h "$STORAGE_PATH" | sed 's/^/  /'
echo

# --- 4. rebuild the two base images ---------------------------------------
echo "-- rebuilding praxis/ops-base --"
PRAXIS_REBUILD=1 "$REPO_ROOT/bootstrap/60-build-base.sh"
echo

echo "-- rebuilding praxis/ops-systemd --"
PRAXIS_REBUILD=1 "$REPO_ROOT/bootstrap/61-build-systemd-base.sh"
echo

# --- 5. re-verify containment on both, same bar as first build -----------
echo "-- re-verifying containment: praxis/ops-base --"
PRAXIS_VERIFY_IMAGE=praxis/ops-base "$REPO_ROOT/bootstrap/50-verify.sh" \
  && ok "50-verify.sh clean against ops-base" \
  || bad "50-verify.sh reported a FAIL against ops-base -- do not build tickets on it"

echo "-- re-verifying containment: praxis/ops-systemd --"
PRAXIS_VERIFY_IMAGE=praxis/ops-systemd "$REPO_ROOT/bootstrap/50-verify.sh" \
  && ok "50-verify.sh clean against ops-systemd" \
  || bad "50-verify.sh reported a FAIL against ops-systemd -- do not build tickets on it"

echo "-- re-verifying shell-access isolation --"
"$REPO_ROOT/security/verify-shell-isolation.sh" \
  && ok "verify-shell-isolation.sh clean" \
  || bad "verify-shell-isolation.sh reported a FAIL"
echo

if [[ "$FAIL" -gt 0 ]]; then
  echo "ERROR: containment did not re-verify clean. STOP -- do not build" >&2
  echo "ticket images on unverified bases. Investigate before proceeding." >&2
  echo "pass=$PASS fail=$FAIL"
  exit 1
fi

# --- 6. rebuild the two real ticket images --------------------------------
echo "-- rebuilding praxis/sjn-01 --"
tar -C "$REPO_ROOT/tickets/SJN-01" -c . | as_sbx podman build -t praxis/sjn-01 -
SJN01_ID="$(as_sbx podman inspect --format '{{.Id}}' praxis/sjn-01)"
echo "  new digest: sha256:$SJN01_ID"
echo

echo "-- rebuilding praxis/cpt-01 --"
tar -C "$REPO_ROOT/tickets/CPT-01" -c . | as_sbx podman build -t praxis/cpt-01 -
CPT01_ID="$(as_sbx podman inspect --format '{{.Id}}' praxis/cpt-01)"
echo "  new digest: sha256:$CPT01_ID"
echo

# --- 7. bring the orchestrator back up ------------------------------------
echo "-- restarting praxis-orchestrator --"
as_sbx systemctl --user start praxis-orchestrator \
  && ok "praxis-orchestrator restarted" \
  || bad "praxis-orchestrator failed to restart -- check: journalctl --user -u praxis-orchestrator"
echo

echo "-- disk usage after full rebuild --"
df -h "$STORAGE_PATH" | sed 's/^/  /'
after_used="$(df --output=used "$STORAGE_PATH" | tail -1 | tr -d ' ')"
reclaimed_kb=$(( before_used - after_used ))
echo "  reclaimed: $(( reclaimed_kb / 1024 ))MB (before - after used space)"
echo

echo "pass=$PASS fail=$FAIL"
echo
echo "New ticket image digests -- update tickets/SJN-01/scenario.yaml and"
echo "tickets/CPT-01/scenario.yaml's substrate_image fields if these differ"
echo "from what's currently recorded there:"
echo "  praxis/sjn-01@sha256:$SJN01_ID"
echo "  praxis/cpt-01@sha256:$CPT01_ID"
echo
echo "This script verifies CONTAINMENT and that builds succeed. It does NOT"
echo "re-run Stage-4-style behavioral verification (confirming SJN-01's"
echo "writer process actually runs, CPT-01's nginx is actually seeded"
echo "disabled, etc). Recommended manual follow-up: spawn one real instance"
echo "of each ticket through the orchestrator API and confirm behavior"
echo "before trusting these for anything real."
exit $(( FAIL > 0 ))
