// Package fleetauth holds the per-host credential the control plane issues at
// enrollment or first registration. Every fleet call presents it; the shared
// enrollment token is only a stand-in for a host that has none yet.
package fleetauth

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Credential is the bearer a host presents to the control plane, shared by the
// fleet agent and the object store so a rotation reaches both.
type Credential struct {
	mu     sync.Mutex
	claim  sync.Mutex
	path   string
	enroll string
	value  string
}

// Open loads a previously persisted credential from path (empty path keeps it
// in memory only). enrollToken is the shared token used to obtain a new one.
func Open(path, enrollToken string) *Credential {
	c := &Credential{path: path, enroll: enrollToken}
	if path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			c.value = strings.TrimSpace(string(raw))
		}
	}
	return c
}

// Bearer is the credential when one is held, otherwise the enrollment token
// (which an upgraded host uses until its first registration succeeds).
func (c *Credential) Bearer() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.value != "" {
		return c.value
	}
	return c.enroll
}

// Held is the per-host credential alone; empty before the control plane issues one.
func (c *Credential) Held() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

// Claim runs register once to obtain a credential after the control plane
// rejected `used`. It does nothing when a credential is held (a rejected
// credential is revoked, and the shared token must never replace it) or when
// there is no shared token. Concurrent callers are serialized, and a caller
// that lost the race just retries with the credential the winner obtained.
func (c *Credential) Claim(used string, register func() error) bool {
	c.claim.Lock()
	defer c.claim.Unlock()
	if held := c.Held(); held != "" {
		return held != used
	}
	if c.enroll == "" {
		return false
	}
	return register() == nil && c.Held() != ""
}

// Set adopts a newly issued credential and persists it with owner-only
// permissions. The in-memory value is updated even when persisting fails.
func (c *Credential) Set(value string) error {
	c.mu.Lock()
	c.value = value
	c.mu.Unlock()
	if c.path == "" {
		return nil
	}
	return writeSecret(c.path, value)
}

// writeSecret replaces path atomically so a crash never leaves a partial credential.
func writeSecret(path, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fleet-credential-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(value + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
