package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Label keys. Kept in this shape deliberately: Docker accepts any value, but
// Kubernetes label values are capped at 63 chars and reject the colons in
// "sha256:..." and RFC3339 timestamps. The k8s backend therefore puts
// AttemptID in a label and the other two in annotations -- same keys, same
// meaning, no schema change.
const (
	LabelManaged = "praxis.managed-by"
	LabelAttempt = "praxis.attempt-id"
	LabelRunbook = "praxis.runbook-digest"
	LabelExpires = "praxis.expires-at"

	// LabelSpawnedAt and LabelWeight exist for internal/metrics -- see its
	// labels.go, which must match these exactly. A container without them
	// still parses (SpawnedAt zero, Weight defaults to 1); they were added
	// after the ones above and older tooling should not choke on their
	// absence.
	LabelSpawnedAt = "praxis.spawned-at"
	LabelWeight    = "praxis.weight"

	// LabelDiskLimit stamps the container's writable-layer cap in bytes.
	// Enforced by the reaper alongside TTL -- see reapTick in cmd/orchestrator
	// and Runbook.EffectiveDiskLimitBytes' comment for why every container
	// gets one of these regardless of whether the caller set DiskLimit.
	LabelDiskLimit = "praxis.disk-limit-bytes"

	// LabelShared lists the Runbook.SharedFromImage paths this sandbox got
	// from the host's copy (comma-separated); absent when it got none.
	LabelShared = "praxis.shared-from-image"

	ManagedValue = "praxis-orchestrator"
	NamePrefix   = "sbx-"

	// SandboxSlice is the persistent systemd slice every sandbox's cgroup
	// nests under (container.go's spec() sets HostConfig.CgroupParent to
	// this). Must match the [Slice] unit name in deploy/praxis-sbx.slice --
	// that unit exists specifically so hostmon's PRAXIS_SLICE_PATH resolves
	// to a real, always-present path (a bare --cgroup-parent string with no
	// backing unit would vanish with the last container using it, moving the
	// "path doesn't exist" problem to precisely the idle state hostmon is
	// most often read during).
	SandboxSlice = "praxis-sbx.slice"
)

var (
	ErrInvalidRunbook   = errors.New("invalid runbook")
	ErrInvalidAttemptID = errors.New("invalid attempt id")
	ErrNotFound         = errors.New("instance not found")
	ErrAtCapacity       = errors.New("at capacity")
	ErrNotRunning       = errors.New("instance not running")
)

type Status string

const (
	StatusRunning Status = "running"
	StatusExited  Status = "exited"
	StatusCreated Status = "created"
	StatusAbsent  Status = "absent"
)

type Instance struct {
	AttemptID     string    `json:"attempt_id"`
	Name          string    `json:"name"`
	Status        Status    `json:"status"`
	ExpiresAt     time.Time `json:"expires_at"`
	RunbookDigest string    `json:"runbook_digest"`
	ContainerID   string    `json:"container_id,omitempty"`
}

type ExecResult struct {
	ExitCode int     `json:"exit_code"`
	Stdout   string  `json:"stdout"`
	Stderr   string  `json:"stderr"`
	Duration float64 `json:"duration_seconds"`
}

// Backend is the whole interface. Four verbs plus two grader primitives.
// A KubernetesBackend implements the same set later, and the same test suite
// must pass against both -- that is how the k8s path gets proven before it is
// needed.
type Backend interface {
	Create(ctx context.Context, attemptID string, rb Runbook) (Instance, error)
	Get(ctx context.Context, attemptID string) (Instance, error)
	Destroy(ctx context.Context, attemptID string) (bool, error)
	Reap(ctx context.Context) ([]string, error)

	// PutFile and ExecScript exist for the portal's grader. The orchestrator
	// does not know a check script from any other script: it runs what it is
	// given and reports exit code and output. Grading semantics live in the
	// portal.
	PutFile(ctx context.Context, attemptID, path string, content []byte, mode int64) error
	ExecScript(ctx context.Context, attemptID string, script []byte, timeout time.Duration) (ExecResult, error)

	// ExecShell attaches an interactive PTY as `user` inside the named
	// attempt's sandbox and hands back the raw duplex stream. This is Phase C
	// (docs/session-02-plan.md): candidate shell access stays podman-exec
	// proxied through here, never a listener inside the sandbox -- network:
	// none and zero egress hold regardless of who is typing. The caller owns
	// the returned stream and must Close it when the session ends; there is
	// no resize support yet, and the caller decides `user` (empty means
	// "candidate") because the orchestrator does not read root_in_sandbox --
	// that is scenario knowledge the portal already has.
	ExecShell(ctx context.Context, attemptID, user string) (io.ReadWriteCloser, error)

	CountRunning(ctx context.Context) (int, error)
}

// SandboxName is deterministic, which is what makes Destroy a direct call on a
// name -- no lookup, no index, no orchestrator database. It also gives
// idempotency for free: a duplicate Create collides on the name, and that
// collision IS the idempotent answer.
func SandboxName(attemptID string) (string, error) {
	if err := ValidateAttemptID(attemptID); err != nil {
		return "", err
	}
	return NamePrefix + attemptID, nil
}

// ValidateAttemptID enforces DNS-1123 safety now, while the backend is Docker
// and does not care, so that the same IDs are legal namespace names when the
// k8s backend lands.
func ValidateAttemptID(id string) error {
	if len(id) == 0 || len(id) > 59 {
		return fmt.Errorf("%w: must be 1..59 chars", ErrInvalidAttemptID)
	}
	if id != strings.ToLower(id) {
		return fmt.Errorf("%w: must be lowercase", ErrInvalidAttemptID)
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' {
			return fmt.Errorf("%w: must match [a-z0-9-]", ErrInvalidAttemptID)
		}
	}
	if strings.HasPrefix(id, "-") || strings.HasSuffix(id, "-") {
		return fmt.Errorf("%w: must not start or end with '-'", ErrInvalidAttemptID)
	}
	return nil
}
