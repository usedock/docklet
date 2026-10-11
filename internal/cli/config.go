package cli

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// CloudURL is the built-in cloud context's API URL.
const CloudURL = "https://api.usedock.io"

// CloudContext is the name of the built-in context.
const CloudContext = "cloud"

// LocalContext is the built-in context that runs docks on this machine, with no account.
const LocalContext = "local"

var contextNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Context is one named control plane target.
type Context struct {
	URL    string `toml:"url,omitempty"`
	APIKey string `toml:"api_key,omitempty"`
	// Workspace is the default workspace id for dock new.
	Workspace string `toml:"workspace,omitempty"`
}

// Config is the on-disk CLI config.
type Config struct {
	Current  string             `toml:"current,omitempty"`
	Contexts map[string]Context `toml:"contexts,omitempty"`
}

// DefaultConfigPath is os.UserConfigDir()/dock/config.toml.
func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "dock", "config.toml"), nil
}

// LoadConfig reads the config; a missing file is an empty config.
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{Contexts: map[string]Context{}}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return nil, newError("config_error", fmt.Sprintf("read %s: %v", path, err))
	}
	if cfg.Contexts == nil {
		cfg.Contexts = map[string]Context{}
	}
	return cfg, nil
}

// Save writes the config atomically with mode 0600 in a 0700 directory.
func (c *Config) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return newError("config_error", err.Error())
	}
	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return newError("config_error", err.Error())
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil && !isWindows() {
		tmp.Close()
		return newError("config_error", err.Error())
	}
	if err := toml.NewEncoder(tmp).Encode(c); err != nil {
		tmp.Close()
		return newError("config_error", err.Error())
	}
	if err := tmp.Close(); err != nil {
		return newError("config_error", err.Error())
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return newError("config_error", err.Error())
	}
	return nil
}

// active returns the context in use: the configured one, else cloud when an API key or
// URL is configured (stored or in the environment), else the built-in local context.
func (c *Config) active(getenv func(string) string) string {
	if c.Current != "" {
		return c.Current
	}
	if getenv != nil && (getenv("DOCK_API_KEY") != "" || getenv("DOCK_API_URL") != "") {
		return CloudContext
	}
	if c.Contexts[CloudContext].APIKey != "" || c.Contexts[CloudContext].URL != "" {
		return CloudContext
	}
	return LocalContext
}

// lookup returns a context with the built-in cloud URL filled in.
func (c *Config) lookup(name string) (Context, bool) {
	if name == LocalContext {
		return Context{URL: LocalContext}, true
	}
	ctx, ok := c.Contexts[name]
	if name == CloudContext {
		if ctx.URL == "" {
			ctx.URL = CloudURL
		}
		return ctx, true
	}
	return ctx, ok
}

// names lists known context names, the built-ins first.
func (c *Config) names() []string {
	var rest []string
	for n := range c.Contexts {
		if n != CloudContext && n != LocalContext {
			rest = append(rest, n)
		}
	}
	sort.Strings(rest)
	return append([]string{LocalContext, CloudContext}, rest...)
}

func validateContextName(name string) error {
	if !contextNameRE.MatchString(name) {
		return newError("invalid_argument", fmt.Sprintf("invalid context name %q (letters, digits, '.', '_', '-')", name))
	}
	return nil
}

func normalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", newError("invalid_argument", fmt.Sprintf("invalid url %q (need http:// or https://)", raw))
	}
	return strings.TrimRight(u.String(), "/"), nil
}
