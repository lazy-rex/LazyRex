package main

import (
	"context"
	"errors"

	"github.com/lazy-rex/LazyRex/hub/internal/rexreal"
)

// checkServer applies the hard rule (AGENTS.md): never experiment on the user's real Rex server.
// The one approved exception, the official hub run, says so with -real-server. In test mode the
// hub asks no server: checkLabProof (a positive proof of a lab) follows, so no test run reaches
// the real server.
func checkServer(ctx context.Context, server string, realServer, test bool) error {
	if test {
		if rexreal.KnownDefault(server) {
			return errors.New("-test refuses the real Rex server")
		}
		return nil
	}
	if isRealServer(ctx, server) != realServer {
		if realServer {
			return errors.New("-real-server is set, but the config does not name the real Rex server")
		}
		return errors.New("refusing the user's real Rex server without -real-server (or no proof that the server is another one); use a lab server (lab/server.sh)")
	}
	return nil
}

// isRealServer reports whether server s is the user's real Rex server (rexreal.IsReal: a known
// default path, or Rex's own default endpoint; each uncertain answer is "real").
func isRealServer(ctx context.Context, s string) bool { return rexreal.IsReal(ctx, s) }
