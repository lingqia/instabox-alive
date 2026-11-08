package main

// keys.go — API key management: multiple keys (one per workspace), persisted
// to a local JSON file (0600). Secrets never leave the server: the web API
// returns masked values only. The env key (INSTABOX_API_KEY) is an in-memory
// entry and is never written to the file.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

type apiKey struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Secret     string    `json:"secret"`     // ibk_... — 0600 file, never sent to the UI
	Persistent bool      `json:"persistent"` // false for the env key: not saved to disk
	CreatedAt  time.Time `json:"createdAt"`

	client *Client // runtime only
}

// Masked renders a display-safe form, e.g. ibk_abcd…wxyz.
func (k *apiKey) Masked() string {
	s := k.Secret
	if len(s) <= 12 {
		return "ibk_…"
	}
	return s[:8] + "…" + s[len(s)-4:]
}

type keyStore struct {
	path string
	mu   sync.Mutex
	keys []*apiKey
	seq  int
}

func newKeyStore(path, base string) *keyStore {
	ks := &keyStore{path: path}
	ks.load(base)
	return ks
}

func (ks *keyStore) load(base string) {
	raw, err := os.ReadFile(ks.path)
	if err != nil {
		return // no file yet
	}
	var list []*apiKey
	if json.Unmarshal(raw, &list) != nil {
		return
	}
	now := time.Now()
	for i := range list {
		list[i].Persistent = true
		list[i].client = NewClient(base, list[i].Secret)
		if list[i].CreatedAt.IsZero() {
			list[i].CreatedAt = now
		}
	}
	ks.keys = list
}

func (ks *keyStore) save() error {
	persist := make([]*apiKey, 0, len(ks.keys))
	for _, k := range ks.keys {
		if k.Persistent {
			persist = append(persist, k)
		}
	}
	raw, _ := json.MarshalIndent(persist, "", "  ")
	tmp := ks.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ks.path)
}

// add appends a key entry and persists it (unless it is the env key).
func (ks *keyStore) add(name, secret string, persistent bool, base string) *apiKey {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.seq++
	k := &apiKey{
		ID:         fmt.Sprintf("key%d", ks.seq),
		Name:       name,
		Secret:     secret,
		Persistent: persistent,
		CreatedAt:  time.Now(),
		client:     NewClient(base, secret),
	}
	for _, e := range ks.keys {
		if e.ID >= k.ID {
			k.ID = e.ID + "x" // keep ids unique even after deletions
		}
	}
	ks.keys = append(ks.keys, k)
	if persistent {
		_ = ks.save()
	}
	return k
}

// remove deletes a key by id. The env key cannot be removed.
func (ks *keyStore) remove(id string) bool {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	for i, e := range ks.keys {
		if e.ID == id {
			if !e.Persistent {
				return false // env key: revoke it by unsetting the env instead
			}
			ks.keys = append(ks.keys[:i], ks.keys[i+1:]...)
			_ = ks.save()
			return true
		}
	}
	return false
}

func (ks *keyStore) list() []*apiKey {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	out := make([]*apiKey, len(ks.keys))
	copy(out, ks.keys)
	return out
}

func (ks *keyStore) byID(id string) *apiKey {
	for _, k := range ks.list() {
		if k.ID == id {
			return k
		}
	}
	return nil
}

// newID makes a short random id for UI-created keys.
func newID(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// ---------- app-level key operations ----------

// discoverMachines adds this key's machines (refs: "all" or id/name list).
func (a *app) discoverMachines(ctx context.Context, k *apiKey, refs []string) {
	for _, ref := range refs {
		if ref == "all" || ref == "*" {
			list, err := k.client.ListMachines(ctx)
			if err != nil {
				a.warnf("key %s: list machines: %v", k.Name, err)
				continue
			}
			for _, raw := range list {
				id, name := strField(raw, "id"), strField(raw, "name")
				if name == "" {
					name = id
				}
				mc := &machineCtl{app: a, client: k.client, keyID: k.ID, keyName: k.Name, ref: name, id: id}
				if a.addMachineCtl(mc) {
					a.info("managing machine %s (%s) [key %s]", name, id, k.Name)
				}
			}
			continue
		}
		id, err := k.client.ResolveMachine(ctx, ref)
		if err != nil {
			a.warnf("key %s: could not resolve %q (%v); using it as an id", k.Name, ref, err)
			id = ref
		} else if id != ref {
			a.info("machine %q resolved to id %s [key %s]", ref, id, k.Name)
		}
		mc := &machineCtl{app: a, client: k.client, keyID: k.ID, keyName: k.Name, ref: ref, id: id}
		if a.addMachineCtl(mc) {
			a.info("managing machine %s (%s) [key %s]", ref, id, k.Name)
		}
	}
}

// addKey validates, stores and discovers machines for a new key (from the web UI).
func (a *app) addKey(ctx context.Context, name, secret string) (map[string]any, error) {
	secret = strings.TrimSpace(secret)
	if !strings.HasPrefix(secret, "ibk_") || len(secret) < 20 {
		return nil, fmt.Errorf("这不是有效的 API key（应为 ibk_ 开头）")
	}
	if name = strings.TrimSpace(name); name == "" {
		name = "key-" + secret[4:8]
	}
	k := a.keys.add(name, secret, true, a.cfg.apiBase)
	if _, err := k.client.ListMachines(ctx); err != nil {
		a.keys.remove(k.ID)
		return nil, fmt.Errorf("key 验证失败：%v", err)
	}
	a.discoverMachines(ctx, k, []string{"all"})
	a.info("已添加密钥 %s（%s）", name, k.Masked())
	return a.keyInfo(k), nil
}

// removeKey removes a stored key and its machines, then re-binds orphans.
func (a *app) removeKey(id string) error {
	k := a.keys.byID(id)
	if k == nil {
		return fmt.Errorf("没有这个密钥")
	}
	if !k.Persistent {
		return fmt.Errorf("环境变量里的 key 不能在这里删除；取消 INSTABOX_API_KEY 后重启即可")
	}
	a.keys.remove(id)
	a.removeMachineCtls(func(mc *machineCtl) bool { return mc.keyID == id })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, other := range a.keys.list() {
		a.discoverMachines(ctx, other, []string{"all"})
	}
	a.info("已删除密钥 %s", k.Name)
	return nil
}

func (a *app) keyInfo(k *apiKey) map[string]any {
	count := 0
	for _, mc := range a.machinesSnapshot() {
		if mc.keyID == k.ID {
			count++
		}
	}
	return map[string]any{
		"id": k.ID, "name": k.Name, "masked": k.Masked(),
		"persistent": k.Persistent, "createdAt": k.CreatedAt, "machineCount": count,
	}
}

func (a *app) keyInfos() []map[string]any {
	list := a.keys.list()
	out := make([]map[string]any, 0, len(list))
	for _, k := range list {
		out = append(out, a.keyInfo(k))
	}
	return out
}
