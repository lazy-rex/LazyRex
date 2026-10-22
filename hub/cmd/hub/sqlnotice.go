package main

// hub.sql notices (docs/sql.md section 7). A handler commit writes its notices into
// lazyrex_notice_outbox of the plugin's file, in the same transaction: a rollback has no notices.
// After the commit the hub applies the outbox rows to hub.db in seq order, in one hub.db
// transaction that also writes a receipt (plugin, seq) for each row, then deletes the rows and
// then their receipts. A crash between the steps applies nothing twice: a row with a receipt is
// skipped. A failed apply retries from a timer (1 s to 60 s), and the hub start applies every
// outbox in <state>/sql. seq is AUTOINCREMENT, so a seq is not used again in one file.

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// outboxRow is one notice of a committed outbox.
type outboxRow struct {
	seq     int64
	agent   string
	pending bool
}

// noticeArgs reads the notices of a commit: [{agent, pending}], one entry for each agent.
func noticeArgs(v any) ([]noticeOp, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		if m, ok := v.(map[string]any); ok && len(m) == 0 {
			return nil, nil // Lua encodes an empty table as {}
		}
		return nil, refuse("invalid_input", "notices is a list of {agent, pending}")
	}
	seen := map[string]bool{}
	var out []noticeOp
	for i, x := range list {
		m, _ := x.(map[string]any)
		agent, _ := m["agent"].(string)
		pending, ok := m["pending"].(bool)
		if agent == "" || !ok || len(m) != 2 {
			return nil, refuse("invalid_input", "notices[%d] is {agent, pending}", i+1)
		}
		if seen[agent] {
			return nil, refuse("invalid_input", "agent %q appears twice in notices", agent)
		}
		seen[agent] = true
		out = append(out, noticeOp{agent: agent, pending: pending})
	}
	return out, nil
}

// writeOutbox adds the notices to the outbox in the open transaction. The caller holds the lock.
func (p *pluginSQL) writeOutbox(ctx context.Context, notes []noticeOp) error {
	for _, n := range notes {
		pending := 0
		if n.pending {
			pending = 1
		}
		if err := p.w.hub(ctx, `INSERT INTO lazyrex_notice_outbox (agent, pending) VALUES (?, ?)`, n.agent, pending); err != nil {
			return err
		}
	}
	return nil
}

// drain applies the committed outbox of p. The caller holds the lock. A failure leaves the rows;
// the retry timer applies them later.
func (s *sqlService) drain(ctx context.Context, p *pluginSQL) error {
	p.w.trusted = true
	rows, err := p.w.c.QueryContext(ctx, `SELECT seq, agent, pending FROM lazyrex_notice_outbox ORDER BY seq`)
	if err != nil {
		p.w.trusted = false
		return err
	}
	var list []outboxRow
	for rows.Next() {
		var r outboxRow
		var pending int
		if err := rows.Scan(&r.seq, &r.agent, &pending); err != nil {
			rows.Close()
			p.w.trusted = false
			return err
		}
		r.pending = pending == 1
		list = append(list, r)
	}
	rows.Close()
	p.w.trusted = false
	if len(list) == 0 {
		return nil
	}
	if err := s.applyNotices(p.name, list); err != nil {
		return err
	}
	last := list[len(list)-1].seq
	if err := p.w.hub(ctx, `DELETE FROM lazyrex_notice_outbox WHERE seq <= ?`, last); err != nil {
		return err
	}
	return s.dropReceipts(p.name, last)
}

// drainLater retries the outbox of p with a backoff until it is empty.
func (s *sqlService) drainLater(p *pluginSQL) {
	p.mu.Lock()
	if p.retrying {
		p.mu.Unlock()
		return
	}
	p.retrying = true
	p.mu.Unlock()
	go func() {
		wait := time.Second
		for {
			time.Sleep(wait)
			p.lock <- struct{}{}
			err := s.drain(context.Background(), p)
			p.release()
			if err == nil {
				break
			}
			log.Printf("hub.sql notices of %s: %v; retry in %s", p.name, err, wait)
			if wait *= 2; wait > time.Minute {
				wait = time.Minute
			}
		}
		p.mu.Lock()
		p.retrying = false
		p.mu.Unlock()
	}()
}

// drainAll applies the outbox of each plugin file in the state, also of plugins that are not
// configured now (hub start).
func (s *sqlService) drainAll(ctx context.Context) {
	if s == nil {
		return
	}
	files, _ := filepath.Glob(filepath.Join(s.dir, "*.db"))
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".db")
		if !pluginName.MatchString(name) {
			continue
		}
		if _, err := os.Stat(f); err != nil {
			continue
		}
		p, err := s.open(ctx, name)
		if err != nil {
			log.Printf("hub.sql notices: open %s: %v", f, err)
			continue
		}
		p.lock <- struct{}{}
		err = s.drain(ctx, p)
		p.release()
		if err != nil {
			log.Printf("hub.sql notices of %s: %v", name, err)
			s.drainLater(p)
		}
	}
}
