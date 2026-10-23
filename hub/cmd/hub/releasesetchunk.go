package main

// The chunking success probe of a release-set rehearsal (plan r3, section 3; the probe of codex-29,
// confirmed by claude-52). In an H window, the SDK fixture plugin "sp" (tests/sdk-parts of the H
// tree) is added to the lab managed file, and a registered lab agent calls sp.save {kb: 150} with
// one key twice, with sp.count before and after. The reply is about 150 KiB, more than one part,
// so the hub must join the parts. Then sp is removed and stopped, and the tool proves that nothing
// of it runs. A missing or failing probe fails the rehearsal.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	relChunkPlugin = "sp"
	relChunkKB     = 150
	relChunkBytes  = relChunkKB * 1024
	// relChunkSHA is the SHA-256 of 153600 ASCII "s" bytes: the data that sp.save {kb: 150} returns.
	relChunkSHA = "1ea33f603a08d9943251c23c9fb7e9ac9c456570d5798a02118e06efd3582f7c"
)

// relChunkCheck is the record of one probe window.
type relChunkCheck struct {
	When         string `json:"when"`
	H            string `json:"h"`
	FixtureSHA   string `json:"fixture_digest"`
	ManagedSHA   string `json:"managed_sha256"` // before, and again after the window
	CountBefore  int64  `json:"count_before"`
	CountAfter   int64  `json:"count_after"`
	DataBytes    int    `json:"data_bytes"`
	DataSHA256   string `json:"data_sha256"`
	PartLines    int    `json:"part_lines"`
	SPInstance   string `json:"sp_instance"`
	SPStoppedPID int    `json:"sp_stopped_pid"`
}

// chunkFixture copies tests/sdk-parts of the H tree from the lab clone into <lab>/fixtures/sp, so
// the fixture is the same in every H window. An H without it fails the set rehearsal.
func (h *rehearsal) chunkFixture() (string, error) {
	clone := filepath.Join(h.lab, "release", "LazyRex")
	ti := h.c.To.Commit + ":tests/sdk-parts"
	ctx := context.Background()
	tree, err := lsTree(ctx, clone, ti)
	if err != nil || tree["lazyrex.json"].OID == "" {
		return "", relFail("H %s has no tests/sdk-parts with a lazyrex.json: the chunking probe needs it (%v)", h.c.To.Commit, err)
	}
	dir := filepath.Join(h.lab, "fixtures", relChunkPlugin)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	files, err := archiveInto(ctx, clone, ti, tree, dir)
	if err != nil {
		return "", err
	}
	return digestOf(files), nil
}

// chunkProbe runs one H window of the probe and records it.
func (h *rehearsal) chunkProbe(when, digest string) error {
	labCfg, err := readPluginCmdConfig(h.cfg)
	if err != nil {
		return err
	}
	rec := relChunkCheck{When: when, H: h.c.To.Commit, FixtureSHA: digest}
	before, err := os.ReadFile(labCfg.managed)
	if err != nil {
		return err
	}
	rec.ManagedSHA = sha256Bytes(before)
	var m struct {
		Plugins []map[string]any `json:"plugins"`
	}
	if err := json.Unmarshal(before, &m); err != nil {
		return relFail("the lab managed file: %v", err)
	}
	for _, p := range m.Plugins {
		if p["id"] == relChunkPlugin {
			return relFail("the lab managed file already has %s", relChunkPlugin)
		}
	}
	m.Plugins = append(m.Plugins, map[string]any{"id": relChunkPlugin, "source": filepath.Join(h.lab, "fixtures", relChunkPlugin)})
	added, _ := json.MarshalIndent(m, "", "  ")
	restore := func() error {
		if err := os.WriteFile(labCfg.managed, before, 0o600); err != nil {
			return err
		}
		if b, err := os.ReadFile(labCfg.managed); err != nil || sha256Bytes(b) != rec.ManagedSHA {
			return relFail("the lab managed file is not back to its bytes (%v)", err)
		}
		return nil
	}
	if err := os.WriteFile(labCfg.managed, added, 0o600); err != nil {
		return err
	}
	// The stop at the end of an earlier probe leaves the plugins row of sp with desired "stopped",
	// and a reload keeps that desire. Then this probe starts sp explicitly.
	stopped, err := h.chunkPluginStopped()
	if err != nil {
		restore()
		return relFail("%s: %v", when, err)
	}
	var client string
	reload := func(lr *releaser) error {
		hub, err := lr.currentHub()
		if err != nil {
			return err
		}
		client = hub.Client
		_, err = lr.hubDo(client, "hub.plugins.reload", map[string]any{})
		return err
	}
	if err := h.withLab(reload); err != nil {
		restore()
		return relFail("%s: hub.plugins.reload with %s: %v", when, relChunkPlugin, err)
	}
	if stopped {
		if err := h.withLab(func(lr *releaser) error {
			_, err := lr.hubDo(client, "hub.admin.start", map[string]any{"plugin": relChunkPlugin})
			return err
		}); err != nil {
			restore()
			return relFail("%s: hub.admin.start %s after the reload: %v", when, relChunkPlugin, err)
		}
	}
	inst, err := h.waitPluginState(relChunkPlugin, true)
	if err != nil {
		restore()
		return relFail("%s: %v", when, err)
	}
	rec.SPInstance = inst
	logp := filepath.Join(labCfg.state, "dev", "hub.log")
	mark, err := markLog(logp)
	if err != nil {
		restore()
		return relFail("%s: %v", when, err)
	}
	defer mark.f.Close()
	key := "rehearsal-chunk-" + strings.ReplaceAll(when, " ", "-")
	save := json.RawMessage(fmt.Sprintf(`{"kb":%d,"key":%q}`, relChunkKB, key))
	outs, err := h.agentBatch("chunk-"+strings.ReplaceAll(when, " ", "-"), []relSetItem{
		{Kind: "action", Action: "sp.count"}, {Kind: "action", Action: "sp.save", Args: save},
		{Kind: "action", Action: "sp.save", Args: save}, {Kind: "action", Action: "sp.count"}})
	probeErr := err
	if probeErr == nil {
		probeErr = checkChunkOuts(outs, &rec)
	}
	if probeErr == nil {
		probeErr = checkChunkLog(mark, logp, &rec)
	}
	// The cleanup runs also after a failed probe; a failed cleanup fails the window.
	if err := restore(); err != nil {
		return err
	}
	if err := h.withLab(reload); err != nil {
		return relFail("%s: hub.plugins.reload without %s: %v", when, relChunkPlugin, err)
	}
	pid, err := h.stopChunkPlugin(client)
	if err != nil {
		return relFail("%s: %v", when, err)
	}
	rec.SPStoppedPID = pid
	if probeErr != nil {
		return relFail("%s: the chunking probe failed: %v", when, probeErr)
	}
	h.chunks = append(h.chunks, rec)
	h.say("chunking probe %s: %d bytes in parts (%d part lines), sha256 %s, one new row; sp removed and stopped", when, rec.DataBytes, rec.PartLines, rec.DataSHA256[:12])
	return nil
}

// checkChunkOuts checks the four calls: counts before and after, two saves with the expected data.
func checkChunkOuts(outs []relCallOut, rec *relChunkCheck) error {
	for i, o := range outs {
		if o.Code != 0 {
			return relFail("call %d exited %d: %s", i+1, o.Code, strings.TrimSpace(string(o.Out)))
		}
	}
	count := func(o relCallOut) (int64, error) {
		var c struct {
			N *int64 `json:"n"`
		}
		if json.Unmarshal(o.Out, &c) != nil || c.N == nil {
			return 0, relFail("sp.count gave %s", strings.TrimSpace(string(o.Out)))
		}
		return *c.N, nil
	}
	var err error
	if rec.CountBefore, err = count(outs[0]); err != nil {
		return err
	}
	if rec.CountAfter, err = count(outs[3]); err != nil {
		return err
	}
	for _, o := range outs[1:3] {
		var s struct {
			KB   *int    `json:"kb"`
			Data *string `json:"data"`
		}
		if json.Unmarshal(o.Out, &s) != nil || s.KB == nil || s.Data == nil {
			return relFail("sp.save gave no kb and data (%d bytes of output)", len(o.Out))
		}
		sum := sha256.Sum256([]byte(*s.Data))
		got := hex.EncodeToString(sum[:])
		if *s.KB != relChunkKB || len(*s.Data) != relChunkBytes || got != relChunkSHA {
			return relFail("sp.save gave kb %d, %d bytes, sha256 %s; want kb %d, %d bytes, sha256 %s", *s.KB, len(*s.Data), got, relChunkKB, relChunkBytes, relChunkSHA)
		}
		rec.DataBytes, rec.DataSHA256 = len(*s.Data), got
	}
	if rec.CountAfter != rec.CountBefore+1 {
		return relFail("sp.count went from %d to %d: the keyed save must add exactly one row", rec.CountBefore, rec.CountAfter)
	}
	return nil
}

// relLogMark is the hub log before a probe: the open file, its identity and its size then.
type relLogMark struct {
	f      *os.File
	fi     os.FileInfo
	offset int64
}

// markLog opens the hub log at p and records its identity and size. The caller closes m.f.
func markLog(p string) (*relLogMark, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, relFail("the hub log %s: %v", p, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, relFail("the hub log %s: %v", p, err)
	}
	return &relLogMark{f: f, fi: fi, offset: fi.Size()}, nil
}

// checkChunkLog reads the hub log lines that came after the mark m: they must have
// hub.reply.part lines (the replies came in parts, not inline) and no parts_invalid or too_large
// refusal. The file at logp must still be the marked file, not cut below the mark, and every read
// must succeed up to its end.
func checkChunkLog(m *relLogMark, logp string, rec *relChunkCheck) error {
	now, err := os.Stat(logp)
	if err != nil {
		return relFail("the hub log %s: %v", logp, err)
	}
	if !os.SameFile(m.fi, now) {
		return relFail("the hub log %s was replaced during the probe", logp)
	}
	cur, err := m.f.Stat()
	if err != nil {
		return relFail("the hub log %s: %v", logp, err)
	}
	if cur.Size() < m.offset {
		return relFail("the hub log %s was cut during the probe (%d bytes, %d before)", logp, cur.Size(), m.offset)
	}
	b, err := readFrom(m.f, m.offset)
	if err != nil {
		return relFail("the hub log %s: %v", logp, err)
	}
	return chunkLogLines(b, rec)
}

// readFrom reads r from offset to its end. Any error other than the end fails.
func readFrom(r io.ReaderAt, offset int64) ([]byte, error) {
	var b []byte
	buf := make([]byte, 64<<10)
	for {
		n, err := r.ReadAt(buf, offset)
		b, offset = append(b, buf[:n]...), offset+int64(n)
		if errors.Is(err, io.EOF) {
			return b, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// chunkLogLines checks the log text b of one probe window.
func chunkLogLines(b []byte, rec *relChunkCheck) error {
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "parts_invalid") || strings.Contains(line, "too_large") {
			return relFail("the hub log has a refusal: %s", line)
		}
		if strings.Contains(line, "hub.reply.part from") {
			rec.PartLines++
		}
	}
	if rec.PartLines == 0 {
		return relFail("the hub log has no hub.reply.part line: the replies did not come in parts")
	}
	return nil
}

// waitPluginState waits (60 s) until the lab hub reports plugin name running and registered
// (running true), and returns its instance ID; or until no entry, kept record or running plugin
// of that name remains (running false).
func (h *rehearsal) waitPluginState(name string, running bool) (string, error) {
	var last []byte
	for i := 0; i < 300; i++ {
		var out []byte
		err := h.withLab(func(lr *releaser) error {
			hub, err := lr.currentHub()
			if err != nil {
				return err
			}
			out, err = lr.hubDo(hub.Client, "hub.plugins", map[string]any{})
			return err
		})
		if err != nil {
			return "", err
		}
		last = out
		var pl struct {
			Plugins []struct {
				Name string `json:"name"`
			} `json:"plugins"`
			Entries []struct {
				Name       string `json:"name"`
				State      string `json:"state"`
				InstanceID string `json:"instance_id"`
				Registered bool   `json:"registered"`
			} `json:"entries"`
			Kept []struct {
				Name string `json:"name"`
			} `json:"kept"`
		}
		if err := json.Unmarshal(out, &pl); err != nil {
			return "", relFail("hub.plugins: %s", out)
		}
		found := false
		for _, e := range pl.Entries {
			if e.Name == name {
				found = true
				if running && e.State == "running" && e.Registered && e.InstanceID != "" {
					return e.InstanceID, nil
				}
			}
		}
		for _, p := range pl.Plugins {
			found = found || p.Name == name
		}
		for _, k := range pl.Kept {
			found = found || k.Name == name
		}
		if !running && !found {
			return "", nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	if running {
		return "", relFail("%s did not run and register in 60 s: %s", name, last)
	}
	return "", relFail("%s is still in hub.plugins after 60 s: %s", name, last)
}

// stopChunkPlugin stops the kept sp record, and proves that its process ended and the hub keeps no
// entry, kept record or row of it. It returns the stopped pid.
func (h *rehearsal) stopChunkPlugin(client string) (int, error) {
	db, err := h.hubDB(true)
	if err != nil {
		return 0, err
	}
	var pid int
	var start string
	err = db.QueryRow("SELECT child_pid, child_start FROM plugins WHERE name = ?", relChunkPlugin).Scan(&pid, &start)
	db.Close()
	if err != nil {
		return 0, relFail("the plugins row of %s: %v", relChunkPlugin, err)
	}
	if err := h.withLab(func(lr *releaser) error {
		_, err := lr.hubDo(client, "hub.admin.stop", map[string]any{"plugin": relChunkPlugin})
		return err
	}); err != nil {
		return 0, relFail("hub.admin.stop %s: %v", relChunkPlugin, err)
	}
	if _, err := h.waitPluginState(relChunkPlugin, false); err != nil {
		return 0, err
	}
	for i := 0; i < 75 && pid > 0 && startTime(pid) == start; i++ {
		time.Sleep(200 * time.Millisecond)
	}
	if pid > 0 && startTime(pid) == start {
		return 0, relFail("the %s process %d still runs after its stop", relChunkPlugin, pid)
	}
	db, err = h.hubDB(true)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	for _, q := range []string{"SELECT count(*) FROM spawn_intents WHERE plugin = ? AND status IN ('prepared', 'sending')",
		"SELECT count(*) FROM plugins WHERE name = ? AND desired = 'running'"} {
		var n int
		if err := db.QueryRow(q, relChunkPlugin).Scan(&n); err != nil {
			return 0, relFail("%s: %v", q, err)
		}
		if n != 0 {
			return 0, relFail("the lab hub.db keeps %d rows of %s: %s", n, relChunkPlugin, q)
		}
	}
	return pid, nil
}

// chunkPluginStopped tells whether the lab hub.db has a plugins row of sp with desired "stopped":
// the trace of the stop at the end of an earlier probe.
func (h *rehearsal) chunkPluginStopped() (bool, error) {
	db, err := h.hubDB(true)
	if err != nil {
		return false, err
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM plugins WHERE name = ? AND desired = 'stopped'", relChunkPlugin).Scan(&n); err != nil {
		return false, relFail("the plugins row of %s: %v", relChunkPlugin, err)
	}
	return n > 0, nil
}
