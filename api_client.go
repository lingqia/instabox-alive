package main

// api_client.go — HTTP core for the InstaBox REST API (https://api.instabox.sh).
// Stdlib only. All bodies are JSON; errors carry a machine-readable `error.code`.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	base string
	key  string
	http *http.Client
}

func NewClient(base, key string) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"),
		key:  key,
		http: &http.Client{Timeout: 90 * time.Second},
	}
}

func newIdemKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "keepalive-" + hex.EncodeToString(b)
}

// do performs a JSON request. idempotent adds a fresh Idempotency-Key header
// (required on lifecycle calls: suspend/resume/start/stop/delete).
func (c *Client) do(ctx context.Context, method, path string, body any, idempotent bool) (map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotent {
		req.Header.Set("Idempotency-Key", newIdemKey())
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, parseAPIError(resp.StatusCode, raw, resp.Header)
	}
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode response: %w (body %.200s)", err, raw)
	}
	return m, nil
}
