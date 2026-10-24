// Command lazyrex-view renders one plugin route in a Rex pane (a lab pane, or a real pane with -real-server).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	tea "charm.land/bubbletea/v2"
)

func main() {
	id := flag.String("view-id", "", "the durable view ID assigned by the hub")
	server := flag.String("server", "", "the Rex server, which the hub passes (required)")
	real := flag.Bool("real-server", false, "run on the user's real Rex server (the hub sets it on approved runs only)")
	showVersion := flag.Bool("version", false, "print the release version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *id == "" || flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "Usage: lazyrex-view -view-id <id> [-server unix://...] [-real-server] <plugin> <route>")
		os.Exit(2)
	}
	if os.Getenv("REX_SESSION") == "" || os.Getenv("REX_BLOCK") == "" {
		fmt.Fprintln(os.Stderr, "Run the renderer in a Rex pane that the hub opened")
		os.Exit(2)
	}
	pick := labSocket
	if *real {
		pick = realSocket
	}
	endpoint, err := serverEndpoint(*server, os.Getenv("REX_SERVER"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	socket, err := pick(endpoint)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	client := &viewClient{Socket: socket, Session: os.Getenv("REX_SESSION"), ViewID: *id, Plugin: flag.Arg(0), Route: flag.Arg(1), Requests: make(chan mutation), changed: make(chan struct{}, 1), seen: make(chan struct{}, 1)}
	// The renderer's pane output goes through one boundary (output.go). A write to a closed output
	// returns EPIPE and takes the failure path, instead of a SIGPIPE that kills the process.
	signal.Ignore(syscall.SIGPIPE)
	out := newPaneFile(os.Stdout)
	out.hideCursor = true
	// Every renderer process clears its pane once, before Bubble Tea writes anything
	// (lead-tv-clear2): a renderer restarted in the same pane would otherwise print the thread
	// history a second time below the old copy. A failed clear is an output failure: exit 1 before
	// the program starts or attaches. No draft exists yet, and stderr is the same pane.
	if _, err := out.WriteString(paneStart); err != nil {
		os.Exit(1)
	}
	client.outFailed = out.Failed
	out.onFault = client.closeSeen
	model := newModel(client)
	model.print.out = out
	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithOutput(out))
	client.emit = program.Send
	go func() {
		select {
		case <-out.fault:
			program.Send(outputFailed{})
		case <-ctx.Done():
		}
	}()
	go client.run(ctx)
	_, err = program.Run()
	code := afterRun(model, out.paneOut, err, ctx.Err() != nil)
	if code == 0 && model.reloadReady && ctx.Err() == nil {
		if err := syscall.Exec(executable, os.Args, os.Environ()); err != nil {
			fmt.Fprintln(os.Stderr, "Cannot reload renderer:", err)
			os.Exit(1)
		}
	}
	if code != 0 {
		os.Exit(code)
	}
}

// afterRun ends a renderer process after Run and returns its exit status. A failed output, also
// one that only the final flush found, gets the bounded draft save once (print.go) and status 1,
// with nothing more written: stderr is the same pane. Otherwise a closed view clears its status
// (a reload keeps it: the new renderer reports again), and a Run error is status 1.
func afterRun(m *model, out *paneOut, runErr error, cancelled bool) int {
	if !out.Failed() && !m.reloadReady && m.programStatus != "" && m.programStatus != statusClear {
		out.WriteString(statusClear)
	}
	if out.Failed() {
		m.finishFailure()
		return 1
	}
	if runErr != nil && !cancelled {
		fmt.Fprintln(os.Stderr, runErr)
		return 1
	}
	return 0
}
