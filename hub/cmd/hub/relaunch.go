package main

// hub.admin.relaunch (docs/hub.md, "Restart wrapper", "Relaunch"): the hub stops as for
// hub.admin.shutdown and exits 4, and its restart wrapper execs itself, so a new binary runs in the
// same wrapper process. A hub process relaunches at most once (the latch). expect_exec makes a
// request apply only to the exec count that the caller saw. After a failed exec, the wrapper starts
// the old version again at the same exec count, and that new hub accepts the same expect_exec: the
// restart command never resends, so only a new command retries.

// adminRelaunch accepts a relaunch. The admin guard and the latch check run before it (main.go).
// Once accepted, the latch refuses every later admin call until the hub exits, and the relaunch
// runs also when its reply is lost (afterReply).
func (h *Hub) adminRelaunch(args map[string]any) (any, error) {
	for k := range args {
		if k != "expect_exec" {
			return nil, refuse("invalid_input", "hub.admin.relaunch takes only expect_exec")
		}
	}
	f, ok := args["expect_exec"].(float64)
	if !ok || f < 0 || f != float64(int(f)) {
		return nil, refuse("invalid_input", "expect_exec is the exec count from hub.info")
	}
	if !h.wrapped.Load() {
		return nil, refuse("refused", "no restart wrapper; stop and start the hub by hand")
	}
	if n := int(f); n != h.exec {
		return nil, refuse("refused", "already restarted: exec is %d, expected %d", h.exec, n)
	}
	if !h.relaunching.CompareAndSwap(false, true) {
		return nil, refuse("refused", "relaunch in progress")
	}
	return map[string]any{"relaunching": true, "exec": h.exec}, nil
}
