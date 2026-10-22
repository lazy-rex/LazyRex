package main

// The rehearsal record (docs/releasing.md, "Rehearse"): <run>/rehearsal.json. A live precheck runs
// only when a passing record binds exactly what the live run uses: the tool binary, the spec, the
// frozen candidate, both pin documents, both verify lists, the probe file and the lab
// transformation. A change of any of them after the rehearsal refuses the precheck.

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

const (
	relRehearsalFile = "rehearsal.json"
	relSpecFile      = "spec.json"
	relVerifyFwd     = "verify-fwd.json"
	relVerifyBack    = "verify-back.json"
	relLabTransform  = "lab-transform.json"
	relSetJSON       = "set.json" // the set file of a release-set rehearsal
)

// The tiers of a rehearsal. The full tier adds the partial-failure cases.
const (
	tierDefault = "default"
	tierFull    = "full"
)

// relRehearsal is the rehearsal record.
type relRehearsal struct {
	Pass        bool                `json:"pass"`
	At          string              `json:"at"`
	Bound       map[string]string   `json:"bound"` // the sha256 of each bound value, by field
	Tier        string              `json:"tier"`
	TierReasons []string            `json:"tier_reasons"`
	Phases      []relPhase          `json:"phases"`
	Lab         string              `json:"lab"`                 // the lab directory (deleted after a pass)
	Mirrors     map[string]string   `json:"mirrors,omitempty"`   // rehearse: the HEAD of each GitHub mirror of the lab
	Commits     map[string][]string `json:"commits,omitempty"`   // rehearse: the commits that the mirrors held, by owner/repo
	SDK         []relSDKCheck       `json:"sdk,omitempty"`       // rehearse: the SDK byte check of each hub check point
	Set         []relSetCheck       `json:"set,omitempty"`       // rehearse -set: every data check and probe of the set phase
	Chunks      []relChunkCheck     `json:"chunks,omitempty"`    // rehearse -set: the chunking probe windows
	SetSteps    []relSetStep        `json:"set_steps,omitempty"` // rehearse -set: each plugin step as its lab freeze saw it
}

// relSetStep is one plugin step of a set rehearsal: the frozen lab candidate and the base pins of
// the lab lock at that freeze. The live gate predicts the live state before the same step from it.
type relSetStep struct {
	Plugin    string       `json:"plugin"`
	Candidate relCandidate `json:"candidate"`
	BasePins  []string     `json:"base_pins"`
}

type relPhase struct {
	Name   string `json:"name"`
	Result string `json:"result"`
}

// relBoundFiles are the run files that a record binds, by field. A missing file is bound as "-".
var relBoundFiles = [...]struct{ field, file string }{
	{"spec", relSpecFile},
	{"candidate", relCandidateFile},
	{"pins-fwd", relFwdDoc},
	{"pins-back", relBackDoc},
	{"verify-fwd", relVerifyFwd},
	{"verify-back", relVerifyBack},
	{"lab-transform", relLabTransform},
	{"set", relSetJSON},
}

// bindings computes the values that a rehearsal record binds, for this run and the tool binary
// tool.
func (r *releaser) bindings(tool string) (map[string]string, error) {
	out := map[string]string{}
	h, err := sha256File(tool)
	if err != nil {
		return nil, relFail("the sha256 of the tool %s: %v", tool, err)
	}
	out["tool"] = h
	file := func(field, path string) error {
		h, err := sha256File(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			out[field] = "-"
		case err != nil:
			return relFail("the sha256 of %s: %v", path, err)
		default:
			out[field] = h
		}
		return nil
	}
	for _, b := range relBoundFiles {
		if err := file(b.field, filepath.Join(r.run, b.file)); err != nil {
			return nil, err
		}
	}
	// The live inputs that rehearse copies into the lab (labFiles): a change after the rehearsal
	// needs a new one. The lock is bound by its pins (a sync can name new cache dirs).
	for _, f := range [][2]string{{"live-config", r.cfg.path}, {"live-managed", r.cfg.managed}, {"live-base", peersPath(r.cfg.lock)}} {
		if err := file(f[0], f[1]); err != nil {
			return nil, err
		}
	}
	if out["live-lock-pins"], err = lockPinsHash(r.cfg.lock); err != nil {
		return nil, relFail("the lock pins of %s: %v", r.cfg.lock, err)
	}
	out["live-profiles"] = "-"
	if p := configProfiles(r.cfg.path); p != "" {
		if err := file("live-profiles", p); err != nil {
			return nil, err
		}
		if out["live-profiles"] == "-" {
			return nil, relFail("the profile file %s does not exist", p)
		}
	}
	out["probes"] = "-"
	if r.spec.Probes != "" {
		if err := file("probes", r.spec.Probes); err != nil {
			return nil, err
		}
		if out["probes"] == "-" {
			return nil, relFail("the probe file %s does not exist", r.spec.Probes)
		}
	}
	return out, nil
}

// readRehearsal reads <run>/rehearsal.json.
func (r *releaser) readRehearsal() (*relRehearsal, error) {
	b, err := os.ReadFile(filepath.Join(r.run, relRehearsalFile))
	if err != nil {
		return nil, relFail("no rehearsal record: %v (rehearse first)", err)
	}
	var rec relRehearsal
	if err := decodeStrict(b, &rec); err != nil {
		return nil, relFail("%s: %v", relRehearsalFile, err)
	}
	return &rec, nil
}

// checkRehearsal is the hard gate of precheck: a passing record whose bound values equal the
// values of this run and this tool binary. No flag overrides it.
func (r *releaser) checkRehearsal(tool string) error {
	rec, err := r.readRehearsal()
	if err != nil {
		return err
	}
	if !rec.Pass {
		return relFail("the rehearsal record is not a pass; rehearse again")
	}
	now, err := r.bindings(tool)
	if err != nil {
		return err
	}
	var fields []string
	for k := range now {
		fields = append(fields, k)
	}
	for k := range rec.Bound {
		if _, ok := now[k]; !ok {
			fields = append(fields, k)
		}
	}
	sort.Strings(fields)
	for _, k := range fields {
		if rec.Bound[k] != now[k] {
			return relFail("the rehearsal does not cover this release: %s; rehearse again", k)
		}
	}
	return nil
}

// writeRehearsal writes the record (temporary file, fsync, rename).
func (r *releaser) writeRehearsal(rec *relRehearsal) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return replaceFile(filepath.Join(r.run, relRehearsalFile), append(b, '\n'))
}

// configProfiles is the profile file that the config at path names, "" for none or an unreadable
// config.
func configProfiles(path string) string {
	var raw struct {
		Profiles string `json:"profiles"`
	}
	json.Unmarshal(mustReadFile(path), &raw)
	return raw.Profiles
}
