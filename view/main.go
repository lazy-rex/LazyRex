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
	server := flag.String("server", "", "the Rex server, which the hub passes (default: $REX_SERVER)")
	real := flag.Bool("real-server", false, "run on the user's real Rex server (the hub sets it on approved runs only)")
	transcriptFlag := flag.Bool("transcript", false, "print the thread history into the pane's scrollback (scrollback prototype)")
	showVersion := flag.Bool("version", false, "print the release version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *id == "" || flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "Usage: lazyrex-view -view-id <id> [-server unix://...] [-real-server] [-transcript] <plugin> <route>")
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
	if *transcriptFlag {
		client.Role = "transcript"
		reload, err := runTranscript(ctx, client)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if reload && ctx.Err() == nil {
			if err := syscall.Exec(executable, os.Args, os.Environ()); err != nil {
				fmt.Fprintln(os.Stderr, "Cannot reload transcript:", err)
				os.Exit(1)
			}
		}
		return
	}
	model := newModel(client)
	program := tea.NewProgram(model, tea.WithContext(ctx))
	client.emit = program.Send
	go client.run(ctx)
	if _, err = program.Run(); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// A closed view leaves no status behind. A reload keeps it: the new renderer reports again.
	if !model.reloadReady && model.programStatus != "" && model.programStatus != statusClear {
		os.Stdout.WriteString(statusClear)
	}
	if model.reloadReady && ctx.Err() == nil {
		if err := syscall.Exec(executable, os.Args, os.Environ()); err != nil {
			fmt.Fprintln(os.Stderr, "Cannot reload renderer:", err)
			os.Exit(1)
		}
	}
}
