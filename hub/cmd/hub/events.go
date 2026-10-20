package main

// Lab events and fault injection (README "Test-hook flags and events").
// Faults are configured only by command-line flags. No Rex action can reach them.

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const faultExitCode = 86

type eventLog struct {
	mu         sync.Mutex
	f          *os.File
	generation string
}

func openEvents(path string) (*eventLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &eventLog{f: f}, nil
}

// emit appends one JSON line and fsyncs it before the hub acts on it.
func (e *eventLog) emit(event string, fields map[string]any) {
	line := map[string]any{"event": event, "generation": e.generation, "at": time.Now().UTC().Format(time.RFC3339Nano)}
	for k, v := range fields {
		line[k] = v
	}
	b, _ := json.Marshal(line)
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.f.Write(append(b, '\n')); err != nil {
		log.Printf("events: %v", err)
	}
	e.f.Sync()
	log.Printf("event %s", b)
}

type fault struct {
	point, mode, op string
	fired           bool
}

type faults struct {
	mu   sync.Mutex
	list []*fault
}

func (fs *faults) String() string { return fmt.Sprint(len(fs.list)) }

func (fs *faults) Set(spec string) error {
	f := &fault{}
	for _, kv := range strings.Split(spec, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("bad fault pair %q", kv)
		}
		switch k {
		case "point":
			f.point = v
		case "mode":
			f.mode = v
		case "op":
			f.op = v
		default:
			return fmt.Errorf("unknown fault key %q", k)
		}
	}
	valid := map[string][]string{"store-commit": {"exit", "pause"}, "block-create": {"exit-before", "exit-after"}, "notice-written": {"gate"}}
	modes, ok := valid[f.point]
	if !ok || !contains(modes, f.mode) {
		return fmt.Errorf("unsupported fault %q", spec)
	}
	if f.op != "" && f.point != "store-commit" {
		return fmt.Errorf("op= applies only to store-commit")
	}
	fs.list = append(fs.list, f)
	return nil
}

// take returns the first unfired fault at point (and mode, if not "") that matches opKey, and marks it fired.
func (fs *faults) take(point, mode, opKey string) *fault {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, f := range fs.list {
		if f.fired || f.point != point || (mode != "" && f.mode != mode) {
			continue
		}
		if point == "store-commit" && (opKey == "" || (f.op != "" && f.op != opKey)) {
			continue
		}
		f.fired = true
		return f
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
