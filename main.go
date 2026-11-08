package main

// main.go — wiring: config -> keys -> (mock) client -> machines -> loop + web.
//
// instabox-keepalive keeps InstaBox (https://instabox.sh) machines alive:
// activity lease renewal (no 30-min idle pause), proactive restart before the
// per-run ceiling (8h cap / dynamic window) every -restart-every (default 6h),
// and auto-recovery from pauses. Multiple API keys (one per workspace) are
// supported; keys can be added/removed from the web UI. Everything runs on the
// documented REST API.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const version = "1.2.0"

func main() {
	cfg, err := loadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(2)
	}

	var logw io.Writer = os.Stdout
	if cfg.logFile != "" {
		f, err := os.OpenFile(cfg.logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open log file: %v\n", err)
			os.Exit(2)
		}
		defer f.Close()
		logw = io.MultiWriter(os.Stdout, f)
	}
	ring := newRingLog(1000, logw, nil, cfg.verbose)

	// -mock: point the client at the built-in offline API. Every code path
	// (HTTP client, ops, loop, web) then runs exactly as against the real one.
	var stopMock func()
	if cfg.mock {
		url, stop := startMock()
		stopMock = stop
		cfg.apiBase = url
	}

	// Keys: the env key is an in-memory entry; keys added in the web UI are
	// persisted to -keys-file (0600). Each key reaches exactly one workspace.
	ks := newKeyStore(cfg.keysFile, cfg.apiBase)
	if cfg.apiKey != "" {
		ks.add("默认（环境变量）", cfg.apiKey, false, cfg.apiBase)
	}
	client := NewClient(cfg.apiBase, cfg.apiKey)
	a := &app{cfg: cfg, client: client, ring: ring, keys: ks}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(ks.list()) == 0 {
		ring.add("error", "没有可用的 API key：设置环境变量 INSTABOX_API_KEY，或启动后在 Web 界面『密钥』里添加")
		os.Exit(2)
	}

	// Managed machines = every machine of every key (refs: id/name list or "all").
	refs := cfg.machineRefs()
	for _, k := range ks.list() {
		a.discoverMachines(ctx, k, refs)
	}
	if a.countMachines() == 0 {
		ring.add("warn", "暂无匹配的机器（-machine=%q，各 key 的工作区里没有匹配项）", cfg.machine)
	}

	ring.add("info", "instabox-keepalive %s starting: %s (%d keys, %d machines)", version, cfg, len(ks.list()), a.countMachines())
	if cfg.webAddr != "" && cfg.webToken == "" {
		ring.add("warn", "web UI 未设置 -web-token：任何能访问该端口的人都可以操作机器和密钥！建议设置环境变量 INSTABOX_WEB_TOKEN")
	}
	a.notify(nil, "start", "keepalive daemon started")

	if cfg.once {
		a.cycleAll(ctx)
		a.shutdown()
		if stopMock != nil {
			stopMock()
		}
		if a.getFatal() != "" {
			os.Exit(1)
		}
		return
	}

	if cfg.webAddr != "" {
		go a.serveWeb(ctx)
	}
	go a.runLoop(ctx)

	<-ctx.Done()
	ring.add("info", "shutting down")
	a.shutdown()
	if stopMock != nil {
		stopMock()
	}
}
