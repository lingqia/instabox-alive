package main

// api_ops.go — the endpoints the keep-alive uses, one method each.
// Reference: https://instabox.sh/docs/api-reference/

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ListMachines returns every machine of the workspace (all pages).
func (c *Client) ListMachines(ctx context.Context) ([]map[string]any, error) {
	var out []map[string]any
	cursor := ""
	for page := 0; page < 20; page++ {
		path := "/v1/machines"
		if cursor != "" {
			path += "?cursor=" + url.QueryEscape(cursor)
		}
		m, err := c.do(ctx, http.MethodGet, path, nil, false)
		if err != nil {
			return nil, err
		}
		list, _ := m["machines"].([]any)
		for _, it := range list {
			if raw, ok := it.(map[string]any); ok {
				out = append(out, raw)
			}
		}
		next, _ := m["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	return out, nil
}

// ResolveMachine turns an id or exact name into a machine id.
func (c *Client) ResolveMachine(ctx context.Context, ref string) (string, error) {
	list, err := c.ListMachines(ctx)
	if err != nil {
		return "", err
	}
	matches, id := 0, ""
	for _, raw := range list {
		if strField(raw, "id") == ref || strField(raw, "name") == ref {
			matches++
			id = strField(raw, "id")
		}
	}
	if matches == 1 {
		return id, nil
	}
	if matches > 1 {
		return "", fmt.Errorf("%q matches %d machines; use the machine id", ref, matches)
	}
	return "", fmt.Errorf("no machine with id or name %q in this workspace", ref)
}

// GetMachine reads the machine record (state, runStartedAt, maxRunMinutes,
// cooldownUntil, autoSuspendedAt, abuseLock, ...).
func (c *Client) GetMachine(ctx context.Context, id string) (*Machine, error) {
	raw, err := c.do(ctx, http.MethodGet, "/v1/machines/"+url.PathEscape(id), nil, false)
	if err != nil {
		return nil, err
	}
	return ParseMachine(raw), nil
}

// RenewLease holds an activity lease: a lease in progress keeps the machine
// busy so idle auto-pause never fires. TTL 30-3600s; renew before expiry.
// PUT /v1/machines/{id}/activity-leases/{leaseID}
func (c *Client) RenewLease(ctx context.Context, id, leaseID string, ttlSeconds int) error {
	_, err := c.do(ctx, http.MethodPut,
		"/v1/machines/"+url.PathEscape(id)+"/activity-leases/"+url.PathEscape(leaseID),
		map[string]any{"ttlSeconds": ttlSeconds}, false)
	return err
}

// ReleaseLease drops the lease. 404 is fine (already gone).
func (c *Client) ReleaseLease(ctx context.Context, id, leaseID string) error {
	_, err := c.do(ctx, http.MethodDelete,
		"/v1/machines/"+url.PathEscape(id)+"/activity-leases/"+url.PathEscape(leaseID), nil, false)
	if e, ok := err.(*APIError); ok && e.Status == 404 {
		return nil
	}
	return err
}

// lifecycle posts a lifecycle action and returns the operation id.
// These need an Idempotency-Key (fresh per call).
func (c *Client) lifecycle(ctx context.Context, id, action string, body any) (string, error) {
	m, err := c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/"+action, body, true)
	if err != nil {
		return "", err
	}
	return opID(m), nil
}

func opID(m map[string]any) string {
	if s := strField(m, "id"); s != "" {
		return s
	}
	if inner, ok := m["operation"].(map[string]any); ok {
		return strField(inner, "id")
	}
	return ""
}

// Wake resumes a machine that paused itself (autoSuspendedAt set).
// No Idempotency-Key needed. Waits up to 45s server-side for the resume.
func (c *Client) Wake(ctx context.Context, id string) (string, error) {
	m, err := c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/wake", nil, false)
	if err != nil {
		return "", err
	}
	return opID(m), nil
}

func (c *Client) Suspend(ctx context.Context, id string) (string, error) {
	return c.lifecycle(ctx, id, "suspend", map[string]any{})
}

func (c *Client) Resume(ctx context.Context, id string) (string, error) {
	return c.lifecycle(ctx, id, "resume", map[string]any{})
}

func (c *Client) Start(ctx context.Context, id string) (string, error) {
	return c.lifecycle(ctx, id, "start", map[string]any{})
}

func (c *Client) Stop(ctx context.Context, id string) (string, error) {
	return c.lifecycle(ctx, id, "stop", map[string]any{})
}

// WaitOperation follows an operation until it is terminal.
// GET /v1/operations/{id} (long poll with ?wait=25s).
func (c *Client) WaitOperation(ctx context.Context, op string) (map[string]any, error) {
	if op == "" {
		return nil, fmt.Errorf("no operation id to wait for")
	}
	for {
		m, err := c.do(ctx, http.MethodGet,
			"/v1/operations/"+url.PathEscape(op)+"?wait=25s", nil, false)
		if err != nil {
			return nil, err
		}
		switch strField(m, "state") {
		case "succeeded":
			return m, nil
		case "failed":
			msg := "operation " + op + " failed"
			if e, ok := m["error"].(map[string]any); ok {
				msg += ": " + strField(e, "code") + " " + strField(e, "message")
			}
			return m, fmt.Errorf("%s", msg)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
