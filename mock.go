package main

// mock.go — built-in offline InstaBox API mock (-mock).
//
// Serves the same REST surface the keep-alive touches, on an in-process
// httptest server, with several scripted machines that mutate on lifecycle
// calls (suspend -> suspended, resume/wake/start -> running with a fresh run
// window). Used for local verification, demos and smoke tests.
//
// Machines are scripted with env (same names as mock/mock_server.py):
//
//	MOCK_MACHINES=name:state:ageMin:maxRun[:auto][:cooldown=SEC],...
//	default: demo:running:60:480,demo2:suspended:130:120:auto,demo3:stopped:0:480
//
// GET /__calls lists every request received, for assertions.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type mockMachine struct {
	id       string
	name     string
	state    string
	runStart time.Time
	hasRun   bool
	maxRun   int64
	autoSusp *time.Time
	cooldown *time.Time
	abuse    bool
	leases   map[string]time.Time
}

type mockAPI struct {
	mu       sync.Mutex
	key      string
	machines []*mockMachine
	byID     map[string]*mockMachine
	ops      map[string]string
	calls    []string
	opN      int
}

func newMockAPI() *mockAPI {
	m := &mockAPI{
		key:  "ibk_mock",
		byID: map[string]*mockMachine{},
		ops:  map[string]string{},
	}
	spec := os.Getenv("MOCK_MACHINES")
	if spec == "" {
		spec = "demo:running:60:480,demo2:suspended:130:120:auto,demo3:stopped:0:480"
	}
	for _, part := range strings.Split(spec, ",") {
		f := strings.Split(strings.TrimSpace(part), ":")
		if len(f) < 4 {
			continue
		}
		mm := &mockMachine{
			id:     "mch_" + f[0],
			name:   f[0],
			state:  f[1],
			leases: map[string]time.Time{},
		}
		age, _ := strconv.ParseFloat(f[2], 64)
		mm.maxRun, _ = strconv.ParseInt(f[3], 10, 64)
		mm.runStart = time.Now().Add(-time.Duration(age * float64(time.Minute)))
		mm.hasRun = mm.state != "stopped"
		for _, opt := range f[4:] {
			switch {
			case opt == "auto":
				t := time.Now().Add(-5 * time.Minute)
				mm.autoSusp = &t
				mm.state = "suspended"
			case strings.HasPrefix(opt, "cooldown="):
				if n, err := strconv.Atoi(strings.TrimPrefix(opt, "cooldown=")); err == nil && n > 0 {
					t := time.Now().Add(time.Duration(n) * time.Second)
					mm.cooldown = &t
				}
			case opt == "abuse":
				mm.abuse = true
			}
		}
		m.machines = append(m.machines, mm)
		m.byID[mm.id] = mm
		m.byID[mm.name] = mm
	}
	return m
}

func iso(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func (mm *mockMachine) toJSON() map[string]any {
	out := map[string]any{
		"id": mm.id, "name": mm.name, "state": mm.state, "free": true,
		"maxRunMinutes": mm.maxRun, "operationId": nil,
		"cooldownUntil": nil, "autoSuspendedAt": nil,
		"abuseLock": nil, "cooldownReason": nil,
	}
	if mm.hasRun {
		out["runStartedAt"] = iso(mm.runStart)
	}
	if mm.autoSusp != nil {
		out["autoSuspendedAt"] = iso(*mm.autoSusp)
	}
	if mm.cooldown != nil {
		out["cooldownUntil"] = iso(*mm.cooldown)
	}
	if mm.abuse {
		out["abuseLock"] = map[string]any{"at": iso(time.Now()), "what": "mock lock"}
		out["cooldownReason"] = "abuse_cpu"
	}
	return out
}

// maybeExpireRun behaves like the real service: a run past maxRunMinutes is
// paused (max-run) with a short cooldown, and the next wake gets a fresh window.
func (mm *mockMachine) maybeExpireRun() {
	if mm.state == "running" && mm.hasRun && mm.maxRun > 0 &&
		time.Since(mm.runStart) >= time.Duration(mm.maxRun)*time.Minute {
		mm.state = "suspended"
		t := time.Now()
		mm.autoSusp = &t
		c := time.Now().Add(2 * time.Minute)
		mm.cooldown = &c
	}
}

func (m *mockAPI) newOp(typ, machineID string) map[string]any {
	m.opN++
	id := fmt.Sprintf("op_mock%d", m.opN)
	m.ops[id] = typ
	return map[string]any{
		"id": id, "state": "succeeded", "type": typ,
		"machineId": machineID, "trigger": "request", "phase": "",
		"runtime": map[string]any{"processesPreserved": true, "ready": true},
	}
}

func (m *mockAPI) err(w http.ResponseWriter, status int, code, msg string, retryable bool, retryAfter int) {
	e := map[string]any{
		"code": code, "message": msg, "requestId": "req_mock",
		"retryable": retryable, "operationId": nil,
	}
	if retryAfter > 0 {
		e["retryAfterSeconds"] = retryAfter
	}
	writeJSON(w, status, map[string]any{"error": e})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func splitPath(p string) []string {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

func (m *mockAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	method := r.Method

	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, method+" "+path)

	if path == "/__calls" {
		writeJSON(w, 200, map[string]any{"calls": m.calls})
		return
	}
	if r.Header.Get("X-API-Key") == "" {
		m.err(w, 401, "missing_key", "no API key sent", false, 0)
		return
	}

	parts := splitPath(path)
	switch {
	case len(parts) == 2 && parts[0] == "v1" && parts[1] == "machines":
		if method == "GET" {
			list := []any{}
			for _, mm := range m.machines {
				list = append(list, mm.toJSON())
			}
			writeJSON(w, 200, map[string]any{"machines": list, "nextCursor": nil})
			return
		}
	case len(parts) == 3 && parts[0] == "v1" && parts[1] == "machines":
		mm := m.byID[parts[2]]
		if mm == nil {
			m.err(w, 404, "not_found", "no such machine", false, 0)
			return
		}
		if method == "GET" {
			mm.maybeExpireRun()
			writeJSON(w, 200, mm.toJSON())
			return
		}
	case len(parts) == 5 && parts[0] == "v1" && parts[1] == "machines" && parts[3] == "activity-leases":
		mm := m.byID[parts[2]]
		if mm == nil {
			m.err(w, 404, "not_found", "no such machine", false, 0)
			return
		}
		leaseID := parts[4]
		switch method {
		case "PUT":
			var body struct {
				TTLSeconds int `json:"ttlSeconds"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.TTLSeconds <= 0 {
				body.TTLSeconds = 300
			}
			mm.maybeExpireRun()
			if mm.state != "running" {
				m.err(w, 409, "machine_not_running", "machine is not running", false, 0)
				return
			}
			exp := time.Now().Add(time.Duration(body.TTLSeconds) * time.Second)
			mm.leases[leaseID] = exp
			writeJSON(w, 200, map[string]any{"id": leaseID, "machineId": mm.id, "expiresAt": iso(exp)})
			return
		case "DELETE":
			delete(mm.leases, leaseID)
			w.WriteHeader(204)
			return
		}
	case len(parts) == 4 && parts[0] == "v1" && parts[1] == "machines":
		mm := m.byID[parts[2]]
		if mm == nil {
			m.err(w, 404, "not_found", "no such machine", false, 0)
			return
		}
		action := parts[3]
		if method == "POST" {
			if mm.abuse {
				m.err(w, 409, "machine_cooldown", "abuse lock in effect", true, 3600)
				return
			}
			if mm.cooldown != nil && time.Now().Before(*mm.cooldown) &&
				(action == "wake" || action == "resume" || action == "start") {
				m.err(w, 409, "machine_cooldown", "machine is cooling down", true,
					int(time.Until(*mm.cooldown).Seconds())+1)
				return
			}
			switch action {
			case "suspend":
				mm.maybeExpireRun()
				if mm.state != "running" {
					m.err(w, 409, "machine_not_running", "not running", false, 0)
					return
				}
				mm.state = "suspended"
				mm.autoSusp = nil // a pause we start is a manual pause
				writeJSON(w, 202, m.newOp("suspend", mm.id))
				return
			case "resume":
				if mm.state != "suspended" {
					m.err(w, 409, "machine_not_running", "not suspended", false, 0)
					return
				}
				mm.state = "running"
				mm.autoSusp = nil
				mm.cooldown = nil
				mm.runStart = time.Now() // fresh run window
				mm.hasRun = true
				writeJSON(w, 202, m.newOp("resume", mm.id))
				return
			case "wake":
				if mm.autoSusp == nil {
					m.err(w, 409, "operation_conflict", "machine is not idle-suspended", false, 0)
					return
				}
				mm.state = "running"
				mm.autoSusp = nil
				mm.cooldown = nil
				mm.runStart = time.Now()
				mm.hasRun = true
				writeJSON(w, 202, m.newOp("wake", mm.id))
				return
			case "start":
				if mm.state != "stopped" && mm.state != "suspended" {
					m.err(w, 409, "operation_conflict", "not stopped", false, 0)
					return
				}
				mm.state = "running"
				mm.autoSusp = nil
				mm.cooldown = nil
				mm.runStart = time.Now()
				mm.hasRun = true
				writeJSON(w, 202, m.newOp("start", mm.id))
				return
			case "stop":
				mm.state = "stopped"
				mm.hasRun = false
				mm.autoSusp = nil
				writeJSON(w, 202, m.newOp("stop", mm.id))
				return
			}
		}
	case len(parts) == 3 && parts[0] == "v1" && parts[1] == "operations":
		if typ, ok := m.ops[parts[2]]; ok {
			writeJSON(w, 200, map[string]any{
				"id": parts[2], "state": "succeeded", "type": typ,
				"trigger": "request",
				"runtime": map[string]any{"processesPreserved": true, "ready": true},
			})
			return
		}
		m.err(w, 404, "not_found", "no such operation", false, 0)
		return
	}
	m.err(w, 404, "not_found", fmt.Sprintf("mock has no route %s %s", method, path), false, 0)
}

// startMock runs the mock API on a random localhost port and returns its base
// URL and a shutdown function. In -mock mode the client is pointed at it, so
// every code path (client, ops, loop, web) runs exactly as against the real API.
func startMock() (string, func()) {
	api := newMockAPI()
	srv := httptest.NewServer(api)
	return srv.URL, srv.Close
}
