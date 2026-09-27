package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

// ContainerBackend speaks the Docker API. Point DOCKER_HOST at whichever
// rootless socket you settled on:
//
//	unix:///run/user/$(id -u)/podman/podman.sock   (rootless podman)
//	unix:///run/user/$(id -u)/docker.sock          (rootless docker)
//
// Rootless is not optional. It is what makes root_in_sandbox safe: root inside
// the container maps to a subordinate UID on the host, so an escape lands as a
// user that owns nothing rather than as root on the box hosting GitLab.
type ContainerBackend struct {
	cli *client.Client

	// sharedDir is PRAXIS_SHARED_DIR with symlinks resolved, or "" when the
	// host serves nothing from it (see Runbook.SharedFromImage).
	sharedDir string
}

var _ Backend = (*ContainerBackend)(nil)

// SetSharedDir enables Runbook.SharedFromImage: dir holds one directory per
// image ID, each a copy of that image's paths, written by the operator's
// image loader and readable by the sandbox user. Everything under it is
// mounted read-only, so it must only ever hold image content.
func (b *ContainerBackend) SetSharedDir(dir string) error {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("shared dir: %w", err)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return fmt.Errorf("shared dir: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("shared dir %s is not a directory", real)
	}
	b.sharedDir = real
	return nil
}

// sharedMounts turns rb.SharedFromImage into read-only bind mounts of
// <sharedDir>/<image id><path>. A path the host has no copy of is left to the
// image: the mount saves time, it never changes what the sandbox contains. A
// copy reached through a symlink is refused, so nothing outside sharedDir can
// be mounted into a sandbox.
func (b *ContainerBackend) sharedMounts(rb Runbook) []mount.Mount {
	if b.sharedDir == "" || len(rb.SharedFromImage) == 0 {
		return nil
	}
	id := localImageRef(rb.Image)
	if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		return nil
	}
	var out []mount.Mount
	for _, p := range rb.SharedFromImage {
		src := filepath.Join(b.sharedDir, id, filepath.FromSlash(p))
		real, err := filepath.EvalSymlinks(src)
		if err != nil || real != src {
			continue
		}
		out = append(out, mount.Mount{Type: mount.TypeBind, Source: src, Target: p, ReadOnly: true})
	}
	return out
}

func NewContainerBackend() (*ContainerBackend, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &ContainerBackend{cli: cli}, nil
}

func (b *ContainerBackend) Close() error { return b.cli.Close() }

// RawClient exposes the underlying docker client for internal/metrics's
// CollectManaged/CollectAll (which need a raw ContainerList, something the
// Backend interface deliberately does not expose -- a future
// KubernetesBackend has no reason to grow a docker-shaped method just to
// satisfy a metrics package it may not even use the same way). Returning the
// concrete *client.Client here, not a metrics-package type, is what keeps
// this package from having to import internal/metrics at all: metrics
// already imports sandbox (for its label constants), and the reverse would
// be an import cycle.
func (b *ContainerBackend) RawClient() *client.Client { return b.cli }

func (b *ContainerBackend) Create(ctx context.Context, attemptID string, rb Runbook) (Instance, error) {
	if err := rb.Validate(); err != nil {
		return Instance{}, err
	}
	name, err := SandboxName(attemptID)
	if err != nil {
		return Instance{}, err
	}

	now := time.Now().UTC()
	expires := now.Add(rb.TTL())
	cfg, hostCfg := b.spec(rb, attemptID, now, expires)
	if mounts := b.sharedMounts(rb); len(mounts) > 0 {
		hostCfg.Mounts = mounts
		targets := make([]string, 0, len(mounts))
		for _, m := range mounts {
			targets = append(targets, m.Target)
		}
		cfg.Labels[LabelShared] = strings.Join(targets, ",")
	}

	created, err := b.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
	if err != nil {
		// A typed conflict check, not a string match on the error text. This is
		// the contract the whole idempotency story rests on: a repeat Create
		// must be distinguishable from a real failure.
		if errdefs.IsConflict(err) {
			return b.Get(ctx, attemptID)
		}
		return Instance{}, fmt.Errorf("create %s: %w", name, err)
	}

	if err := b.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		// Do not leak a created-but-dead container. Best effort: the reaper
		// would eventually take it on TTL, but that is minutes of a scarce slot.
		_ = b.cli.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
		return Instance{}, fmt.Errorf("start %s: %w", name, err)
	}
	return b.Get(ctx, attemptID)
}

func (b *ContainerBackend) Get(ctx context.Context, attemptID string) (Instance, error) {
	name, err := SandboxName(attemptID)
	if err != nil {
		return Instance{}, err
	}
	insp, err := b.cli.ContainerInspect(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return Instance{AttemptID: attemptID, Name: name, Status: StatusAbsent}, ErrNotFound
		}
		return Instance{}, fmt.Errorf("inspect %s: %w", name, err)
	}
	return instanceFrom(insp.Config.Labels, insp.State.Status, insp.ID, attemptID, name), nil
}

func (b *ContainerBackend) Destroy(ctx context.Context, attemptID string) (bool, error) {
	name, err := SandboxName(attemptID)
	if err != nil {
		return false, err
	}
	err = b.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("remove %s: %w", name, err)
	}
	return true, nil
}

// Reap is the backstop for the entire design. If the portal loses an attempt
// record, nothing else will ever destroy the container. Every failure path here
// fails closed -- an unparseable expiry is treated as expired.
func (b *ContainerBackend) Reap(ctx context.Context) ([]string, error) {
	managed, err := b.managed(ctx, true)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var killed []string
	for _, c := range managed {
		raw, ok := c.Labels[LabelExpires]
		if !ok {
			continue
		}
		expires, perr := time.Parse(time.RFC3339, raw)
		if perr != nil {
			expires = now // unparseable expiry: fail closed
		}
		if expires.After(now) {
			continue
		}
		id := c.Labels[LabelAttempt]
		if rmErr := b.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); rmErr != nil {
			if !errdefs.IsNotFound(rmErr) {
				continue
			}
		}
		killed = append(killed, id)
	}
	return killed, nil
}

func (b *ContainerBackend) CountRunning(ctx context.Context) (int, error) {
	running, err := b.managed(ctx, false)
	if err != nil {
		return 0, err
	}
	return len(running), nil
}

func (b *ContainerBackend) PutFile(ctx context.Context, attemptID, dest string, content []byte, mode int64) error {
	name, err := SandboxName(attemptID)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Name: path.Base(dest),
		Mode: mode,
		Size: int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar header: %w", err)
	}
	if _, err := tw.Write(content); err != nil {
		return fmt.Errorf("tar write: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("tar close: %w", err)
	}
	dir := path.Dir(dest)
	if dir == "" {
		dir = "/"
	}
	return b.cli.CopyToContainer(ctx, name, dir, &buf, types.CopyToContainerOptions{})
}

func (b *ContainerBackend) ExecScript(ctx context.Context, attemptID string, script []byte, timeout time.Duration) (ExecResult, error) {
	name, err := SandboxName(attemptID)
	if err != nil {
		return ExecResult{}, err
	}
	const target = "/opt/praxis/run.sh"
	if err := b.PutFile(ctx, attemptID, target, script, 0o500); err != nil {
		return ExecResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	created, err := b.cli.ContainerExecCreate(ctx, name, types.ExecConfig{
		Cmd:          []string{"/bin/sh", target},
		User:         "root",
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec create: %w", err)
	}

	started := time.Now()
	attach, err := b.cli.ContainerExecAttach(ctx, created.ID, types.ExecStartCheck{})
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec attach: %w", err)
	}
	defer attach.Close()

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attach.Reader); err != nil && err != io.EOF {
		return ExecResult{}, fmt.Errorf("exec read: %w", err)
	}

	insp, err := b.cli.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec inspect: %w", err)
	}

	return ExecResult{
		ExitCode: insp.ExitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(started).Seconds(),
	}, nil
}

// ExecShell attaches an interactive PTY, unlike ExecScript's buffered batch
// run. With Tty:true the runtime does not multiplex stdout/stderr into the
// stdcopy frame format ExecScript reads with -- it hands back raw terminal
// bytes -- so this returns them directly rather than routing through
// stdcopy.StdCopy, which would corrupt them.
func (b *ContainerBackend) ExecShell(ctx context.Context, attemptID, user string) (io.ReadWriteCloser, error) {
	name, err := SandboxName(attemptID)
	if err != nil {
		return nil, err
	}
	if user == "" {
		user = "candidate"
	}

	created, err := b.cli.ContainerExecCreate(ctx, name, types.ExecConfig{
		Cmd:          []string{"/bin/bash", "-l"},
		User:         user,
		Tty:          true,
		AttachStdin:  true,
		AttachStdout: true,
	})
	if err != nil {
		return nil, fmt.Errorf("shell exec create: %w", err)
	}

	attach, err := b.cli.ContainerExecAttach(ctx, created.ID, types.ExecStartCheck{Tty: true})
	if err != nil {
		return nil, fmt.Errorf("shell exec attach: %w", err)
	}
	return &hijackedExec{attach: attach}, nil
}

// hijackedExec adapts a docker exec attachment into a plain
// io.ReadWriteCloser. Reads go through attach.Reader, not attach.Conn
// directly -- the client library may already have buffered bytes the server
// sent ahead of the handoff, and reading the raw Conn instead would drop them.
type hijackedExec struct {
	attach types.HijackedResponse
}

func (h *hijackedExec) Read(p []byte) (int, error)  { return h.attach.Reader.Read(p) }
func (h *hijackedExec) Write(p []byte) (int, error) { return h.attach.Conn.Write(p) }
func (h *hijackedExec) Close() error                { h.attach.Close(); return nil }

// ---- internals ----

func (b *ContainerBackend) managed(ctx context.Context, all bool) ([]types.Container, error) {
	f := filters.NewArgs()
	f.Add("label", LabelManaged+"="+ManagedValue)
	list, err := b.cli.ContainerList(ctx, container.ListOptions{All: all, Filters: f})
	if err != nil {
		return nil, fmt.Errorf("list managed: %w", err)
	}
	return list, nil
}

// localImageRef reduces a digest-pinned Runbook.Image ("repo@sha256:<hex>")
// to the bare content hash for the actual ContainerCreate call. Empirically
// verified against the real host (no code-only assumption): with no
// registries configured, podman's short-name resolver refuses
// "repo@sha256:<hex>" outright -- "did not resolve to an alias and no
// unqualified-search registries are defined" -- treating the missing
// registry hostname as something that needs registry resolution rather than
// a local lookup, even though the image is already in local storage. A bare
// image ID has no name component to trigger that resolution path at all and
// runs cleanly. Validate() still requires the full "repo@sha256:<hex>" form
// as the source of truth for the pinning invariant; only what's handed to
// the runtime changes.
//
// This is the local-only-MVP answer specifically. If a registry is ever
// added, revisit this: a fully qualified reference from a real registry
// should very likely be passed through as-is (so policy/signature checks
// tied to that reference apply), not reduced to a bare ID that only means
// "whichever local image happens to have this content hash," with no
// provenance attached.
func localImageRef(image string) string {
	const marker = "@sha256:"
	if i := strings.LastIndex(image, marker); i >= 0 {
		return image[i+len(marker):]
	}
	return image
}

func (b *ContainerBackend) spec(rb Runbook, attemptID string, spawnedAt, expires time.Time) (*container.Config, *container.HostConfig) {
	labels := map[string]string{
		LabelManaged:   ManagedValue,
		LabelAttempt:   attemptID,
		LabelRunbook:   rb.Digest(),
		LabelExpires:   expires.Format(time.RFC3339),
		LabelSpawnedAt: spawnedAt.Format(time.RFC3339),
		LabelWeight:    strconv.Itoa(rb.EffectiveWeight()),
		LabelDiskLimit: strconv.FormatInt(rb.EffectiveDiskLimitBytes(), 10),
	}

	cfg := &container.Config{
		Image:        localImageRef(rb.Image),
		Labels:       labels,
		WorkingDir:   rb.Workdir,
		Tty:          false,
		ExposedPorts: nat.PortSet{},
	}

	tmpfs := map[string]string{}
	for k, v := range rb.Tmpfs {
		tmpfs[k] = v
	}

	hostCfg := &container.HostConfig{
		AutoRemove:  false,
		NetworkMode: container.NetworkMode(networkMode(rb, attemptID)),
		Tmpfs:       tmpfs,
		Resources: container.Resources{
			Memory:    parseMemory(rb.Memory),
			NanoCPUs:  int64(rb.CPUs * 1e9),
			PidsLimit: &rb.PidsLimit,
			// CgroupParent lives on Resources, not HostConfig directly --
			// easy to miss, the compiler will reject it at the top level of
			// the HostConfig literal with "unknown field" if it's ever moved.
			//
			// Requires deploy/praxis-sbx.slice installed and started on the
			// host -- see SandboxSlice's own comment. What happens here if
			// that unit is NOT installed is genuinely unverified: systemd's
			// transient-unit API often auto-vivifies an intermediate slice
			// path on demand, in which case this degrades to "hostmon has
			// nothing pre-existing to read yet" rather than a spawn failure
			// -- but that is an assumption, not something confirmed against
			// this host's actual systemd/podman versions. Deploy
			// deploy/praxis-sbx.slice BEFORE this ships, and confirm a spawn
			// still succeeds if it's ever missing, rather than trusting this
			// comment.
			CgroupParent: SandboxSlice,
		},
		SecurityOpt: []string{"no-new-privileges"},
		CapDrop:     []string{"ALL"},
		// Confirmed live and dangerous, not theoretical: without this, a
		// container spawned through this exact code path maps its root
		// process to host uid 1001 -- praxis-sbx itself, the account that
		// owns the podman socket, the image store, and this orchestrator
		// process. An escape would own everything, not nothing. Every
		// verification this session (50-verify.sh, preflight-ticket.sh,
		// verify-shell-isolation.sh) spawns with --userns auto explicitly;
		// this was the one path that never did, because it never went
		// through any of those scripts' spawn flags at all -- it has its own.
		UsernsMode: "auto",
	}

	if rb.Systemd {
		// systemd as PID 1 needs cgroup delegation and a writable /run. Under a
		// rootless runtime this stays unprivileged -- do not reach for
		// --privileged, which would void the entire containment model.
		hostCfg.CapAdd = []string{"SETUID", "SETGID", "CHOWN", "KILL", "DAC_OVERRIDE", "SYS_ADMIN"}
		hostCfg.CgroupnsMode = container.CgroupnsModePrivate
		hostCfg.ReadonlyRootfs = false
		if _, ok := tmpfs["/run"]; !ok {
			tmpfs["/run"] = "size=64m"
		}
		if _, ok := tmpfs["/run/lock"]; !ok {
			tmpfs["/run/lock"] = "size=8m"
		}
		if len(rb.Entrypoint) > 0 {
			cfg.Cmd = rb.Entrypoint
		} else {
			cfg.Cmd = []string{"/sbin/init"}
		}
		cfg.StopSignal = "SIGRTMIN+3"
	} else {
		hostCfg.CapAdd = []string{"CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE", "KILL"}
		hostCfg.ReadonlyRootfs = rb.ReadOnly
		if len(rb.Entrypoint) > 0 {
			cfg.Cmd = rb.Entrypoint
		} else {
			cfg.Cmd = []string{"/bin/sh", "-c", "sleep infinity"}
		}
	}

	return cfg, hostCfg
}

func networkMode(rb Runbook, attemptID string) string {
	if rb.Network == "none" {
		return "none"
	}
	// A per-attempt bridge, never the default one -- the default bridge can
	// reach GitLab on the host.
	return "praxis-" + attemptID
}

func parseMemory(s string) int64 {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "g"):
		mult, s = 1<<30, strings.TrimSuffix(s, "g")
	case strings.HasSuffix(s, "m"):
		mult, s = 1<<20, strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "k"):
		mult, s = 1<<10, strings.TrimSuffix(s, "k")
	}
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0
	}
	return n * mult
}

func instanceFrom(labels map[string]string, status, id, attemptID, name string) Instance {
	inst := Instance{
		AttemptID:     attemptID,
		Name:          name,
		Status:        Status(status),
		RunbookDigest: labels[LabelRunbook],
		ContainerID:   id,
	}
	if raw, ok := labels[LabelExpires]; ok {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			inst.ExpiresAt = t
		}
	}
	return inst
}
