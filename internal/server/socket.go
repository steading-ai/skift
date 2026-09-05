package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// listenUnix binds cfg.SocketPath and sets its mode and group.
//
// The socket is created with the process umask, then tightened. The parent
// directory is what guards the gap: under systemd it is a RuntimeDirectory
// with its own mode, and a caller that can traverse it could reach the
// socket in any case.
func listenUnix(cfg Config) (net.Listener, error) {
	path := cfg.SocketPath
	if path == "" {
		return nil, errors.New("server: socket path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("server: create socket directory: %w", err)
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}

	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("server: listen: %w", err)
	}
	if err := restrictSocket(path, cfg); err != nil {
		return nil, errors.Join(err, l.Close())
	}
	return l, nil
}

// removeStaleSocket clears a socket left by a previous process. One that
// still answers belongs to a running server and is left alone.
func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("server: inspect socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("server: %s exists and is not a socket", path)
	}
	if conn, err := net.Dial("unix", path); err == nil {
		_ = conn.Close()
		return fmt.Errorf("server: %s is in use by another server", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("server: remove stale socket: %w", err)
	}
	return nil
}

func restrictSocket(path string, cfg Config) error {
	if cfg.SocketGroup != "" {
		gid, err := lookupGID(cfg.SocketGroup)
		if err != nil {
			return err
		}
		if err := os.Chown(path, -1, gid); err != nil {
			return fmt.Errorf("server: chown socket to group %q: %w", cfg.SocketGroup, err)
		}
	}
	if err := os.Chmod(path, cfg.SocketMode.Perm()); err != nil {
		return fmt.Errorf("server: chmod socket: %w", err)
	}
	return nil
}

// lookupGID resolves a group given by name or numeric id.
func lookupGID(group string) (int, error) {
	if gid, err := strconv.Atoi(group); err == nil {
		return gid, nil
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return 0, fmt.Errorf("server: resolve socket group: %w", err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return 0, fmt.Errorf("server: group %q has non-numeric id %q", group, g.Gid)
	}
	return gid, nil
}

// SocketOwner reports the mode and group of the socket at path, for
// operators and specs checking what a running server exposed.
func SocketOwner(path string) (mode os.FileMode, gid int, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("server: no ownership information for %s", path)
	}
	return info.Mode().Perm(), int(stat.Gid), nil
}
