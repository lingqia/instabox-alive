package main

// web.go — embedded web UI + REST API:
//
//	GET  /                 the page (web/index.html, embedded into the binary)
//	GET  /api/status       all machines + keep-alive status snapshots
//	GET  /api/log?since=N  ring-log entries after seq N (incremental polling)
//	POST /api/action       {"action":"keepalive"|"restart","machine":"<id|name>"}
//
// Optional bearer-token gate: set -web-token (env INSTABOX_WEB_TOKEN) and every
// request must carry it as X-Keepalive-Token or ?token= — recommended when the
// UI is exposed through a machine's public port URL.

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

func (a *app) serveWeb(ctx context.Context) {
	mux := http.NewServeMux()

	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if tok := a.cfg.webToken; tok != "" {
				got := r.Header.Get("X-Keepalive-Token")
				if got == "" {
					got = r.URL.Query().Get("token")
				}
				if got != tok {
					writeJSONOut(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "bad token"})
					return
				}
			}
			h(w, r)
		}
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if tok := a.cfg.webToken; tok != "" && r.URL.Query().Get("token") != tok && r.Header.Get("X-Keepalive-Token") != tok {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>401</title><body style="font-family:sans-serif;padding:40px">需要访问令牌：在 URL 后加 <code>?token=你的令牌</code></body>`)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})

	mux.HandleFunc("/api/status", auth(func(w http.ResponseWriter, r *http.Request) {
		// Opportunistic background refresh so the dashboard feels live without
		// waiting for the next keep-alive cycle (throttled to 10s per machine).
		for _, mc := range a.machinesSnapshot() {
			go a.refreshMachine(context.Background(), mc)
		}
		writeJSONOut(w, 200, a.status())
	}))

	// Key management: list (masked) / add (validates then discovers machines) /
	// delete. Secrets are never returned to the browser.
	mux.HandleFunc("/api/keys", auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSONOut(w, 200, map[string]any{"keys": a.keyInfos()})
		case http.MethodPost:
			var body struct {
				Name string `json:"name"`
				Key  string `json:"key"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSONOut(w, 400, map[string]any{"ok": false, "error": "bad JSON body: " + err.Error()})
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
			defer cancel()
			info, err := a.addKey(ctx, body.Name, body.Key)
			if err != nil {
				writeJSONOut(w, 200, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			writeJSONOut(w, 200, map[string]any{"ok": true, "key": info})
		default:
			writeJSONOut(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "GET or POST"})
		}
	}))

	mux.HandleFunc("/api/keys/", auth(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/keys/")
		if r.Method != http.MethodDelete || id == "" {
			writeJSONOut(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "DELETE /api/keys/{id}"})
			return
		}
		if err := a.removeKey(id); err != nil {
			writeJSONOut(w, 200, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSONOut(w, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/log", auth(func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.Atoi(r.URL.Query().Get("since"))
		entries := a.ring.Since(since)
		if entries == nil {
			entries = []logEntry{}
		}
		next := since
		if len(entries) > 0 {
			next = entries[len(entries)-1].Seq
		} else if since == 0 {
			next = a.ring.Seq()
		}
		writeJSONOut(w, 200, map[string]any{"entries": entries, "next": next})
	}))

	mux.HandleFunc("/api/action", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONOut(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "POST only"})
			return
		}
		var body struct {
			Action  string `json:"action"`
			Machine string `json:"machine"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONOut(w, 400, map[string]any{"ok": false, "error": "bad JSON body: " + err.Error()})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		msg, err := a.doAction(ctx, body.Action, body.Machine)
		if err != nil {
			writeJSONOut(w, 200, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSONOut(w, 200, map[string]any{"ok": true, "message": msg})
	}))

	srv := &http.Server{Addr: a.cfg.webAddr, Handler: mux}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()
	a.info("web UI on http://%s", displayAddr(a.cfg.webAddr))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		a.errf("web server: %v", err)
	}
}

func writeJSONOut(w http.ResponseWriter, status int, v any) {
	body, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// displayAddr turns ":8080" into "0.0.0.0:8080" for the startup banner.
func displayAddr(addr string) string {
	if len(addr) > 0 && addr[0] == ':' {
		return "0.0.0.0" + addr
	}
	return addr
}
