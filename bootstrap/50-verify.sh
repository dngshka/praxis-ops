#!/usr/bin/env bash
# 50-verify.sh -- prove the hardening actually holds. Run as root.
#
# This is the gate between "configured" and "trusted". It spawns a throwaway
# container as the sandbox user and asserts containment properties from the
# inside. A FAIL here means a ticket image would not be safely contained.
#
# Re-run after ANY change to podman policy, the firewall, or the runtime.
set -uo pipefail

SBX_USER="${SBX_USER:-praxis-sbx}"
SBX_UID="$(id -u "$SBX_USER" 2>/dev/null || echo 0)"
SBX_HOME="$(getent passwd "$SBX_USER" | cut -d: -f6)"
IMAGE="${PRAXIS_VERIFY_IMAGE:-}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HARDENING_CHECK="$REPO_ROOT/security/hardening-check.sh"

PASS=0; FAIL=0
ok()  { printf '  OK    %s\n' "$*"; PASS=$((PASS+1)); }
bad() { printf '  FAIL  %s\n' "$*"; FAIL=$((FAIL+1)); }

as_sbx() {
  # Subshell cd to / first: runuser inherits the caller's working directory, and
  # praxis-sbx cannot traverse into /home/praxis. Without this every podman call
  # fails at process setup with "cannot chdir", which looks like a podman fault
  # rather than a permissions one.
  ( cd / && runuser -u "$SBX_USER" -- env \
      XDG_RUNTIME_DIR="/run/user/$SBX_UID" \
      HOME="$SBX_HOME" \
      "$@" )
}

# Must run as root. Unprivileged, this script cannot read the sandbox user's
# 0700 config, cannot list nftables, and cannot runuser -- every one of those
# reports as a FAIL, which looks like broken hardening rather than a missing
# sudo. Refuse instead of lying.
if [[ "$(id -u)" -ne 0 ]]; then
  echo "ERROR: run as root (sudo $0) -- unprivileged runs produce false failures" >&2
  exit 2
fi

echo "=== praxis host verification ==="

# --- host-side --------------------------------------------------------------
echo "-- account and filesystem --"
[[ "$(stat -c %a "$SBX_HOME")" == "700" ]] \
  && ok "home is 0700 (image store unreadable by other users)" \
  || bad "home is $(stat -c %a "$SBX_HOME"), expected 700"

[[ "$(getent passwd "$SBX_USER" | cut -d: -f7)" == "/usr/sbin/nologin" ]] \
  && ok "no login shell" || bad "login shell is set"

[[ "$(stat -c %a "/run/user/$SBX_UID" 2>/dev/null)" == "700" ]] \
  && ok "XDG_RUNTIME_DIR is 0700 (podman socket protected)" \
  || bad "XDG_RUNTIME_DIR missing or not 0700"

# The single most important negative check: can the portal user reach the socket?
if id praxis-portal >/dev/null 2>&1; then
  if runuser -u praxis-portal -- test -r "/run/user/$SBX_UID/podman/podman.sock" 2>/dev/null; then
    bad "portal user CAN read the podman socket -- boundary broken"
  else
    ok "portal user cannot reach the podman socket"
  fi
else
  echo "  note  praxis-portal does not exist yet; re-run this check after creating it"
fi

# The direction that matters most. praxis-sbx reading the operator's home would
# expose the grading tree -- the answer keys -- to a compromised orchestrator.
# Blocked by /home/praxis being 0750, not by anything under it, so a file-mode
# mistake deeper in the tree is survivable while this holds.
echo "-- reverse isolation (sandbox user -> operator files) --"
for opdir in /home/*; do
  opuser="$(basename "$opdir")"
  [[ "$opuser" == "$SBX_USER" ]] && continue
  [[ -d "$opdir" ]] || continue
  if ( cd / && runuser -u "$SBX_USER" -- ls "$opdir" ) >/dev/null 2>&1; then
    bad "$SBX_USER can list $opdir"
  else
    ok "$SBX_USER cannot read $opdir"
  fi
done
( cd / && runuser -u "$SBX_USER" -- cat /etc/shadow ) >/dev/null 2>&1 \
  && bad "$SBX_USER can read /etc/shadow" || ok "$SBX_USER cannot read /etc/shadow"

echo "-- policy files --"
for f in containers.conf storage.conf registries.conf policy.json; do
  [[ -f "$SBX_HOME/.config/containers/$f" ]] && ok "present: $f" || bad "missing: $f"
done
python3 - "$SBX_HOME/.config/containers/policy.json" <<'PYCHK' && ok "signature policy defaults to reject, no registry pulls" || bad "signature policy wrong"
import json, sys
p = json.load(open(sys.argv[1]))
assert p["default"] == [{"type": "reject"}], "default is not reject"
# The transport that pulls from a registry must be absent.
assert "docker" not in p.get("transports", {}), "docker transport is permitted"
PYCHK

echo "-- firewall --"
nft list table inet praxis >/dev/null 2>&1 \
  && ok "nftables table inet praxis loaded" || bad "nftables table inet praxis missing"
# nft may render the uid numerically or resolve it to a username depending on
# version and whether output is a tty -- accept either.
nft list table inet praxis 2>/dev/null | grep -qE "meta skuid != ($SBX_UID|$SBX_USER)\b" \
  && ok "egress rules scoped to uid $SBX_UID" || bad "uid scoping rule not found"

echo "-- podman --"
as_sbx podman info >/dev/null 2>&1 && ok "podman usable as $SBX_USER" || bad "podman not usable as $SBX_USER"
rootless="$(as_sbx podman info --format '{{.Host.Security.Rootless}}' 2>/dev/null)"
[[ "$rootless" == "true" ]] && ok "running rootless" || bad "NOT rootless (got '$rootless')"
cgv="$(as_sbx podman info --format '{{.Host.CgroupsVersion}}' 2>/dev/null)"
[[ "$cgv" == "v2" ]] && ok "cgroups $cgv" || bad "cgroups $cgv, need v2"
# Without fuse-overlayfs every sandbox slice copies its whole image
# (30-podman-policy.sh, storage.conf). Also catches a reset that dropped
# the user's storage.conf.
as_sbx podman info --format '{{json .Store.GraphOptions}}' 2>/dev/null | grep -q 'fuse-overlayfs' \
  && ok "overlay mounts through fuse-overlayfs (no per-slice image copies)" \
  || bad "overlay does not use fuse-overlayfs -- every sandbox slice will copy its image"

# --- container-side ----------------------------------------------------------
if [[ -z "$IMAGE" ]]; then
  echo
  echo "  Set PRAXIS_VERIFY_IMAGE=<digest-pinned image in your registry> to run the"
  echo "  in-container containment checks. Skipping those for now."
else
  echo "-- containment (live container) --"
  NAME="praxis-verify-$$"
  as_sbx podman run -d --rm --name "$NAME" \
    --userns auto \
    --network none --memory 256m --pids-limit 64 \
    --cap-drop ALL --security-opt no-new-privileges \
    "$IMAGE" sleep 120 >/dev/null 2>&1 \
    && ok "spawned verification container" || bad "could not spawn verification container"

  cexec() { as_sbx podman exec "$NAME" sh -c "$1" >/dev/null 2>&1; }

  # Container root must map to a SUBORDINATE host uid -- not to root, and not to
  # praxis-sbx either. Reading /proc/self/uid_map from inside is not sufficient:
  # under nested rootless userns those numbers are relative to the outer
  # namespace, so a container mapped onto praxis-sbx looks identical to one
  # mapped into the subuid range. Ask the HOST what uid the process runs as.
  cpid="$(as_sbx podman inspect --format '{{.State.Pid}}' "$NAME" 2>/dev/null)"
  hostuid="$(ps -o uid= -p "${cpid:-0}" 2>/dev/null | tr -d ' ')"
  if [[ -z "$hostuid" ]]; then
    bad "could not resolve host uid of container process"
  elif [[ "$hostuid" == "0" ]]; then
    bad "container process runs as HOST ROOT"
  elif [[ "$hostuid" == "$SBX_UID" ]]; then
    bad "container root maps to $SBX_USER ($SBX_UID) -- an escape owns the socket and image store; set userns=auto"
  else
    ok "container root maps to subordinate host uid $hostuid"
  fi

  cexec 'test -S /var/run/docker.sock' && bad "docker socket visible in container" || ok "no docker socket"
  cexec 'ls /run/user/*/podman/podman.sock' && bad "podman socket visible in container" || ok "no podman socket"
  # podman masks /proc/kcore by bind-mounting /dev/null over it, so `cat` exits 0
  # and a naive success/failure check reports it as readable. Measure bytes.
  kb="$(as_sbx podman exec "$NAME" sh -c 'head -c 64 /proc/kcore 2>/dev/null | wc -c' 2>/dev/null | tr -d ' ')"
  [[ "$kb" == "0" ]] && ok "/proc/kcore masked (0 bytes)" || bad "/proc/kcore returned ${kb:-?} bytes"
  cexec 'ls /dev/sd* /dev/nvme* /dev/vd*' && bad "block devices exposed" || ok "no block devices"
  cexec 'mkdir -p /tmp/m && mount -t proc proc /tmp/m' && bad "mount succeeded" || ok "mount blocked"
  cexec 'getent hosts github.com' && bad "DNS resolves external names" || ok "no external DNS"
  cexec 'timeout 4 sh -c ": </dev/tcp/1.1.1.1/443"' && bad "external egress works" || ok "no external egress"

  mm="$(as_sbx podman exec "$NAME" cat /sys/fs/cgroup/memory.max 2>/dev/null)"
  [[ "$mm" != "max" && -n "$mm" ]] && ok "memory ceiling enforced ($mm)" || bad "no memory ceiling"
  pm="$(as_sbx podman exec "$NAME" cat /sys/fs/cgroup/pids.max 2>/dev/null)"
  [[ "$pm" != "max" && -n "$pm" ]] && ok "pids ceiling enforced ($pm)" || bad "no pids ceiling"

  # Outside-asserted vs inside-observed containment are kept deliberately
  # separate evidence (docs/session-02-plan.md Phase B) -- this re-runs the
  # sandbox's own hardening validation from inside the container it is meant
  # to protect, on top of everything asserted from outside above.
  #
  # Piped over exec's stdin, not staged with `podman cp`: cp's copier resolves
  # ownership through the --userns auto mapping table to chown the copied file
  # into the container, and that failed for real ("container ID 65534 cannot
  # be mapped to a host ID") -- the auto-generated subordinate range doesn't
  # necessarily cover every id a copy-in wants to reason about. Piping into
  # `bash -s` never creates a file inside the container at all, so there is
  # nothing to map, and nothing left behind afterward either. This also
  # sidesteps the /home/praxis vs /root vs /tmp readability maze entirely --
  # root (this script) reads $HARDENING_CHECK directly; ownership under
  # /home/praxis was only ever a problem for praxis-sbx reading it itself.
  #
  # Must run before the load-spawner test below, not after: that test
  # deliberately saturates the container's pids cgroup, and once it's full no
  # further `podman exec` into this container can fork -- including this one.
  echo "-- hardening-check.sh (inside view) --"
  if [[ -f "$HARDENING_CHECK" ]]; then
    # bash explicitly: hardening-check.sh uses arrays and [[ ]], and /bin/sh
    # on Ubuntu is dash, not bash.
    hc_out="$(as_sbx podman exec -i "$NAME" bash -s < "$HARDENING_CHECK" 2>&1)"
    hc_rc=$?
    echo "$hc_out" | sed 's/^/  /'
    [[ $hc_rc -eq 0 ]] \
      && ok "hardening-check.sh: all containment properties hold from inside" \
      || bad "hardening-check.sh: reported a FAIL from inside (see output above)"
  else
    bad "hardening-check.sh not found at $HARDENING_CHECK"
  fi

  # pids.max being set proves a ceiling exists, not that it actually engages
  # under load. Deliberately NOT a literal fork bomb: `:(){ :|:& };:` is bash
  # function syntax, and the container's /bin/sh is dash -- it errors out on
  # "Bad function name" before spawning anything, which reads as a passing
  # ceiling for the wrong reason. A deterministic spawner (POSIX `for`, no
  # bash-isms) is bounded and reports a real number, capped at 300 requests
  # against a ceiling of 64.
  #
  # This is deliberately the LAST containment check: fully saturating the pids
  # cgroup means the container can no longer fork anything -- including the
  # helper process a subsequent `podman exec` itself needs, which is exactly
  # what broke the first attempt at reading pids.current back via exec
  # ("crun: fork: Resource temporarily unavailable"). Read it from the HOST
  # side instead: $cpid (resolved above, for the userns check) names the
  # container's process on the host, and root can follow /proc/$cpid/cgroup to
  # the same pids.current the container sees, without forking anything inside
  # the namespace that's now full. `podman rm -f` below doesn't need to fork
  # into the container either -- it tears the cgroup and namespace down from
  # the host -- so a saturated container is still safe to clean up.
  echo "-- load spawner vs pids ceiling --"
  as_sbx podman exec -d "$NAME" sh -c 'for i in $(seq 1 300); do sleep 30 & done' >/dev/null 2>&1
  sleep 3
  cg_rel="$(awk -F: '{print $3}' "/proc/$cpid/cgroup" 2>/dev/null)"
  pids_cur=""
  [[ -n "$cg_rel" ]] && pids_cur="$(cat "/sys/fs/cgroup${cg_rel}/pids.current" 2>/dev/null)"
  if [[ -n "$pids_cur" && -n "$pm" && "$pm" != "max" && "$pids_cur" -ge $(( pm - 2 )) ]]; then
    ok "pids ceiling engaged under load (300 requested, $pids_cur / $pm holding)"
  else
    bad "pids.current ($pids_cur) never reached the ceiling ($pm) -- ceiling may be advisory only"
  fi
  host_procs="$(ps -e --no-headers 2>/dev/null | wc -l)"
  printf '  note  host process count during load spawn: %s (spawner is capped inside the container cgroup, never touches the host pid table)\n' "$host_procs"

  as_sbx podman rm -f "$NAME" >/dev/null 2>&1
fi

echo
echo "pass=$PASS fail=$FAIL"
if [[ $FAIL -eq 0 ]]; then
  echo "host is ready -- safe to build ticket images and start the orchestrator"
else
  echo "DO NOT run untrusted images until the FAIL lines are resolved" >&2
fi
exit $(( FAIL > 0 ))
