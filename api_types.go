package main

// api_types.go — types shared by the API layer: error envelope, Machine, JSON
// helpers. Shapes follow the published OpenAPI spec (GET /v1/openapi.json).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// APIError is a non-2xx InstaBox response. Branch on Code, never on Message.
type APIError struct {
	Status      int
	Code        string
	Message     string
	Retryable   bool
	RetryAfter  time.Duration
	OperationID string
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("instabox API %d %s: %s", e.Status, e.Code, e.Message)
	if e.RetryAfter > 0 {
		s += fmt.Sprintf(" (retry after %s)", e.RetryAfter.Round(time.Second))
	}
	return s
}

// IsAuthError reports whether retrying will never help: fix the key/scope instead.
func (e *APIError) IsAuthError() bool {
	switch e.Code {
	case "missing_key", "malformed_key", "invalid_key", "expired_key", "revoked_key",
		"session_expired", "missing_scope", "permission_denied":
		return true
	}
	return e.Status == 401
}

func parseAPIError(status int, raw []byte, hdr http.Header) *APIError {
	e := &APIError{
		Status:  status,
		Code:    fmt.Sprintf("http_%d", status),
		Message: strings.TrimSpace(string(raw)),
	}
	var env struct {
		Error struct {
			Code              string  `json:"code"`
			Message           string  `json:"message"`
			Retryable         bool    `json:"retryable"`
			RetryAfterSeconds *int64  `json:"retryAfterSeconds"`
			OperationID       *string `json:"operationId"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Error.Code != "" {
		e.Code = env.Error.Code
		e.Message = env.Error.Message
		e.Retryable = env.Error.Retryable
		if env.Error.RetryAfterSeconds != nil {
			e.RetryAfter = time.Duration(*env.Error.RetryAfterSeconds) * time.Second
		}
		if env.Error.OperationID != nil {
			e.OperationID = *env.Error.OperationID
		}
	}
	if e.RetryAfter == 0 {
		if v := hdr.Get("Retry-After"); v != "" {
			if s, err := strconv.ParseInt(v, 10, 64); err == nil && s > 0 {
				e.RetryAfter = time.Duration(s) * time.Second
			}
		}
	}
	return e
}

// ---------- small JSON helpers (responses are parsed defensively) ----------

func strField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			return s
		}
	}
	return ""
}

func boolField(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}

func numField(m map[string]any, key string) (float64, bool) {
	switch v := m[key].(type) {
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

func timeField(m map[string]any, key string) (time.Time, bool) {
	s, ok := m[key].(string)
	if !ok || s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// ---------- Machine ----------

// Machine holds the fields the keep-alive cares about.
// MachineState: creating|starting|running|stopping|stopped|suspending|
// suspended|resuming|deleting|deleted|error.
type Machine struct {
	ID              string
	Name            string
	State           string
	Free            bool
	RunStartedAt    time.Time // start of the current continuous run
	HasRunStart     bool
	CooldownUntil   time.Time // all wake paths refused until this deadline
	HasCooldown     bool
	AutoSuspendedAt time.Time // set when the machine paused itself (idle / max-run)
	HasAutoSuspend  bool
	MaxRunMinutes   int64 // -1 unlimited, 0 sleeps immediately; minutes from runStartedAt
	HasMaxRun       bool
	CooldownReason  string // abuse_* while an abuse lock holds the pause
	AbuseLock       bool
	OperationID     string
}

func ParseMachine(raw map[string]any) *Machine {
	m := &Machine{
		ID:             strField(raw, "id"),
		Name:           strField(raw, "name"),
		State:          strField(raw, "state"),
		Free:           boolField(raw, "free"),
		CooldownReason: strField(raw, "cooldownReason"),
		OperationID:    strField(raw, "operationId"),
	}
	if t, ok := timeField(raw, "runStartedAt"); ok {
		m.RunStartedAt, m.HasRunStart = t, true
	}
	if t, ok := timeField(raw, "cooldownUntil"); ok {
		m.CooldownUntil, m.HasCooldown = t, true
	}
	if t, ok := timeField(raw, "autoSuspendedAt"); ok {
		m.AutoSuspendedAt, m.HasAutoSuspend = t, true
	}
	if v, ok := numField(raw, "maxRunMinutes"); ok {
		m.MaxRunMinutes, m.HasMaxRun = int64(v), true
	}
	if al, ok := raw["abuseLock"].(map[string]any); ok && len(al) > 0 {
		m.AbuseLock = true
	}
	return m
}

// RunAge is how long the current run has been going, when known.
func (m *Machine) RunAge(now time.Time) (time.Duration, bool) {
	if !m.HasRunStart {
		return 0, false
	}
	d := now.Sub(m.RunStartedAt)
	if d < 0 {
		d = 0
	}
	return d, true
}

// RunDeadline is when max-run will pause this run (-1/0 or unknown => zero, false).
func (m *Machine) RunDeadline() (time.Time, bool) {
	if !m.HasRunStart || !m.HasMaxRun || m.MaxRunMinutes <= 0 {
		return time.Time{}, false
	}
	return m.RunStartedAt.Add(time.Duration(m.MaxRunMinutes) * time.Minute), true
}
