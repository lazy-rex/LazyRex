package main

// Agent prompts (docs/hub.md "Agent prompts and acting as the user"). An agent's configured
// program status record in state blocked is an open prompt. The hub gives each prompt an id and a
// per-agent generation, in memory only: a hub restart closes every prompt. syncPrompts compares
// each agent with its open prompt after every record or binding change, so no change path has to
// know the prompt rules.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"
	"unicode/utf8"
)

// statusRecord is the part of a program status record that the hub keeps (rex-protocol.md,
// "Agent status"). Msg is the decoded text.
type statusRecord struct {
	State        string `json:"state"`
	Kind         string `json:"kind"`
	Msg          string `json:"msg"`
	App          string `json:"app"`
	EffectiveApp string `json:"effective_app"`
}

// promptMsgMax bounds the msg of a prompt event, so one event fits a control message.
const promptMsgMax = 8 << 10

// prompt is an open prompt and the binding it was opened for.
type prompt struct {
	ID                    string
	Gen                   int64
	Agent, Session, Block string
	PID                   int
	Start                 string
	Kind, Msg             string // Msg is cut to promptMsgMax
	Truncated             bool
	App, EffectiveApp     string
	OpenedAt              time.Time
	sum                   [32]byte // sha256 of the full msg: a change after the cut also replaces
}

// promptBook is the prompt state. h.mu guards it.
type promptBook struct {
	open     map[string]*prompt      // agent -> its open prompt
	gen      map[string]int64        // agent -> the generation of its last prompt
	last     map[string]closedPrompt // agent -> its last closed prompt
	agentRev map[string]int64        // agent -> rev of its last change
	rev      int64                   // increases at each open and close
}

// closedPrompt names the last prompt of an agent that closed, for the plugin's text only.
type closedPrompt struct {
	ID, Reason string
}

// syncPrompts opens, replaces and closes prompts to match the agents. h.mu must be held.
func (h *Hub) syncPrompts() {
	if h.prompts.open == nil {
		h.prompts = promptBook{open: map[string]*prompt{}, gen: map[string]int64{}, last: map[string]closedPrompt{}, agentRev: map[string]int64{}}
	}
	agents := map[string]*Agent{}
	for _, a := range h.everyAgent() {
		agents[a.ID] = a
	}
	for id, p := range h.prompts.open {
		if agents[id] == nil {
			h.closePrompt(p, "gone")
		}
	}
	for _, a := range agents {
		h.syncPrompt(a)
	}
}

func (h *Hub) syncPrompt(a *Agent) {
	b := &h.prompts
	r := a.Record
	sum := sha256.Sum256([]byte(r.Msg))
	live := a.BlockID != "" && a.live()
	if p := b.open[a.ID]; p != nil {
		reason := ""
		switch {
		case !live || p.Block != a.BlockID || p.PID != a.ChildPID || p.Start != a.ChildStart || p.Session != h.bindingSession(a):
			reason = "gone"
		case a.State == "":
			reason = "removed" // the record is absent, or its state is unknown
		case a.State != "blocked":
			reason = "state"
		case p.Kind != r.Kind || p.sum != sum || p.App != r.App || p.EffectiveApp != r.EffectiveApp:
			reason = "replaced"
		default:
			return // the same prompt
		}
		h.closePrompt(p, reason)
	}
	if !live || a.State != "blocked" {
		return
	}
	b.gen[a.ID]++
	msg, cut := cutUTF8(r.Msg, promptMsgMax)
	p := &prompt{ID: newPromptID(), Gen: b.gen[a.ID], Agent: a.ID, Session: h.bindingSession(a), Block: a.BlockID,
		PID: a.ChildPID, Start: a.ChildStart, Kind: r.Kind, Msg: msg, Truncated: cut, App: r.App,
		EffectiveApp: r.EffectiveApp, OpenedAt: time.Now().UTC(), sum: sum}
	b.open[a.ID] = p
	b.rev++
	b.agentRev[a.ID] = b.rev
	h.markPromptDirty(a.ID)
	h.ev.emit("prompt_opened", map[string]any{"agent": a.ID, "prompt_id": p.ID, "generation": p.Gen, "rev": b.rev})
}

func (h *Hub) closePrompt(p *prompt, reason string) {
	b := &h.prompts
	delete(b.open, p.Agent)
	b.last[p.Agent] = closedPrompt{ID: p.ID, Reason: reason}
	b.rev++
	b.agentRev[p.Agent] = b.rev
	h.markPromptDirty(p.Agent)
	h.ev.emit("prompt_closed", map[string]any{"agent": p.Agent, "prompt_id": p.ID, "reason": reason, "rev": b.rev})
}

// promptOpen returns the prompt with this id if it is still the open prompt of its agent, for the
// same binding and a blocked record (an answer's binding check). h.mu must be held.
func (h *Hub) promptOpen(id string) *prompt {
	var found *prompt
	for _, p := range h.prompts.open {
		if p.ID == id {
			found = p
		}
	}
	if found == nil {
		return nil
	}
	h.syncPrompts() // a process that exited without an event yet
	if h.prompts.open[found.Agent] != found {
		return nil
	}
	return found
}

func newPromptID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// cutUTF8 cuts s to at most max bytes on a rune boundary.
func cutUTF8(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	i := max
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i], true
}

// json is a prompt as pushes and hub.prompts give it.
func (p *prompt) json() map[string]any {
	return map[string]any{"prompt_id": p.ID, "generation": p.Gen, "agent": p.Agent, "session_id": p.Session,
		"block_id": p.Block, "kind": p.Kind, "msg": p.Msg, "msg_truncated": p.Truncated, "app": p.App,
		"opened_at": p.OpenedAt.Format(time.RFC3339Nano)}
}

// promptsList is hub.prompts: the open prompts and the rev that describes exactly this list (both
// read under one hold of h.mu).
func (h *Hub) promptsList(pl *Plugin) (any, error) {
	if err := h.checkPromptsGrant(pl); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.syncPrompts()
	ids := make([]string, 0, len(h.prompts.open))
	for id := range h.prompts.open {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	list := []any{}
	for _, id := range ids {
		list = append(list, h.prompts.open[id].json())
	}
	return map[string]any{"hub_generation": revString(h.generation), "rev": revString(h.prompts.rev), "prompts": list}, nil
}

// promptsGranted reports the prompts grant of a plugin: its run (the instance's config) has it,
// and the record still has a config entry. A kept record (its entry was removed) has no grant,
// also while its old instance runs. h.mu must be held.
func (pl *Plugin) promptsGranted() bool {
	return pl.entry != nil && pl.run.Prompts
}

// checkPromptsGrant refuses a plugin without the prompts grant.
func (h *Hub) checkPromptsGrant(pl *Plugin) error {
	h.mu.Lock()
	ok := pl.promptsGranted()
	h.mu.Unlock()
	if !ok {
		return refuse("not_granted", "the hub config gives plugin %s no prompts grant", pl.Name)
	}
	return nil
}
