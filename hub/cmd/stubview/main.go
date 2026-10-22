// Command stubview stands in for the renderer (lazyrex-view) in hub lab tests. It checks in
// with hub.view.attach from its pane, prints what it got, and waits. -exit-after makes it exit,
// to test renderer crash handling.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/lazy-rex/LazyRex/hub/internal/rexreal"
	"github.com/lazy-rex/LazyRex/hub/rex"
)

func main() {
	viewID := flag.String("view-id", "", "view ID from the hub")
	exitAfter := flag.Duration("exit-after", 0, "exit after this time (lab crash test)")
	// The hub passes its endpoint as to the renderer (viewCommand); it must be the pane's REX_SERVER.
	server := flag.String("server", "", "the hub's Rex server; it must equal REX_SERVER")
	flag.Parse()
	if *viewID == "" || flag.NArg() != 2 {
		log.Fatal("usage: stubview -view-id <id> <plugin> <route>")
	}
	if *server != "" && !rexreal.SameEndpoint(*server, os.Getenv("REX_SERVER")) {
		log.Fatalf("-server %s is not REX_SERVER %s", *server, os.Getenv("REX_SERVER"))
	}
	ctx := context.Background()
	socket, err := rex.SocketPath(os.Getenv("REX_SERVER"))
	if err != nil {
		log.Fatal(err)
	}
	r, err := rex.Dial(ctx, socket)
	if err != nil {
		log.Fatal(err)
	}
	hub, err := resolveHub(ctx, r)
	if err != nil {
		log.Fatal(err)
	}
	var out struct {
		Result map[string]any `json:"result"`
	}
	// The hub answers not_ready until it has recorded this pane.
	for i := 0; ; i++ {
		err = r.Call(ctx, "client.perform", map[string]any{"client_id": hub, "action": "hub.view.attach",
			"args": map[string]any{"view_id": *viewID, "schema_version": 1}, "timeout_ms": 10000}, &out)
		if err == nil {
			break
		}
		if i == 20 || !strings.Contains(err.Error(), "not_ready") {
			log.Fatalf("attach: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("stubview %s %s %s\nattached: %v\n", *viewID, flag.Arg(0), flag.Arg(1), out.Result)
	if *exitAfter > 0 {
		time.Sleep(*exitAfter)
		os.Exit(3)
	}
	select {}
}

// resolveHub finds the one connected client labeled lazyrex-hub. A stopped hub stays listed
// as lost during Rex's reconnect grace period, so only connected candidates count.
func resolveHub(ctx context.Context, r *rex.Rex) (string, error) {
	type target struct {
		ClientID string `json:"client_id"`
	}
	var res struct {
		Results []struct {
			Status     string   `json:"status"`
			Target     target   `json:"target"`
			Candidates []target `json:"candidates"`
		} `json:"results"`
	}
	if err := r.Call(ctx, "target.resolve", map[string]any{"kinds": []string{"client"}, "queries": []string{"lazyrex-hub"}}, &res); err != nil {
		return "", err
	}
	if len(res.Results) != 1 {
		return "", fmt.Errorf("resolve hub: no result")
	}
	candidates := res.Results[0].Candidates
	if res.Results[0].Status == "resolved" {
		candidates = []target{res.Results[0].Target}
	}
	var connected []string
	for _, c := range candidates {
		var ins struct {
			Client struct {
				ConnectionState string `json:"connection_state"`
			} `json:"client"`
		}
		if r.Call(ctx, "client.inspect", map[string]any{"client_id": c.ClientID}, &ins) == nil && ins.Client.ConnectionState == "connected" {
			connected = append(connected, c.ClientID)
		}
	}
	if len(connected) != 1 {
		return "", fmt.Errorf("resolve hub: %d connected of %d", len(connected), len(candidates))
	}
	return connected[0], nil
}
