package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// checkServer applies the hard rule (AGENTS.md): never experiment on the user's real Rex server.
// The one approved exception, the official hub run, says so with -real-server. In test mode the
// hub asks no server: checkLabProof (a positive proof of a lab) follows, so no test run reaches
// the real server.
func checkServer(server string, realServer, test bool) error {
	if test {
		if macRealServer(server) {
			return errors.New("-test refuses the real Rex server")
		}
		return nil
	}
	if isRealServer(server) != realServer {
		if realServer {
			return errors.New("-real-server is set, but the config does not name the real Rex server")
		}
		return errors.New("refusing the user's real Rex server without -real-server (or no proof that the server is another one); use a lab server (lab/server.sh)")
	}
	return nil
}

// macRealServer reports whether s is the macOS path of the default server.
func macRealServer(s string) bool {
	return strings.Contains(s, "Application%20Support/rex/server.sock") || strings.Contains(s, "Application Support/rex/server.sock")
}

// isRealServer reports whether server s is the user's real Rex server: the default server of
// the rex CLI on PATH. Hard rule: never experiment on the real server, so each uncertain answer
// is "real" and the caller refuses.
//
//   - The macOS path of the default server is real, with no question to Rex.
//   - Rex names its default endpoint (server status, without REX_SERVER and -S). The same
//     endpoint after normalization is real.
//   - Otherwise, if s answers, its instance_id must differ from the default server's. If s does
//     not answer, it is another path with no server: not real.
//   - No default server runs ("rex server is not running"): a server that answers is another one.
//     Any other failure of the default query is real.
//
// Observed on Rex 0.0.1792158932-g88fd41: with no default server, server status exits 1 with
// "Error: rex server is not running".
func isRealServer(s string) bool {
	if macRealServer(s) {
		return true
	}
	def, err := rexStatus("")
	if err != nil {
		if !strings.Contains(err.Error(), "rex server is not running") {
			return true
		}
		_, err := rexStatus(s)
		return err != nil
	}
	if def.Endpoint != "" && sameEndpoint(def.Endpoint, s) {
		return true
	}
	cfg, err := rexStatus(s)
	if err != nil {
		return false
	}
	return def.InstanceID == "" || cfg.InstanceID == "" || cfg.InstanceID == def.InstanceID
}

type rexServerStatus struct {
	Endpoint   string `json:"endpoint"`
	InstanceID string `json:"instance_id"`
}

// rexStatus runs "rex server status --json" for server s, or for the default server when s is
// "". REX_SERVER is removed, so the default is the CLI's own; no server starts.
func rexStatus(s string) (rexServerStatus, error) {
	var st rexServerStatus
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
		return st, &rexCLIError{err: err, stderr: strings.TrimSpace(errOut.String())}
	}
	return st, json.Unmarshal(out, &st)
}

type rexCLIError struct {
	err    error
	stderr string
}

func (e *rexCLIError) Error() string { return e.err.Error() + ": " + e.stderr }

// sameEndpoint compares two unix:// endpoints by their cleaned, unescaped socket paths, with
// symbolic links resolved where they exist (for example /tmp and /private/tmp on macOS).
func sameEndpoint(a, b string) bool {
	pa, oka := endpointPath(a)
	pb, okb := endpointPath(b)
	return oka && okb && pa == pb
}

func endpointPath(e string) (string, bool) {
	p, ok := strings.CutPrefix(e, "unix://")
	if !ok {
		return "", false
	}
	if u, err := url.PathUnescape(p); err == nil {
		p = u
	}
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p, true
}
