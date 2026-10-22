// Package rexreal tells whether a Rex server is the user's real one: the default server of the rex
// CLI on PATH. Hard rule (AGENTS.md): never experiment on the real server, so each uncertain answer
// is "real" and the caller refuses. cmd/hub and cmd/mcp use it; view/ is another module and keeps
// a copy of KnownDefault with the same test cases (view/wire.go).
package rexreal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// KnownDefault reports whether endpoint is a known default socket of Rex, with no question to Rex:
//   - macOS: ~/Library/Application Support/rex/server.sock;
//   - Linux: $XDG_RUNTIME_DIR/rex/server.sock, and /run/user/<uid>/rex/server.sock, the path that
//     Rex uses when XDG_RUNTIME_DIR is unset (observed on Rex 0.0.1792593189-g3f4a0a, 2026-10-22).
//
// The paths of both systems count on either host. False means only "not a known default path".
func KnownDefault(endpoint string) bool {
	home, _ := os.UserHomeDir()
	return knownDefault(endpoint, home, os.Getenv("XDG_RUNTIME_DIR"), os.Getuid())
}

func knownDefault(endpoint, home, xdg string, uid int) bool {
	p, ok := socketPath(endpoint)
	if !ok {
		return false
	}
	var candidates []string
	if filepath.IsAbs(home) {
		candidates = append(candidates, filepath.Join(home, "Library", "Application Support", "rex", "server.sock"))
	}
	if filepath.IsAbs(xdg) { // an unset or relative XDG_RUNTIME_DIR makes no candidate
		candidates = append(candidates, filepath.Join(xdg, "rex", "server.sock"))
	}
	candidates = append(candidates, filepath.Join("/run/user", strconv.Itoa(uid), "rex", "server.sock"))
	for _, c := range candidates {
		if resolve(c) == p {
			return true
		}
	}
	// The same paths of another home or uid: a guard never treats them as a lab.
	if strings.HasSuffix(p, "/Library/Application Support/rex/server.sock") {
		return true
	}
	if rest, ok := strings.CutPrefix(p, "/run/user/"); ok {
		u, tail, _ := strings.Cut(rest, "/")
		if _, err := strconv.Atoi(u); err == nil && tail == "rex/server.sock" {
			return true
		}
	}
	return false
}

// socketPath is the absolute socket path of a unix:// endpoint: unescaped, cleaned, and with
// symbolic links resolved where they exist.
func socketPath(endpoint string) (string, bool) {
	p, ok := strings.CutPrefix(endpoint, "unix://")
	if !ok {
		return "", false
	}
	p, err := url.PathUnescape(p)
	if err != nil || !filepath.IsAbs(p) {
		return "", false
	}
	return resolve(p), true
}

func resolve(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	// The socket may not exist (a stopped server): resolve its directory.
	if r, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(r, filepath.Base(p))
	}
	return p
}

// SameEndpoint compares two unix:// endpoints by their socket paths.
func SameEndpoint(a, b string) bool {
	pa, oka := socketPath(a)
	pb, okb := socketPath(b)
	return oka && okb && pa == pb
}

// IsReal reports whether server s is the user's real Rex server.
//
//   - A known default path is real, with no question to Rex.
//   - Rex names its default endpoint (server status, without REX_SERVER and -S). The same
//     endpoint after normalization is real.
//   - A default answer without an instance_id or an endpoint is real.
//   - Otherwise, if s answers, its instance_id must differ from the default server's. If the rex
//     call for s fails, s is another path with no server: not real. An answer that is not status
//     JSON is real.
//   - No default server runs ("rex server is not running"): a server that answers is another one.
//     Any other failure of the default query is real.
//
// Observed on Rex 0.0.1792158932-g88fd41: with no default server, server status exits 1 with
// "Error: rex server is not running".
func IsReal(s string) bool {
	if KnownDefault(s) {
		return true
	}
	def, err := status("")
	if err != nil {
		if !strings.Contains(err.Error(), "rex server is not running") {
			return true
		}
		_, err := status(s)
		return err != nil
	}
	if def.InstanceID == "" || def.Endpoint == "" || SameEndpoint(def.Endpoint, s) {
		return true // a default answer without its identity proves nothing (Codex review of a87d0ee)
	}
	cfg, err := status(s)
	if err != nil {
		var ce *cliError
		return !errors.As(err, &ce) // only a failed rex call is an absent server; bad JSON is uncertain
	}
	return cfg.InstanceID == "" || cfg.InstanceID == def.InstanceID
}

// DefaultEndpoint is the endpoint of Rex's default server, for a caller without a server (a Linux
// pane has no REX_SERVER). An answer without an endpoint or an instance_id is an error.
func DefaultEndpoint() (string, error) {
	def, err := status("")
	if err != nil {
		return "", err
	}
	if _, ok := socketPath(def.Endpoint); !ok || def.InstanceID == "" {
		return "", errors.New("rex server status names no default endpoint")
	}
	return def.Endpoint, nil
}

type serverStatus struct {
	Endpoint   string `json:"endpoint"`
	InstanceID string `json:"instance_id"`
}

// status runs "rex server status --json" for server s, or for the default server when s is "".
// REX_SERVER is removed, so the default is the CLI's own; no server starts.
func status(s string) (serverStatus, error) {
	var st serverStatus
	args := []string{"--autostart=false", "--timeout", "5s"}
	if s != "" {
		args = append(args, "-S", s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rex", append(args, "server", "status", "--json")...)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "REX_SERVER=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	out, err := cmd.Output()
	if err != nil {
		return st, &cliError{err: err, stderr: strings.TrimSpace(errOut.String())}
	}
	return st, json.Unmarshal(out, &st)
}

type cliError struct {
	err    error
	stderr string
}

func (e *cliError) Error() string { return e.err.Error() + ": " + e.stderr }
