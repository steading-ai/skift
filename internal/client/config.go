// Package client is the Connect client behind the skift CLI and skift mcp:
// one set of generated service clients over a transport chosen by
// configuration. Which transport a call uses is a setting, never a code
// path, so the CLI and the MCP server reach the service through identical
// code. See "One binary, three faces" in docs/architecture.md.
package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Environment variables the client reads. Each overrides the config file
// and is overridden by an explicit flag.
const (
	// EnvConfig is the path of the config file, replacing [DefaultConfigPath].
	EnvConfig = "SKIFT_CONFIG"
	// EnvSocket is the path of the service's unix socket.
	EnvSocket = "SKIFT_SOCKET"
	// EnvAddress is the URL of the service over the tailnet or HTTPS.
	EnvAddress = "SKIFT_ADDRESS"
	// EnvProject is the project a bare "#7" reference resolves in.
	EnvProject = "SKIFT_PROJECT"
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
	// tailnet name such as "http://skift.tail1234.ts.net" or an HTTPS
	// origin.
	Address string `json:"address,omitempty"`
	// Project is the default project for commands that take one.
	Project string `json:"project,omitempty"`
}

// SystemSocketPath is the system service's socket, where a systemd
// RuntimeDirectory= puts it.
const SystemSocketPath = "/run/skift/skift.sock"

// DefaultSocketPath is the socket the client dials when nothing names one:
// the invoking user's own "skift serve" at [UserSocketPath], when that
// socket exists, and the system service's otherwise. A login session on a
// host running the system service reaches it without configuration, and a
// user's own instance takes precedence when there is one.
//
// On macOS there is no system service, so the user's own socket is the
// default whether or not it exists yet.
func DefaultSocketPath() string {
	return defaultSocketPath(runtime.GOOS)
}

func defaultSocketPath(goos string) string {
	own := userSocketPath(goos)
	if own == "" {
		return SystemSocketPath
	}
	if goos == "darwin" {
		return own
	}
	if _, err := os.Stat(own); err == nil {
		return own
	}
	return SystemSocketPath
}

// UserSocketPath is where a user's own "skift serve" listens:
// skift/skift.sock under $XDG_RUNTIME_DIR. macOS sets no runtime directory,
// so there it falls back to the XDG state directory, $XDG_STATE_HOME or
// ~/.local/state. Empty when neither applies.
func UserSocketPath() string {
	return userSocketPath(runtime.GOOS)
}

func userSocketPath(goos string) string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "skift", "skift.sock")
	}
	if goos != "darwin" {
		return ""
	}
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "skift", "skift.sock")
}

// DefaultConfigPath is $XDG_CONFIG_HOME/skift/config.json, with the XDG
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
	return filepath.Join(dir, "skift", "config.json")
}

// LoadConfig reads the config file at path, then lays the environment over
// it. An empty path means [DefaultConfigPath], or the path in $SKIFT_CONFIG,
// and is allowed to be absent; a path given explicitly must exist. Values
// the caller has from flags go on top with [Config.Override].
func LoadConfig(path string) (Config, error) {
	loaded, err := Load(path, Config{})
	if err != nil {
		return Config{}, err
	}
	return loaded.Config, nil
}

// Source is where a setting's value came from.
type Source string

// The places a setting is read from, in rising precedence.
const (
	SourceDefault Source = "default"
	SourceFile    Source = "config file"
	SourceEnv     Source = "environment"
	SourceFlag    Source = "flag"
)

// Loaded is a configuration and where each part of it came from.
type Loaded struct {
	Config
	// Path is the config file consulted, whether or not it exists. Empty
	// when there is no home directory to find the default under.
	Path string
	// Found reports whether the file at Path was read.
	Found bool
	// SocketFrom and AddressFrom say which source set Socket and Address;
	// SourceDefault when none did.
	SocketFrom, AddressFrom Source
}

// Load resolves the configuration as [LoadConfig] does and lays flags over
// it as [Config.Override] does, recording the source of each connection
// setting. On an error, Path still names the file that failed.
func Load(path string, flags Config) (Loaded, error) {
	explicit := path != ""
	if !explicit {
		if env := os.Getenv(EnvConfig); env != "" {
			path, explicit = env, true
		} else {
			path = DefaultConfigPath()
		}
	}

	l := Loaded{Path: path, SocketFrom: SourceDefault, AddressFrom: SourceDefault}
	if path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist) && !explicit:
			// Nothing configured; every value takes its default.
		case err != nil:
			return l, fmt.Errorf("client: read config %s: %w", path, err)
		default:
			l.Found = true
			var file Config
			if err := json.Unmarshal(raw, &file); err != nil {
				return l, fmt.Errorf("client: parse config %s: %w", path, err)
			}
			l.layer(file, SourceFile)
		}
	}
	l.layer(Config{
		Socket:  os.Getenv(EnvSocket),
		Address: os.Getenv(EnvAddress),
		Project: os.Getenv(EnvProject),
	}, SourceEnv)
	l.layer(flags, SourceFlag)
	return l, nil
}

// layer overrides l with over, crediting each connection setting over sets
// to from.
func (l *Loaded) layer(over Config, from Source) {
	if over.Socket != "" {
		l.SocketFrom = from
	}
	if over.Address != "" {
		l.AddressFrom = from
	}
	l.Config = l.Override(over)
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
