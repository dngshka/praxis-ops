#!/usr/bin/env bash
# 30-podman-policy.sh -- runtime policy for the sandbox user. Run as root.
# Idempotent.
#
# These are the defaults every sandbox inherits WITHOUT the orchestrator asking.
# That matters: a bug in the orchestrator's pod spec should degrade to a locked
# container, not to a privileged one. Defence in depth means the runtime is
# already hostile before any per-attempt config is applied.
set -euo pipefail

SBX_USER="${SBX_USER:-praxis-sbx}"
SBX_UID="$(id -u "$SBX_USER")"
SBX_HOME="$(getent passwd "$SBX_USER" | cut -d: -f6)"
CONF="$SBX_HOME/.config/containers"

echo "=== podman policy for ${SBX_USER} ==="
install -d -o "$SBX_USER" -g "$SBX_USER" -m 0700 "$CONF"

# --- containers.conf ---------------------------------------------------------
cat > "$CONF/containers.conf" <<EOF
# Praxis sandbox defaults. Every container starts from here.

[containers]
# Empty default set: a container gets NO capabilities unless the orchestrator
# explicitly adds them. The systemd tier adds a narrow set; nothing else does.
default_capabilities = []

# Map container root to a SUBORDINATE uid, not to praxis-sbx itself. Rootless
# podman's default maps container 0 -> the podman user (uid 1001), so an escape
# would land as the user that owns the socket and the image store. With auto,
# container 0 lands in the 165536-231071 range, which owns nothing -- and each
# container gets a distinct slice, isolating attempts from each other.
#
# Default slice is 1024 uids: enough for root plus the candidate user at 1000.
# 65536 subuids / 1024 = 64 concurrent ranges, far above this box's capacity.
userns = "auto"

# No network unless asked. The orchestrator overrides this per attempt, and
# "none" is the correct default for every ticket in the POC.
netns = "none"

# Do not leak the host's name resolution or hosts file into sandboxes.
no_hosts = true
dns_searches = []

# Ceilings that apply even if the orchestrator forgets to set them.
pids_limit = 256
log_size_max = 10485760

# umask inside containers; keeps candidate-created files off world-readable.
umask = "0027"

# No host devices, ever.
devices = []

[engine]
runtime = "crun"
cgroup_manager = "systemd"
events_logger = "file"
# Never fall back to a remote socket we did not configure.
remote = false

[network]
network_backend = "netavark"
default_subnet = "10.89.240.0/24"
EOF

# --- storage.conf ------------------------------------------------------------
cat > "$CONF/storage.conf" <<EOF
[storage]
driver = "overlay"
graphroot = "$SBX_HOME/.local/share/containers/storage"
runroot = "/run/user/$SBX_UID/containers"

[storage.options]
# No shared stores. The sandbox user's images are its own; nothing else on the
# box can read them, and it can read nothing else.
additionalimagestores = []

[storage.options.overlay]
# nodev: a sandbox layer can never carry a usable device node.
mountopt = "nodev"
# fuse-overlayfs maps uids itself. With the kernel overlay driver, rootless
# podman gives every userns=auto slice its own chowned copy of the whole
# image, kept until the image is removed: ~115k inodes, ~1.4 GB and ~150 s
# per Medusa sandbox, which capped the praxis UET demo at 5-6 sandboxes on
# this store. Measured 2026-09-26 (docs/capacity-benchmark.md): with this
# line a sandbox spawns in under a second and the store does not grow; file
# reads inside the sandbox are 2-3x slower.
# Switching an existing store needs no reset (measured 2026-09-27): the
# copies already made stay until their images are removed, so rmi and load
# each image again to reclaim them; the image IDs do not change. Switching
# back does need 90-storage-reset-rebuild.sh: the store records
# overlay/.has-mount-program and keeps using fuse-overlayfs without this line.
mount_program = "/usr/bin/fuse-overlayfs"
EOF

# --- registries.conf ---------------------------------------------------------
# No registry on this host. An empty unqualified-search list means a bare image
# name ("ubuntu") fails immediately instead of silently reaching for docker.io.
# The explicit blocks are belt and braces for a fully-qualified reference.
cat > "$CONF/registries.conf" <<EOF
unqualified-search-registries = []

[[registry]]
location = "docker.io"
blocked = true

[[registry]]
location = "quay.io"
blocked = true

[[registry]]
location = "registry.k8s.io"
blocked = true

[[registry]]
location = "ghcr.io"
blocked = true
EOF

# --- policy.json -------------------------------------------------------------
# Default reject. The `docker` transport -- the one that pulls from a registry --
# is NOT listed, so this host can pull from nowhere. That is the guarantee.
#
# The four permitted transports are all local-file operations: an operator must
# put the bytes on disk first. `tarball` is required by `podman import` (used by
# 60-build-base.sh); `docker-archive` and `oci-archive` by `podman load`. Omitting
# tarball makes the base image build fail with "rejected by policy", which reads
# like a broken build rather than a policy decision.
cat > "$CONF/policy.json" <<'EOF'
{
  "default": [{"type": "reject"}],
  "transports": {
    "containers-storage": { "": [{"type": "insecureAcceptAnything"}] },
    "tarball":            { "": [{"type": "insecureAcceptAnything"}] },
    "docker-archive":     { "": [{"type": "insecureAcceptAnything"}] },
    "oci-archive":        { "": [{"type": "insecureAcceptAnything"}] }
  }
}
EOF

# --- seccomp -----------------------------------------------------------------
# Start from the shipped default and remove what a troubleshooting sandbox has no
# business calling. Keep this list short and justified: over-blocking produces
# tickets that fail for reasons the candidate cannot diagnose, which is worse
# than a slightly wider syscall surface.
SECCOMP="$CONF/seccomp-praxis.json"
if [[ -f /usr/share/containers/seccomp.json ]]; then
  python3 - "$SECCOMP" <<'PY'
import json, sys
base = json.load(open("/usr/share/containers/seccomp.json"))
# Explicit deny list layered on top of the default profile.
deny = [
    "kexec_load", "kexec_file_load",      # no kernel replacement
    "init_module", "finit_module", "delete_module",  # no modules
    "bpf",                                 # no eBPF from a sandbox
    "perf_event_open",                     # no host-wide perf
    "userfaultfd",                         # common escape primitive
    "process_vm_readv", "process_vm_writev",
]
base.setdefault("syscalls", []).insert(0, {
    "names": deny,
    "action": "SCMP_ACT_ERRNO",
    "errnoRet": 1,
})
json.dump(base, open(sys.argv[1], "w"), indent=1)
print(f"  seccomp profile written with {len(deny)} extra denied syscalls")
PY
  chown "$SBX_USER:$SBX_USER" "$SECCOMP"
  chmod 0400 "$SECCOMP"
  # Wire it in as the default for this user.
  sed -i "s|^\[containers\]|[containers]\nseccomp_profile = \"$SECCOMP\"|" "$CONF/containers.conf"
else
  echo "  WARN: /usr/share/containers/seccomp.json missing -- using podman's built-in default"
fi

chown -R "$SBX_USER:$SBX_USER" "$CONF"
chmod 0600 "$CONF"/*.conf "$CONF"/policy.json 2>/dev/null || true

echo "  policy written to $CONF"
ls -la "$CONF" | sed 's/^/  /'

# --- enable the socket -------------------------------------------------------
runuser -u "$SBX_USER" -- env XDG_RUNTIME_DIR="/run/user/$SBX_UID" \
  systemctl --user enable --now podman.socket 2>/dev/null \
  && echo "  podman.socket enabled" \
  || echo "  WARN: could not enable podman.socket -- check lingering, then retry"

echo
echo "done -- continue with 40-network-guard.sh"
