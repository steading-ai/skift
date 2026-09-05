// Package client is the Connect client behind dg and dg mcp: one set of
// generated service clients over a transport chosen by configuration. Which
// transport a call uses is a setting, never a code path, so the CLI and the
// MCP server reach the service through identical code. See "One binary,
// three faces" in docs/architecture.md.
package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Environment variables the client reads. Each overrides the config file
// and is overridden by an explicit flag.
const (
	// EnvConfig is the path of the config file, replacing [DefaultConfigPath].
	EnvConfig = "DG_CONFIG"
	// EnvSocket is the path of the service's unix socket.
	EnvSocket = "DG_SOCKET"
	// EnvAddress is the URL of the service over the tailnet or HTTPS.
	EnvAddress = "DG_ADDRESS"
	// EnvProject is the project a bare "#7" reference resolves in.
	EnvProject = "DG_PROJECT"
)

// Config is where the client connects and what it assumes when a command
// does not say. Address wins over Socket when both are set: a laptop on the
// tailnet configures an address, a process on the service's host configures
// nothing and gets the default socket.
type Config struct {
	// Socket is the path of the service's unix socket. Used when Address
	// is empty. Empty means [DefaultSocketPath].
	Socket string `json:"socket,omitempty"`
	// Address is the base URL of the service reached over the network: a
	// tailnet name such as "http://dagger.tail1234.ts.net" or an HTTPS
	// origin.
	Address string `json:"address,omitempty"`
	// Project is the default project for commands that take one.
	Project string `json:"project,omitempty"`
}

// DefaultSocketPath is the socket "dg serve" listens on by default: under
// the invoking user's runtime directory when there is one, and the system
// runtime directory otherwise, where a systemd RuntimeDirectory= puts it.
func DefaultSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "dagger", "dg.sock")
	}
	return "/run/dagger/dg.sock"
}

// DefaultConfigPath is $XDG_CONFIG_HOME/dagger/config.json, with the XDG
// fallback of ~/.config when the variable is unset. Empty when there is no
// home directory to fall back to.
func DefaultConfigPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "dagger", "config.json")
}

// LoadConfig reads the config file at path, then lays the environment over
// it. An empty path means [DefaultConfigPath], or the path in $DG_CONFIG,
// and is allowed to be absent; a path given explicitly must exist. Values
// the caller has from flags go on top with [Config.Override].
func LoadConfig(path string) (Config, error) {
	explicit := path != ""
	if !explicit {
		if env := os.Getenv(EnvConfig); env != "" {
			path, explicit = env, true
		} else {
			path = DefaultConfigPath()
		}
	}

	var cfg Config
	if path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist) && !explicit:
			// Nothing configured; every value takes its default.
		case err != nil:
			return Config{}, fmt.Errorf("client: read config %s: %w", path, err)
		default:
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return Config{}, fmt.Errorf("client: parse config %s: %w", path, err)
			}
		}
	}

	cfg = cfg.Override(Config{
		Socket:  os.Getenv(EnvSocket),
		Address: os.Getenv(EnvAddress),
		Project: os.Getenv(EnvProject),
	})
	return cfg, nil
}

// Override returns c with every non-empty field of over replacing its own.
func (c Config) Override(over Config) Config {
	if over.Socket != "" {
		c.Socket = over.Socket
	}
	if over.Address != "" {
		c.Address = over.Address
	}
	if over.Project != "" {
		c.Project = over.Project
	}
	return c
}
