package main

// Prompt pushes (docs/hub.md "Prompt pushes"). Each registration of a plugin with the prompts grant
// has one sender. The sender sends the CURRENT prompt state of one dirty agent at a time, with that
// agent's rev, and has at most one call outstanding. A failed or unanswered call leaves the agent
// dirty: the next call sends the state current then. So a lost reply never hides a close, and no
// event history is kept.

import (
	"context"
	"log"
	"sort"
	"time"
)

// promptSender delivers prompt state to one plugin registration (instance and client).
type promptSender struct {
	plugin, instance, client string
	dirty                    map[string]bool // agents whose state the plugin may not have
	kick                     chan struct{}
	done                     chan struct{}   // closed when the sender ends: it wakes an idle or retry wait
	backoff                  []time.Duration // promptPushBackoff when the sender started
}

// promptPushBackoff is the retry delay after the n-th failed call in a row.
var promptPushBackoff = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}

// promptPush makes the call. Tests replace it.
var promptPush = func(ctx context.Context, h *Hub, client string, args map[string]any) error {
	return h.rex.Call(ctx, "client.perform", map[string]any{"client_id": client, "action": "hub.prompt", "args": args, "timeout_ms": 3000}, nil)
}

// startPromptSender starts the sender of a new registration. The sender of an earlier
// registration stops: at once when it waits, else after its outstanding call. h.mu must be held.
func (h *Hub) startPromptSender(pl *Plugin) {
	if h.promptSenders == nil {
		h.promptSenders = map[string]*promptSender{}
	}
	s := &promptSender{plugin: pl.Name, instance: pl.InstanceID, client: pl.ClientID, dirty: map[string]bool{}, kick: make(chan struct{}, 1), done: make(chan struct{}), backoff: promptPushBackoff}
	// Every agent that had a prompt in this hub run: the plugin may hold a stale state for it.
	for agent := range h.prompts.agentRev {
		s.dirty[agent] = true
	}
	if old := h.promptSenders[pl.Name]; old != nil {
		h.stopPromptSender(old)
	}
	h.promptSenders[pl.Name] = s
	s.kick <- struct{}{}
	go h.promptSendLoop(s)
}

// stopPromptSender ends s: it leaves the map, and its loop returns at once from a wait, or after
// its outstanding call. h.mu must be held.
func (h *Hub) stopPromptSender(s *promptSender) {
	if h.promptSenders[s.plugin] == s {
		delete(h.promptSenders, s.plugin)
	}
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

// reapPromptSenders stops each sender whose registration ended (an exit, a stop, a new spawn, or
// a lost grant). changed("plugins") calls it, so an idle sender does not wait for the next prompt
// change to see that it is stale.
func (h *Hub) reapPromptSenders() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.promptSenders {
		if !h.promptSenderCurrent(s) {
			h.stopPromptSender(s)
		}
	}
}

// markPromptDirty marks an agent dirty for every sender. h.mu must be held.
func (h *Hub) markPromptDirty(agent string) {
	for _, s := range h.promptSenders {
		s.dirty[agent] = true
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
}

// current reports whether s is still the sender of the plugin's current registration. h.mu must
// be held.
func (h *Hub) promptSenderCurrent(s *promptSender) bool {
	pl := h.plugins[s.plugin]
	return h.promptSenders[s.plugin] == s && pl != nil && pl.InstanceID == s.instance && pl.ClientID == s.client && pl.promptsGranted()
}

// promptState is the push for one agent: its current prompt (or null), its rev and its last close.
// h.mu must be held.
func (h *Hub) promptState(agent string) map[string]any {
	b := &h.prompts
	args := map[string]any{"hub_generation": revString(h.generation), "rev": revString(b.agentRev[agent]), "agent": agent, "prompt": nil, "closed": nil}
	if p := b.open[agent]; p != nil {
		args["prompt"] = p.json()
	}
	if c, ok := b.last[agent]; ok {
		args["closed"] = map[string]any{"prompt_id": c.ID, "reason": c.Reason}
	}
	return args
}

func (h *Hub) promptSendLoop(s *promptSender) {
	failures := 0
	var retry <-chan time.Time
	for {
		if retry != nil {
			select { // a change during the backoff only marks its agent dirty
			case <-retry:
			case <-s.done:
				return
			}
		} else {
			select {
			case <-s.kick:
			case <-s.done:
				return
			}
		}
		retry = nil
		for {
			h.mu.Lock()
			if !h.promptSenderCurrent(s) {
				h.stopPromptSender(s) // the registration ended: no sender
				h.mu.Unlock()
				return
			}
			var agents []string
			for a := range s.dirty {
				agents = append(agents, a)
			}
			if len(agents) == 0 {
				h.mu.Unlock()
				break
			}
			sort.Strings(agents)
			agent := agents[0]
			delete(s.dirty, agent) // a change during the call marks it again
			args := h.promptState(agent)
			h.mu.Unlock()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := promptPush(ctx, h, s.client, args)
			cancel()
			if err == nil {
				failures = 0
				continue
			}
			log.Printf("hub.prompt to %s (agent %s): %v", s.plugin, agent, err)
			h.mu.Lock()
			s.dirty[agent] = true
			h.mu.Unlock()
			d := s.backoff[len(s.backoff)-1]
			if failures < len(s.backoff) {
				d = s.backoff[failures]
			}
			failures++
			retry = time.After(d)
			break
		}
	}
}
