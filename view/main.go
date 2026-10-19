// Command lazyrex-view renders one plugin route in a Rex pane (a lab pane, or a real pane with -real-server).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
)

func main() {
	id := flag.String("view-id", "", "the durable view ID assigned by the hub")
	real := flag.Bool("real-server", false, "run on the user's real Rex server (the hub sets it on approved runs only)")
	transcriptFlag := flag.Bool("transcript", false, "print the thread history into the pane's scrollback (scrollback prototype)")
	fixture := flag.String("fixture", "", "demo thread file (JSON Lines); runs without a hub")
	styleName := flag.String("style", "11", "transcript style: a, c, d (first demo styles), or a gallery style number with light and dark palettes (default 11, the maintainer's pick)")
	window := flag.Int("window", 200, "fixture mode: the number of latest messages in the history window")
	gallery := flag.Bool("gallery", false, "fixture transcript: cycle the gallery message styles with s and S")
	every := flag.Duration("every", 5*time.Second, "fixture mode: add an agent message at this interval (0 turns it off)")
	flag.Parse()
	style, err := parseTranscriptStyle(*styleName)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *fixture != "" {
		runFixture(ctx, *transcriptFlag, *gallery, *fixture, style, *window, *every)
		return
	}
	if *id == "" || flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "Usage: lazyrex-view -view-id <id> [-real-server] [-transcript] <plugin> <route>")
		fmt.Fprintln(os.Stderr, "       lazyrex-view -fixture <thread.jsonl> [-transcript] [-style a|c|d]")
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
	socket, err := pick(os.Getenv("REX_SERVER"))
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
		reload, err := runTranscript(ctx, transcriptOptions{style: style, client: client})
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

// runFixture runs a demo pane against a thread file, with no hub and no Rex connection:
// the transcript printer, or the composer that appends sends to the file.
func runFixture(ctx context.Context, transcriptMode, gallery bool, path string, style transcriptStyle, window int, every time.Duration) {
	if transcriptMode {
		if _, err := runTranscript(ctx, transcriptOptions{style: style, gallery: gallery, thread: newFixtureThread(path, window, every)}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	client := &viewClient{ViewID: "demo-composer", Plugin: "demo", Route: "/demo/thread", fixture: true, Requests: make(chan mutation), changed: make(chan struct{}, 1), seen: make(chan struct{}, 1)}
	model := newModel(client)
	program := tea.NewProgram(model, tea.WithContext(ctx))
	client.emit = program.Send
	go runFixtureComposer(ctx, client, path)
	if _, err := program.Run(); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
