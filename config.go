package main

// config.go — env + flag double channel. Flags win over environment variables
// so one-off overrides are easy; env suits systemd and CI.

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

type config struct {
	// connection
	machine string // id or exact name
	apiKey  string // ibk_... (env preferred; keeps the key out of ps)
	apiBase string
	mock    bool // run against the built-in offline mock API

	// keep-alive
	interval      time.Duration // poll / lease-renew cadence
	ttlSeconds    int           // activity lease TTL (30-3600)
	leaseID       string        // stable lease id
	restartEvery  time.Duration // proactive restart period (the "every 6h" knob)
	deadlineMarg  time.Duration // restart this long before the max-run deadline
	restartMode   string        // pause (suspend+resume) | stop (stop+start)
	autoResume    bool
	autoStart     bool
	releaseOnExit bool

	// misc
	notifyCmd  string
	once       bool
	restartNow bool
	webAddr    string // web UI listen addr; "" disables
	webToken   string // optional token gate for the web UI
	keysFile   string // API key store (0600)
	logFile    string
	verbose    bool
}

func loadConfig(args []string) (config, error) {
	c := config{}
	fs := flag.NewFlagSet("keepalive", flag.ContinueOnError)
	fs.StringVar(&c.machine, "machine", os.Getenv("INSTABOX_MACHINE"), "machine id or exact name (env INSTABOX_MACHINE)")
	fs.StringVar(&c.apiKey, "api-key", os.Getenv("INSTABOX_API_KEY"), "API key ibk_... (env INSTABOX_API_KEY; prefer env)")
	fs.StringVar(&c.apiBase, "api-base", envOr("INSTABOX_API_BASE", "https://api.instabox.sh"), "API base URL")
	fs.BoolVar(&c.mock, "mock", false, "run against the built-in offline mock (no real API calls)")
	fs.DurationVar(&c.interval, "interval", 2*time.Minute, "poll / lease-renew interval (must be < --ttl)")
	fs.IntVar(&c.ttlSeconds, "ttl", 300, "activity lease TTL in seconds (30-3600)")
	fs.StringVar(&c.leaseID, "lease-id", "keepalive", "activity lease id")
	fs.DurationVar(&c.restartEvery, "restart-every", 6*time.Hour, "proactive restart period to stay under the per-run cap; 0 disables")
	fs.DurationVar(&c.deadlineMarg, "deadline-margin", 10*time.Minute, "also restart this long before the max-run deadline when sooner (dynamic windows); 0 disables")
	fs.StringVar(&c.restartMode, "restart-mode", "pause", "pause (suspend+resume, keeps processes) or stop (stop+start, reboot)")
	fs.BoolVar(&c.autoResume, "auto-resume", true, "wake/resume the machine when paused and off cooldown")
	fs.BoolVar(&c.autoStart, "auto-start", false, "start the machine when stopped")
	fs.BoolVar(&c.releaseOnExit, "release-on-exit", true, "release the activity lease on clean shutdown")
	fs.StringVar(&c.notifyCmd, "notify-cmd", "", "shell command run on state changes (IB_EVENT/IB_MACHINE/IB_STATE/IB_MESSAGE env)")
	fs.BoolVar(&c.once, "once", false, "run one cycle and exit (for cron)")
	fs.BoolVar(&c.restartNow, "restart-now", false, "force a restart on this cycle")
	fs.StringVar(&c.webAddr, "web-addr", ":8080", "web UI listen address; empty disables")
	fs.StringVar(&c.webToken, "web-token", os.Getenv("INSTABOX_WEB_TOKEN"), "optional access token for the web UI (env INSTABOX_WEB_TOKEN)")
	fs.StringVar(&c.keysFile, "keys-file", "keepalive-keys.json", "API key store path (0600); keys added in the web UI live here")
	fs.StringVar(&c.logFile, "log-file", "", "also append logs to this file")
	fs.BoolVar(&c.verbose, "verbose", false, "log every lease renewal and skip reason")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	return c, c.validate()
}

func (c *config) validate() error {
	if c.mock {
		// mock mode: no key or machine needed; fill friendly defaults.
		// "all" shows every scripted machine in the web UI.
		if c.apiKey == "" {
			c.apiKey = "ibk_mock"
		}
		if c.machine == "" {
			c.machine = "all"
		}
	}
	if c.machine == "" {
		c.machine = "all" // every machine of every configured key
	}
	if c.apiKey == "" {
		return errors.New("-api-key (or INSTABOX_API_KEY) is required; create one in the dashboard under API keys")
	}
	if c.ttlSeconds < 30 || c.ttlSeconds > 3600 {
		return errors.New("-ttl must be 30-3600 seconds")
	}
	if c.interval >= time.Duration(c.ttlSeconds)*time.Second {
		return errors.New("-interval must be shorter than -ttl so the lease never lapses")
	}
	if c.interval < 15*time.Second {
		return errors.New("-interval must be at least 15s")
	}
	switch c.restartMode {
	case "pause", "stop":
	default:
		return errors.New("-restart-mode must be pause or stop")
	}
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// machineRefs splits -machine into refs. Comma-separated list of ids/names,
// or "all" / "*" for every machine in the workspace.
func (c *config) machineRefs() []string {
	var out []string
	for _, r := range strings.Split(c.machine, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

// String renders the config for the startup banner (never includes the key).
func (c config) String() string {
	key := "unset"
	if c.apiKey != "" {
		key = fmt.Sprintf("set(%d chars)", len(c.apiKey))
	}
	return fmt.Sprintf("machine=%s apiBase=%s key=%s mock=%v interval=%s ttl=%ds restart-every=%s deadline-margin=%s mode=%s web=%q",
		c.machine, c.apiBase, key, c.mock, c.interval, c.ttlSeconds,
		c.restartEvery, c.deadlineMarg, c.restartMode, c.webAddr)
}
