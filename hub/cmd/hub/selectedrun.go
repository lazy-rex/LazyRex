package main

// The selected run (the P1 plan r6): the run that an automatic start uses. An explicit start (hub
// start recovery, hub.admin.start, a reload, a replace) resolves the current pin and selects its
// run: spawn saves it with the run, and a start that waits saves it before its lease ends. An
// automatic start (a crash respawn, a restart after a session loss, a wake) never reads a pin: it
// runs the selected run, so a lock change takes effect at the next explicit start.
//
// The lifetime: a crash, a failed spawn, a retirement and a session loss keep it; a stop
// (markStopped), the entry's removal and hub start (loadKept) clear it.

// selectedRun is the plugins row selected_run: the run, and the entry that selected it
// (entryIdentity). An automatic start under another entry does not use it.
type selectedRun struct {
	Entry string    `json:"entry"`
	Run   pluginRun `json:"run"`
}
