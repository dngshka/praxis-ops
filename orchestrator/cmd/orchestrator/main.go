// Command orchestrator is the Praxis sandbox interpreter.
//
// Scope: takes a Runbook plus an attempt_id and drives a container runtime.
// It knows nothing about tickets, stars, gates, scoring or candidates. If a
// field named difficulty or fault_id ever appears in this binary, the boundary
// has leaked.
//
// State lives on the runtime as labels. There is no database and no in-memory
// registry -- a restart rebuilds the whole world from a label selector.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"praxis-orchestrator/internal/api"
	"praxis-orchestrator/internal/dashboard"
	"praxis-orchestrator/internal/metrics"
	"praxis-orchestrator/internal/sandbox"
)

var version = "dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	token := os.Getenv("PRAXIS_ORCH_TOKEN")
	if token == "" {
		log.Error("PRAXIS_ORCH_TOKEN is required")
		os.Exit(1)
	}

	addr := envOr("PRAXIS_LISTEN", "127.0.0.1:8081")
	reapEvery := time.Duration(envInt("PRAXIS_REAP_INTERVAL", 30)) * time.Second
	// Weight, not a flat container count: PRAXIS_MAX_CONCURRENT is retired.
	// Every current ticket (SJN-01/SKN-01/CPT-01) leaves Runbook.Weight unset,
	// which EffectiveWeight() treats as 1 -- so this is a flat concurrent
	// count until a ticket sets a real weight. 35 = ~70% of CPT-01's real
	// measured staircase ceiling (50, disk-bound -- docs/capacity-benchmark.md
	// "Phase D close-out", 2026-09-05/06); matches the deploy unit's default
	// on purpose.
	capacityWeight := envInt("PRAXIS_CAPACITY_WEIGHT", 35)
	execTimeout := time.Duration(envInt("PRAXIS_EXEC_TIMEOUT", 120)) * time.Second

	backend, err := sandbox.NewContainerBackend()
	if err != nil {
		log.Error("backend init failed; is DOCKER_HOST pointing at the rootless socket?", "err", err)
		os.Exit(1)
	}
	defer backend.Close()

	// Runbook.SharedFromImage: host copies of image trees, served read-only.
	// Optional and never fatal: without it every sandbox runs from its image.
	if dir := os.Getenv("PRAXIS_SHARED_DIR"); dir != "" {
		if err := backend.SetSharedDir(dir); err != nil {
			log.Error("PRAXIS_SHARED_DIR unusable; sandboxes run from their images alone", "err", err)
		} else {
			log.Info("serving shared_from_image paths", "dir", dir)
		}
	}

	reg := metrics.NewRegistry("orchestrator", version)

	// backend.RawClient() satisfies metrics.Lister structurally (it's
	// *client.Client, the same docker client backend already wraps) --
	// passed separately rather than through the Backend interface itself,
	// which stays runtime-neutral (a future KubernetesBackend has no reason
	// to know about internal/metrics's types). Shared by the reaper and the
	// API server's admission check -- see api.New's comment on why admission
	// does its own live list rather than reading reg's cached snapshot.
	lister := backend.RawClient()

	apiRoutes := api.New(backend, lister, reg, api.Config{
		Token: token, CapacityWeight: capacityWeight, ExecTimeout: execTimeout,
	}, log).Routes()

	// The operator dashboard's static frontend, mounted at /ui/ -- not
	// /dashboard/, which would (as a ServeMux subtree pattern) swallow the
	// API's own GET /dashboard/summary before this outer mux ever handed
	// the request to apiRoutes. See internal/dashboard's own header
	// comment for the full reasoning.
	dashboardRoutes, err := dashboard.Handler("/ui/")
	if err != nil {
		log.Error("dashboard asset init failed", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle("/", apiRoutes)
	mux.Handle("/ui/", dashboardRoutes)
	// "/{$}" (Go 1.22+ ServeMux syntax) matches ONLY the exact root path,
	// not every path as "/" alone would -- more specific than apiRoutes'
	// own "/" registration above, so this wins for a bare visit to the
	// orchestrator's URL without shadowing any real API route.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go reaper(ctx, backend, lister, reapEvery, reg, capacityWeight, log)

	go func() {
		log.Info("orchestrator listening", "addr", addr, "capacity_weight", capacityWeight)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

// reaper enforces TTL. This is what makes a stateless orchestrator safe: if the
// portal loses an attempt record, the container still dies on schedule, because
// expiry does not depend on anyone remembering why the sandbox exists.
//
// The snapshot this reads is also what internal/metrics publishes -- one
// list call serves both reaping and metrics, so a scrape costs nothing on the
// socket and the two can never disagree about what's actually running
// (docs/observability-wiring.md §1). This is why it drives destroys through
// lister+Backend.Destroy per session rather than calling Backend.Reap(): that
// method does its own independent listing internally, which is exactly the
// two-views-disagreeing risk this design avoids. Backend.Reap() still exists
// and is still used by the manual POST /reap endpoint -- a known, accepted
// second implementation for on-demand use, not touched here.
func reaper(ctx context.Context, b sandbox.Backend, lister metrics.Lister, every time.Duration, reg *metrics.Registry, capacityLimit int, log *slog.Logger) {
	// Prime immediately, not just on the first ticker fire. Confirmed against
	// a real restart: without this, /metrics and /sessions read a fully empty
	// snapshot (Sessions=nil, taken_at the zero time) for up to a full
	// PRAXIS_REAP_INTERVAL after every restart -- indistinguishable from an
	// actually idle box for as long as 30s by default. hostmon's own main.go
	// already primes once before its ticker loop for exactly this reason;
	// this just brings the orchestrator's reaper in line with it.
	reapTick(ctx, b, lister, reg, capacityLimit, log)

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reapTick(ctx, b, lister, reg, capacityLimit, log)
		}
	}
}

func reapTick(ctx context.Context, b sandbox.Backend, lister metrics.Lister, reg *metrics.Registry, capacityLimit int, log *slog.Logger) {
	start := time.Now()

	snap := metrics.CollectManaged(ctx, lister)
	if snap.Err != nil {
		log.Error("reap cycle: collection failed", "err", snap.Err)
		reg.SetSnapshot(snap) // publish the failure; a stale zero is worse
		return
	}

	now := time.Now()
	var killed []string
	for _, sess := range snap.Sessions {
		reason := ""
		switch {
		case sess.Expired(now):
			reason = "ttl"
		case sess.OverDiskLimit():
			// Checked every tick, same cadence as TTL -- not a soft warning.
			// root_in_sandbox tickets (SJN-01, CPT-01) had nothing stopping a
			// candidate writing past any sane size before this existed; see
			// Runbook.EffectiveDiskLimitBytes and Session.OverDiskLimit.
			reason = "disk_limit"
			log.Warn("sandbox over its disk limit, destroying",
				"attempt_id", sess.AttemptID, "used_bytes", sess.DiskUsedBytes,
				"limit_bytes", sess.DiskLimitBytes)
		default:
			continue
		}
		ok, err := b.Destroy(ctx, sess.AttemptID)
		if err != nil {
			reg.IncDestroy("error")
			log.Error("reap destroy failed", "attempt_id", sess.AttemptID, "reason", reason, "err", err)
			continue
		}
		if ok {
			reg.IncDestroy(reason)
			killed = append(killed, sess.AttemptID)
		}
	}
	if len(killed) > 0 {
		log.Info("reaped expired sandboxes", "count", len(killed), "attempts", killed)
	}

	reg.SetSnapshot(snap)
	reg.SetCapacity(metrics.WeightInFlight(snap), capacityLimit)
	// Only on a completed tick, deliberately: PraxisReaperStalled fires on
	// the ABSENCE of progress, which catches a deadlock an error counter
	// would not (docs/observability-wiring.md §1).
	reg.ObserveReaper(time.Since(start))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
