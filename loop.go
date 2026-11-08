package main

// loop.go — the core state machine, one machineCtl per machine:
//
//   - activity lease renewal (keeps the machine "busy": no 30-min idle pause)
//   - proactive restart every -restart-every (default 6h), before the per-run
//     ceiling (8h absolute cap / dynamic window). A pause we start owes no
//     cooldown and resets the max-run counter; a max-run pause costs
//     5→20→120+ min of escalating cooldown.
//   - deadline-aware early restart (within -deadline-margin of maxRunMinutes)
//   - auto-recovery: wake / resume / start, cooldown waiting, re-lease
//   - ring log + per-machine status snapshots for the web UI
//
// Plus doAction(): the manual "keep alive now" / "restart now" buttons.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// execCommand builds the notify hook shell command.
func execCommand(script string) *exec.Cmd { return exec.Command("sh", "-c", script) }

// ---------- ring log ----------

type logEntry struct {
	Seq   int       `json:"seq"`
	TS    time.Time `json:"ts"`
	Level string    `json:"level"` // debug | info | warn | error
	Msg   string    `json:"msg"`
}

type ringLog struct {
	mu      sync.Mutex
	buf     []logEntry
	seq     int
	maxSize int
	out     io.Writer
	file    io.Writer
	verbose bool
}

func newRingLog(maxSize int, out io.Writer, file io.Writer, verbose bool) *ringLog {
	return &ringLog{maxSize: maxSize, out: out, file: file, verbose: verbose}
}

func (r *ringLog) add(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	e := logEntry{TS: time.Now(), Level: level, Msg: msg}
	r.mu.Lock()
	r.seq++
	e.Seq = r.seq
	r.buf = append(r.buf, e)
	if len(r.buf) > r.maxSize {
		r.buf = r.buf[len(r.buf)-r.maxSize:]
	}
	printIt := level != "debug" || r.verbose
	out, file := r.out, r.file
	r.mu.Unlock()

	if printIt {
		line := fmt.Sprintf("%s [%s] %s\n", e.TS.Format("2006-01-02 15:04:05"), level, msg)
		if out != nil {
			io.WriteString(out, line)
		}
		if file != nil {
			io.WriteString(file, line)
		}
	}
}

// Seq returns the newest entry's sequence number.
func (r *ringLog) Seq() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq
}

// Since returns entries with seq > afterSeq (for incremental web polling).
func (r *ringLog) Since(afterSeq int) []logEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []logEntry
	for _, e := range r.buf {
		if e.Seq > afterSeq {
			out = append(out, e)
		}
	}
	return out
}

// ---------- per-machine status (web view model) ----------

type machineSnapshot struct {
	MachineID       string     `json:"machineId"`
	MachineName     string     `json:"machineName"`
	KeyName         string     `json:"keyName"`
	State           string     `json:"state"`
	Free            bool       `json:"free"`
	RunStartedAt    *time.Time `json:"runStartedAt"`
	RunAgeSec       float64    `json:"runAgeSec"`
	MaxRunMinutes   int64      `json:"maxRunMinutes"`
	Deadline        *time.Time `json:"deadline"`
	DeadlineInSec   float64    `json:"deadlineInSec"`
	AutoSuspendedAt *time.Time `json:"autoSuspendedAt"`
	CooldownUntil   *time.Time `json:"cooldownUntil"`
	CooldownInSec   float64    `json:"cooldownInSec"`
	AbuseLock       bool       `json:"abuseLock"`
	CooldownReason  string     `json:"cooldownReason"`

	RestartEverySec   float64    `json:"restartEverySec"`
	NextRestartInSec  float64    `json:"nextRestartInSec"`
	DeadlineMarginSec float64    `json:"deadlineMarginSec"`
	LastRestartAt     *time.Time `json:"lastRestartAt"`
	LeaseExpiresAt    *time.Time `json:"leaseExpiresAt"`
	LastFetchAt       *time.Time `json:"lastFetchAt"`
	LastError         string     `json:"lastError,omitempty"`
}

type statusResponse struct {
	Version     string            `json:"version"`
	Mock        bool              `json:"mock"`
	FatalError  string            `json:"fatalError,omitempty"`
	RestartMode string            `json:"restartMode"`
	IntervalSec float64           `json:"intervalSec"`
	TtlSeconds  int               `json:"ttlSeconds"`
	Machines    []machineSnapshot `json:"machines"`
}

// ---------- machineCtl: one per machine ----------

type machineCtl struct {
	app     *app
	client  *Client // the key's client that owns this machine's workspace
	keyID   string
	keyName string
	ref     string // user-supplied id or name
	id      string // resolved id

	opMu sync.Mutex // serializes lifecycle work (a machine runs one op at a time)

	mu             sync.Mutex // guards everything below
	lastState      string
	lastRestart    time.Time
	preRestartRun  time.Time
	preRestartOK   bool
	restartSupp    bool
	leaseExpiresAt time.Time
	lastMachine    *Machine
	fetchedAt      time.Time
	lastError      string
	abuseNotified  bool
}

// logf prefixes messages with the machine name when more than one is managed.
func (mc *machineCtl) logf(level, format string, args ...any) {
	prefix := ""
	if mc.app.countMachines() > 1 {
		prefix = "[" + mc.nameOrRef() + "] "
	}
	mc.app.ring.add(level, prefix+format, args...)
}

func (mc *machineCtl) nameOrRef() string {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if mc.lastMachine != nil && mc.lastMachine.Name != "" {
		return mc.lastMachine.Name
	}
	return mc.ref
}

// ---------- app ----------

type app struct {
	cfg    config
	client *Client // default client (env key / mock)
	ring   *ringLog
	keys   *keyStore

	machMu   sync.Mutex
	machines []*machineCtl

	mu         sync.Mutex
	fatalError string
}

// machinesSnapshot returns a stable copy for iteration.
func (a *app) machinesSnapshot() []*machineCtl {
	a.machMu.Lock()
	defer a.machMu.Unlock()
	out := make([]*machineCtl, len(a.machines))
	copy(out, a.machines)
	return out
}

func (a *app) countMachines() int {
	a.machMu.Lock()
	defer a.machMu.Unlock()
	return len(a.machines)
}

// addMachineCtl appends a machine unless its id is already managed.
func (a *app) addMachineCtl(mc *machineCtl) bool {
	a.machMu.Lock()
	defer a.machMu.Unlock()
	for _, e := range a.machines {
		if e.id == mc.id {
			return false
		}
	}
	a.machines = append(a.machines, mc)
	return true
}

// removeMachineCtls drops machines matching pred.
func (a *app) removeMachineCtls(pred func(*machineCtl) bool) {
	a.machMu.Lock()
	defer a.machMu.Unlock()
	kept := a.machines[:0]
	for _, e := range a.machines {
		if !pred(e) {
			kept = append(kept, e)
		}
	}
	a.machines = kept
}

func (a *app) info(format string, args ...any)   { a.ring.add("info", format, args...) }
func (a *app) warnf(format string, args ...any)  { a.ring.add("warn", format, args...) }
func (a *app) errf(format string, args ...any)   { a.ring.add("error", format, args...) }
func (a *app) debugf(format string, args ...any) { a.ring.add("debug", format, args...) }

func (a *app) setFatal(s string) {
	a.mu.Lock()
	a.fatalError = s
	a.mu.Unlock()
}

func (a *app) getFatal() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fatalError
}

// runLoop cycles all machines until the context is cancelled.
func (a *app) runLoop(ctx context.Context) {
	for {
		sleepFor := a.cycleAll(ctx)
		if ctx.Err() != nil {
			return
		}
		if a.getFatal() != "" {
			// Auth/config problems do not fix themselves; idle until restarted.
			sleepFor = 30 * time.Minute
		}
		if !sleepCtx(ctx, sleepFor) {
			return
		}
	}
}

// cycleAll runs one keep-alive pass over every machine and returns how long to
// sleep before the next pass (the smallest hint across machines).
func (a *app) cycleAll(ctx context.Context) time.Duration {
	sleepFor := a.cfg.interval
	for _, mc := range a.machinesSnapshot() {
		if h := a.cycleMachine(ctx, mc); h < sleepFor {
			sleepFor = h
		}
	}
	return sleepFor
}

// ---------- one machine cycle ----------

func (a *app) cycleMachine(ctx context.Context, mc *machineCtl) time.Duration {
	mc.opMu.Lock()
	defer mc.opMu.Unlock()
	cfg := a.cfg
	sleepFor := cfg.interval

	m, err := mc.client.GetMachine(ctx, mc.id)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) {
			if ae.IsAuthError() {
				mc.logf("error", "FATAL %v", ae)
				a.setFatal(ae.Error())
				a.notify(mc, "fatal", ae.Error())
				return sleepFor
			}
			if ae.Status == 404 {
				mc.logf("error", "FATAL machine %q not found (deleted, or wrong id/name): %v", mc.ref, ae)
				a.setFatal("machine not found: " + ae.Error())
				a.notify(mc, "fatal", "machine not found")
				return sleepFor
			}
		}
		mc.setLastError(err.Error())
		mc.logf("warn", "get machine: %v", err)
		return sleepFor
	}
	mc.setLastError("")
	mc.rememberMachine(m)
	mc.logState(m)

	// A machine behind an abuse/security lock is left strictly alone.
	if m.AbuseLock || strings.HasPrefix(m.CooldownReason, "abuse") {
		mc.mu.Lock()
		first := !mc.abuseNotified
		mc.abuseNotified = true
		mc.mu.Unlock()
		if first {
			msg := fmt.Sprintf("machine is under an abuse/security lock (cooldownReason=%s, until %s) — not touching it; resolve the appeal first",
				m.CooldownReason, m.CooldownUntil.Format(time.RFC3339))
			mc.logf("warn", "%s", msg)
			a.notify(mc, "abuse_lock", msg)
		}
		return 10 * time.Minute
	}
	mc.mu.Lock()
	mc.abuseNotified = false
	mc.mu.Unlock()

	// Cooldown (e.g. after a max-run pause): every wake path is refused until
	// cooldownUntil; there is nothing to do but wait it out.
	if m.State != "running" && m.HasCooldown && time.Now().Before(m.CooldownUntil) {
		left := time.Until(m.CooldownUntil)
		mc.logf("debug", "cooldown: %s left (until %s); waiting", left.Round(time.Second), m.CooldownUntil.Format(time.RFC3339))
		if left > cfg.interval {
			sleepFor = left + 5*time.Second
			if sleepFor > 30*time.Minute {
				sleepFor = 30 * time.Minute
			}
		}
		return sleepFor
	}

	switch m.State {
	case "running":
		if cfg.restartNow || a.restartDue(mc, m) {
			if err := a.doRestart(ctx, mc, m); err != nil {
				mc.logf("error", "restart: %v", err)
				a.notify(mc, "restart_failed", err.Error())
			}
			return cfg.interval
		}
		a.renewLease(ctx, mc, m)

	case "suspended":
		if !cfg.autoResume {
			mc.logf("debug", "machine is suspended and auto-resume is off; leaving it")
			return sleepFor
		}
		a.wakeOrResume(ctx, mc, m)

	case "stopped":
		if !cfg.autoStart {
			mc.logf("debug", "machine is stopped and auto-start is off; leaving it")
			return sleepFor
		}
		mc.logf("info", "machine stopped; starting it")
		if err := a.startMachine(ctx, mc, m); err != nil {
			mc.logf("error", "start: %v", err)
			a.notify(mc, "start_failed", err.Error())
		}

	case "suspending", "resuming", "starting", "stopping", "creating", "deleting":
		mc.logf("debug", "machine is mid-transition (%s); waiting it out", m.State)

	case "error":
		mc.logf("error", "machine reports state=error (see dashboard for lastError); will keep checking")

	default:
		mc.logf("debug", "unhandled machine state %q", m.State)
	}
	return sleepFor
}

// renewLease refreshes the activity lease that keeps the machine "busy".
func (a *app) renewLease(ctx context.Context, mc *machineCtl, m *Machine) {
	err := mc.client.RenewLease(ctx, m.ID, a.cfg.leaseID, a.cfg.ttlSeconds)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) {
			switch ae.Code {
			case "machine_not_running":
				mc.logf("debug", "lease: machine no longer running (%v); next cycle will recover", ae)
				return
			case "machine_cooldown":
				mc.logf("warn", "lease: machine in cooldown, waiting: %v", ae)
				return
			}
		}
		mc.logf("warn", "renew lease: %v", err)
		return
	}
	exp := time.Now().Add(time.Duration(a.cfg.ttlSeconds) * time.Second)
	mc.mu.Lock()
	mc.leaseExpiresAt = exp
	mc.mu.Unlock()
	mc.logf("debug", "lease renewed (ttl %ds, until %s)", a.cfg.ttlSeconds, exp.Format("15:04:05"))
}

// wakeOrResume brings a suspended machine back: wake for self-pauses
// (autoSuspendedAt set: idle or max-run), resume for pauses we started.
func (a *app) wakeOrResume(ctx context.Context, mc *machineCtl, m *Machine) {
	var (
		op  string
		err error
		how string
	)
	if m.HasAutoSuspend {
		how = "wake"
		op, err = mc.client.Wake(ctx, m.ID)
	} else {
		how = "resume"
		op, err = mc.client.Resume(ctx, m.ID)
	}
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.IsAuthError() {
			mc.logf("error", "FATAL %v", ae)
			a.setFatal(ae.Error())
			a.notify(mc, "fatal", ae.Error())
			return
		}
		// A pause we started will not respond to wake and vice versa; try the other once.
		if errors.As(err, &ae) && (ae.Code == "machine_not_running" || ae.Code == "operation_conflict" || ae.Status == 404 || ae.Status == 405) {
			mc.logf("debug", "%s failed (%v); trying the other path", how, ae)
			if how == "wake" {
				how = "resume"
				op, err = mc.client.Resume(ctx, m.ID)
			} else {
				how = "wake"
				op, err = mc.client.Wake(ctx, m.ID)
			}
		}
		if err != nil {
			mc.logf("error", "%s: %v", how, err)
			a.notify(mc, "wake_failed", err.Error())
			return
		}
	}
	mc.logf("info", "%s sent (op %s); waiting for the machine", how, op)
	if _, err := mc.client.WaitOperation(ctx, op); err != nil {
		mc.logf("debug", "%s wait: %v (checking state next cycle)", how, err)
	}
	if nm, err := mc.client.GetMachine(ctx, m.ID); err == nil && nm.State == "running" {
		mc.rememberMachine(nm)
		mc.logf("info", "machine %s and running again; re-taking lease", how)
		a.renewLease(ctx, mc, nm)
	}
	a.notify(mc, "woken", how+" completed")
}

// startMachine starts a stopped machine and waits for it.
func (a *app) startMachine(ctx context.Context, mc *machineCtl, m *Machine) error {
	op, err := mc.client.Start(ctx, m.ID)
	if err != nil {
		return err
	}
	if _, err := mc.client.WaitOperation(ctx, op); err != nil {
		return err
	}
	mc.logf("info", "machine started (op %s)", op)
	a.notify(mc, "started", "machine started")
	if nm, err := mc.client.GetMachine(ctx, m.ID); err == nil && nm.State == "running" {
		mc.rememberMachine(nm)
		a.renewLease(ctx, mc, nm)
	}
	return nil
}

// ---------- proactive restart ----------

// restartDue reports whether a proactive restart should happen now.
//
// Two triggers, both aimed at never letting a run hit its ceiling:
//   - run age >= -restart-every (the "every 6h" restart that beats the 8h cap)
//   - within -deadline-margin of the reported max-run deadline, when that is
//     sooner (dynamic runtime windows can end a run after 2h)
//
// A manual pause+resume owes no cooldown and resets the max-run counter, which
// is exactly why we restart *before* the deadline instead of riding it out.
func (a *app) restartDue(mc *machineCtl, m *Machine) bool {
	cfg := a.cfg
	if cfg.restartEvery <= 0 {
		return false
	}
	now := time.Now()

	mc.mu.Lock()
	lastRestart := mc.lastRestart
	preRun, preOK := mc.preRestartRun, mc.preRestartOK
	mc.mu.Unlock()

	// Anti-flap: never restart more often than 10 cycles (min 10 minutes).
	throttle := 10 * cfg.interval
	if throttle < 10*time.Minute {
		throttle = 10 * time.Minute
	}
	if !lastRestart.IsZero() && now.Sub(lastRestart) < throttle {
		mc.logf("debug", "restart suppressed: last restart %s ago (< throttle %s)", now.Sub(lastRestart).Round(time.Second), throttle)
		return false
	}

	if age, ok := m.RunAge(now); ok {
		// If our last restart did not move runStartedAt (service behaviour
		// changed), fall back to timer restarts instead of looping.
		if preOK && m.RunStartedAt.Equal(preRun) && now.Sub(lastRestart) < cfg.restartEvery {
			mc.mu.Lock()
			first := !mc.restartSupp
			mc.restartSupp = true
			mc.mu.Unlock()
			if first {
				mc.logf("warn", "runStartedAt did not change across our restart; falling back to timer-only restarts every %s", cfg.restartEvery)
			}
			return false
		}
		mc.mu.Lock()
		mc.restartSupp = false
		mc.mu.Unlock()

		if age >= cfg.restartEvery {
			mc.logf("debug", "restart due: run age %s >= %s", age.Round(time.Second), cfg.restartEvery)
			return true
		}
		if cfg.deadlineMarg > 0 {
			if dl, ok := m.RunDeadline(); ok && now.After(dl.Add(-cfg.deadlineMarg)) {
				mc.logf("debug", "restart due: run deadline %s is within margin %s (age %s, limit %dm)",
					dl.Format(time.RFC3339), cfg.deadlineMarg, age.Round(time.Second), m.MaxRunMinutes)
				return true
			}
		}
		return false
	}

	// No runStartedAt reported: fall back to a pure timer.
	if now.Sub(lastRestart) >= cfg.restartEvery {
		mc.logf("debug", "restart due: no runStartedAt reported, timer %s elapsed", cfg.restartEvery)
		return true
	}
	return false
}

// doRestart runs one proactive restart cycle: release lease -> suspend ->
// resume (or stop -> start) -> wait -> re-take lease. The machine's own
// pause path (max-run) owes an escalating cooldown; a pause we start owes
// none and resets the max-run counter.
func (a *app) doRestart(ctx context.Context, mc *machineCtl, m *Machine) error {
	cfg := a.cfg
	age, _ := m.RunAge(time.Now())
	reason := fmt.Sprintf("run age %s", age.Round(time.Second))
	if dl, ok := m.RunDeadline(); ok {
		reason += fmt.Sprintf(", ceiling %s (maxRunMinutes %d)", dl.Format(time.RFC3339), m.MaxRunMinutes)
	}
	mc.logf("info", "proactive restart (%s mode): %s", cfg.restartMode, reason)
	a.notify(mc, "restart", fmt.Sprintf("proactive restart (%s): %s", cfg.restartMode, reason))

	// Drop the lease first: the run is ending anyway.
	_ = mc.client.ReleaseLease(ctx, m.ID, cfg.leaseID)

	ctxS, cancelS := context.WithTimeout(ctx, 4*time.Minute)
	defer cancelS()

	if cfg.restartMode == "pause" {
		op, err := mc.client.Suspend(ctxS, m.ID)
		if err != nil {
			return fmt.Errorf("suspend: %w", err)
		}
		if _, err := mc.client.WaitOperation(ctxS, op); err != nil {
			return fmt.Errorf("suspend wait (op %s): %w", op, err)
		}
		mc.logf("debug", "suspended (op %s); resuming", op)

		ctxR, cancelR := context.WithTimeout(ctx, 6*time.Minute)
		defer cancelR()
		op, err = mc.client.Resume(ctxR, m.ID)
		if err != nil {
			// A cooldown should never follow a pause we started; if it does,
			// wait it out and retry once.
			var ae *APIError
			if errors.As(err, &ae) && ae.Code == "machine_cooldown" {
				mc.logf("warn", "resume hit cooldown (%v); waiting and retrying once", ae)
				if !sleepCtx(ctx, ae.RetryAfter+5*time.Second) {
					return ctx.Err()
				}
				op, err = mc.client.Resume(ctxR, m.ID)
			}
			if err != nil {
				return fmt.Errorf("resume: %w", err)
			}
		}
		if _, err := mc.client.WaitOperation(ctxR, op); err != nil {
			return fmt.Errorf("resume wait (op %s): %w", op, err)
		}
	} else {
		op, err := mc.client.Stop(ctxS, m.ID)
		if err != nil {
			return fmt.Errorf("stop: %w", err)
		}
		if _, err := mc.client.WaitOperation(ctxS, op); err != nil {
			return fmt.Errorf("stop wait (op %s): %w", op, err)
		}
		mc.logf("debug", "stopped (op %s); starting", op)

		ctxR, cancelR := context.WithTimeout(ctx, 6*time.Minute)
		defer cancelR()
		op, err = mc.client.Start(ctxR, m.ID)
		if err != nil {
			return fmt.Errorf("start: %w", err)
		}
		if _, err := mc.client.WaitOperation(ctxR, op); err != nil {
			return fmt.Errorf("start wait (op %s): %w", op, err)
		}
	}

	// Confirm running and re-take the lease.
	for i := 0; i < 10; i++ {
		nm, err := mc.client.GetMachine(ctx, m.ID)
		if err != nil {
			return fmt.Errorf("post-restart get machine: %w", err)
		}
		if nm.State == "running" {
			mc.rememberMachine(nm)
			mc.logf("info", "restart done: machine running again (runStartedAt=%s)", nm.RunStartedAt.Format(time.RFC3339))
			a.renewLease(ctx, mc, nm)
			mc.mu.Lock()
			mc.lastRestart = time.Now()
			mc.preRestartRun, mc.preRestartOK = m.RunStartedAt, m.HasRunStart
			mc.mu.Unlock()
			a.notify(mc, "restarted", "proactive restart complete")
			return nil
		}
		if !sleepCtx(ctx, 3*time.Second) {
			return ctx.Err()
		}
	}
	return fmt.Errorf("machine not running yet after restart")
}

// ---------- manual actions (web buttons) ----------

// findMachine matches by id or name; empty ref works when exactly one machine
// is managed.
func (a *app) findMachine(ref string) (*machineCtl, error) {
	machines := a.machinesSnapshot()
	if ref == "" && len(machines) == 1 {
		return machines[0], nil
	}
	for _, mc := range machines {
		if mc.id == ref || mc.ref == ref {
			return mc, nil
		}
		mc.mu.Lock()
		name := ""
		if mc.lastMachine != nil {
			name = mc.lastMachine.Name
		}
		mc.mu.Unlock()
		if name != "" && name == ref {
			return mc, nil
		}
	}
	if ref == "" {
		return nil, fmt.Errorf("machine is required (multiple machines managed)")
	}
	return nil, fmt.Errorf("no managed machine %q", ref)
}

// doAction runs one manual action on one machine, serialized with its cycle.
//   - keepalive: make the machine alive right now (wake/resume/start as needed,
//     then hold the lease)
//   - restart:   run one proactive restart cycle now
func (a *app) doAction(ctx context.Context, action, ref string) (string, error) {
	mc, err := a.findMachine(ref)
	if err != nil {
		return "", err
	}
	mc.opMu.Lock()
	defer mc.opMu.Unlock()

	m, err := mc.client.GetMachine(ctx, mc.id)
	if err != nil {
		return "", err
	}
	mc.rememberMachine(m)
	mc.logState(m)

	name := m.Name
	switch action {
	case "keepalive", "keep-alive":
		switch m.State {
		case "running":
			a.renewLease(ctx, mc, m)
			return name + ": lease renewed; machine is running", nil
		case "suspended":
			a.wakeOrResume(ctx, mc, m)
			return name + ": wake/resume sent", nil
		case "stopped":
			if err := a.startMachine(ctx, mc, m); err != nil {
				return "", err
			}
			return name + ": machine started", nil
		default:
			return "", fmt.Errorf("%s: machine is %s right now; try again in a moment", name, m.State)
		}
	case "restart":
		if m.State != "running" {
			return "", fmt.Errorf("%s: machine is %s; run keepalive first", name, m.State)
		}
		if err := a.doRestart(ctx, mc, m); err != nil {
			return "", err
		}
		return name + ": restart complete", nil
	default:
		return "", fmt.Errorf("unknown action %q (want keepalive or restart)", action)
	}
}

// ---------- state / snapshots ----------

func (mc *machineCtl) rememberMachine(m *Machine) {
	mc.mu.Lock()
	mc.lastMachine = m
	mc.fetchedAt = time.Now()
	mc.mu.Unlock()
}

func (mc *machineCtl) setLastError(s string) {
	mc.mu.Lock()
	mc.lastError = s
	mc.mu.Unlock()
}

func (mc *machineCtl) logState(m *Machine) {
	mc.mu.Lock()
	changed := m.State != mc.lastState
	mc.lastState = m.State
	mc.mu.Unlock()
	if changed {
		extra := ""
		if m.HasRunStart {
			extra = fmt.Sprintf(" runStartedAt=%s", m.RunStartedAt.Format(time.RFC3339))
		}
		if m.HasMaxRun {
			extra += fmt.Sprintf(" maxRunMinutes=%d", m.MaxRunMinutes)
		}
		mc.logf("info", "machine %s (%s): state=%s%s", m.Name, m.ID, m.State, extra)
		if m.State != "running" {
			mc.app.notify(mc, "state_change", "state="+m.State)
		}
	}
}

// snapshot builds one machine's web view model.
func (mc *machineCtl) snapshot() machineSnapshot {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	now := time.Now()
	cfg := mc.app.cfg
	s := machineSnapshot{
		MachineID:         mc.id,
		KeyName:           mc.keyName,
		RestartEverySec:   cfg.restartEvery.Seconds(),
		DeadlineMarginSec: cfg.deadlineMarg.Seconds(),
		LastError:         mc.lastError,
	}
	if !mc.lastRestart.IsZero() {
		t := mc.lastRestart
		s.LastRestartAt = &t
	}
	if !mc.leaseExpiresAt.IsZero() {
		t := mc.leaseExpiresAt
		s.LeaseExpiresAt = &t
	}
	if !mc.fetchedAt.IsZero() {
		t := mc.fetchedAt
		s.LastFetchAt = &t
	}
	m := mc.lastMachine
	if m == nil {
		s.State = "unknown"
		s.MachineName = mc.ref
		return s
	}
	s.MachineID, s.MachineName, s.State, s.Free = m.ID, m.Name, m.State, m.Free
	s.AbuseLock, s.CooldownReason = m.AbuseLock, m.CooldownReason
	s.MaxRunMinutes = m.MaxRunMinutes
	if m.HasRunStart {
		t := m.RunStartedAt
		s.RunStartedAt = &t
		s.RunAgeSec = now.Sub(m.RunStartedAt).Seconds()
		if s.RunAgeSec < 0 {
			s.RunAgeSec = 0
		}
	}
	if dl, ok := m.RunDeadline(); ok {
		t := dl
		s.Deadline = &t
		s.DeadlineInSec = dl.Sub(now).Seconds()
	}
	if m.HasCooldown {
		t := m.CooldownUntil
		s.CooldownUntil = &t
		s.CooldownInSec = m.CooldownUntil.Sub(now).Seconds()
		if s.CooldownInSec < 0 {
			s.CooldownInSec = 0
		}
	}
	if m.HasAutoSuspend {
		t := m.AutoSuspendedAt
		s.AutoSuspendedAt = &t
	}
	// When is the next proactive restart due?
	if cfg.restartEvery > 0 && m.State == "running" {
		best := time.Time{}
		consider := func(t time.Time) {
			if t.IsZero() {
				return
			}
			if t.Before(now) {
				t = now
			}
			if best.IsZero() || t.Before(best) {
				best = t
			}
		}
		if m.HasRunStart {
			consider(m.RunStartedAt.Add(cfg.restartEvery))
			if cfg.deadlineMarg > 0 {
				if dl, ok := m.RunDeadline(); ok {
					consider(dl.Add(-cfg.deadlineMarg))
				}
			}
		}
		if !mc.lastRestart.IsZero() {
			consider(mc.lastRestart.Add(cfg.restartEvery))
		}
		if !best.IsZero() {
			s.NextRestartInSec = best.Sub(now).Seconds()
			if s.NextRestartInSec < 0 {
				s.NextRestartInSec = 0
			}
		}
	}
	return s
}

// statusResponse builds the full web status payload.
func (a *app) status() statusResponse {
	out := statusResponse{
		Version:     version,
		Mock:        a.cfg.mock,
		FatalError:  a.getFatal(),
		RestartMode: a.cfg.restartMode,
		IntervalSec: a.cfg.interval.Seconds(),
		TtlSeconds:  a.cfg.ttlSeconds,
		Machines:    make([]machineSnapshot, 0, a.countMachines()),
	}
	for _, mc := range a.machinesSnapshot() {
		out.Machines = append(out.Machines, mc.snapshot())
	}
	return out
}

// refreshMachine fetches fresh state for one machine (used by /api/status so
// the dashboard feels live without waiting for the next keep-alive cycle).
func (a *app) refreshMachine(ctx context.Context, mc *machineCtl) {
	mc.mu.Lock()
	stale := mc.fetchedAt.IsZero() || time.Since(mc.fetchedAt) > 10*time.Second
	id := mc.id
	mc.mu.Unlock()
	if !stale {
		return
	}
	m, err := mc.client.GetMachine(ctx, id)
	if err != nil {
		mc.setLastError(err.Error())
		return
	}
	mc.setLastError("")
	mc.rememberMachine(m)
}

// ---------- notify / shutdown ----------

// notify runs the configured hook with event context in the environment.
func (a *app) notify(mc *machineCtl, event, message string) {
	if a.cfg.notifyCmd == "" {
		return
	}
	name := ""
	if mc != nil {
		name = mc.nameOrRef()
	}
	cmd := execCommand(a.cfg.notifyCmd)
	cmd.Env = append(os.Environ(),
		"IB_EVENT="+event,
		"IB_MACHINE="+name,
		"IB_MESSAGE="+message,
	)
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	select {
	case err := <-done:
		if err != nil {
			a.warnf("notify-cmd %q: %v", a.cfg.notifyCmd, err)
		}
	case <-time.After(15 * time.Second):
		a.warnf("notify-cmd %q timed out after 15s", a.cfg.notifyCmd)
	}
}

func (a *app) shutdown() {
	if a.cfg.releaseOnExit {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, mc := range a.machinesSnapshot() {
			if err := mc.client.ReleaseLease(ctx, mc.id, a.cfg.leaseID); err != nil {
				a.warnf("release lease on exit (%s): %v", mc.nameOrRef(), err)
			} else {
				a.info("lease released on exit (%s)", mc.nameOrRef())
			}
		}
	}
	a.info("instabox-keepalive stopped")
}

// sleepCtx sleeps for d, returning false if the context was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
