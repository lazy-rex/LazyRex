package main

// The SDK byte binding of a rehearsal: at each hub start check point, the bound hub runs the
// sdk/lua bytes of the expected commit. dev.sh passes -sdk "$DIR/sdk/lua" from the release clone,
// so a checkout moves the hub and the SDK together; this check proves it for the running process.
// It reads the exact argv (procArgvExact), not the ps words of procArgv: a path can hold spaces.

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// relSDKCheck is the record of one check point.
type relSDKCheck struct {
	Phase    string `json:"phase"`
	PID      int    `json:"pid"`
	Start    string `json:"start"`
	Client   string `json:"client"`
	Seq      int    `json:"seq"`
	Commit   string `json:"commit"`   // the expected commit
	Path     string `json:"path"`     // the -sdk path of the hub argv
	Expected string `json:"expected"` // the digest of <commit>:sdk/lua
	Actual   string `json:"actual"`   // the digest of the files at Path
}

// parseProcArgs2 reads the argv of a kern.procargs2 buffer: a 32-bit argc, the executable path, NUL
// padding, then argc NUL-terminated arguments (the environment follows). A short buffer is an error.
func parseProcArgs2(b []byte) ([]string, error) {
	if len(b) < 4 {
		return nil, errors.New("procargs2: no argc")
	}
	argc := int(int32(binary.LittleEndian.Uint32(b)))
	if argc < 1 {
		return nil, errors.New("procargs2: argc is not positive")
	}
	i := 4
	for i < len(b) && b[i] != 0 {
		i++
	}
	for i < len(b) && b[i] == 0 {
		i++
	}
	argv := make([]string, 0, argc)
	for len(argv) < argc {
		j := i
		for j < len(b) && b[j] != 0 {
			j++
		}
		if j >= len(b) {
			return nil, errors.New("procargs2: the arguments are cut")
		}
		argv = append(argv, string(b[i:j]))
		i = j + 1
	}
	return argv, nil
}

// sdkArgExact is the value of the one -sdk flag of a hub argv. It parses argv[1:] with the hub's
// own flags (hubFlags), so it stops where the hub stops and consumes values as the hub does. It
// refuses an argv that does not parse, any argument after the flags (a "--" or a positional
// argument: the hub would not read a -sdk after it), a missing, repeated or empty -sdk, and a path
// that is not absolute and clean.
func sdkArgExact(argv []string) (string, error) {
	if len(argv) == 0 {
		return "", relFail("the hub argv is empty")
	}
	set := flag.NewFlagSet("hub", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	v := hubFlags(set)
	if err := set.Parse(argv[1:]); err != nil {
		return "", relFail("the hub argv does not parse with the hub flags: %v", err)
	}
	if set.NArg() > 0 {
		return "", relFail("the hub argv has arguments after its flags: %q", set.Args())
	}
	switch {
	case v.sdkN == 0:
		return "", relFail("the hub argv has no -sdk flag")
	case v.sdkN > 1:
		return "", relFail("the hub argv has %d -sdk flags", v.sdkN)
	}
	p := *v.sdk
	switch {
	case p == "":
		return "", relFail("the hub argv has an empty -sdk path")
	case !filepath.IsAbs(p) || filepath.Clean(p) != p:
		return "", relFail("the hub -sdk path %q is not absolute and clean", p)
	}
	return p, nil
}

// sdkUnder refuses a path that is not inside root, with symlinks resolved on both sides.
func sdkUnder(p, root string) error {
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		return relFail("the hub -sdk path %q: %v", p, err)
	}
	rr, err := filepath.EvalSymlinks(root)
	if err != nil {
		return relFail("the root %q: %v", root, err)
	}
	rel, err := filepath.Rel(rr, rp)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return relFail("the hub -sdk path %q is not inside %s", p, root)
	}
	return nil
}

// sdkTreeDigest is the digest of <commit>:sdk/lua in the git repository dir: the tree is
// extracted with git archive into a temporary directory under tmp, as a plugin fetch does.
func sdkTreeDigest(ctx context.Context, dir, commit, tmp string) (string, error) {
	ti := commit + ":sdk/lua"
	tree, err := lsTree(ctx, execGit, dir, ti)
	if err != nil {
		return "", err
	}
	if len(tree) == 0 {
		return "", relFail("%s has no files", ti)
	}
	d, err := os.MkdirTemp(tmp, ".sdk-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(d)
	files, err := archiveInto(ctx, execGit, dir, ti, tree, d)
	if err != nil {
		return "", err
	}
	return digestOf(files), nil
}

func (r *releaser) argvExact(pid int) ([]string, error) {
	if r.argvExactOf != nil {
		return r.argvExactOf(pid)
	}
	return procArgvExact(pid)
}

func (r *releaser) sdkDigestAt(commit string) (string, error) {
	if r.sdkTreeOf != nil {
		return r.sdkTreeOf(commit)
	}
	return sdkTreeDigest(r.ctx, r.relClone(), commit, r.run)
}

// sameHub fails unless the hub of the last hub_started event is want: the same pid, start time,
// client and start sequence.
func (r *releaser) sameHub(want *relHub, when string) error {
	cur, err := r.currentHub()
	if err != nil {
		return relFail("%s: %v", when, err)
	}
	if *cur != *want {
		return relFail("%s: the hub changed: pid %d start %s client %s seq %d, now pid %d start %s client %s seq %d",
			when, want.PID, want.Start, want.Client, want.Seq, cur.PID, cur.Start, cur.Client, cur.Seq)
	}
	return nil
}

// checkSDK checks that the bound hub want runs the sdk/lua bytes of commit from a path inside
// root. It checks the hub identity before the argv read, after it, and after the hashing. Every
// failed read or mismatch fails the check.
func (r *releaser) checkSDK(phase string, want *relHub, commit, root string) (*relSDKCheck, error) {
	if want == nil {
		return nil, relFail("phase %s: no bound hub for the SDK check", phase)
	}
	if err := r.sameHub(want, "phase "+phase+", before the argv read"); err != nil {
		return nil, err
	}
	argv, err := r.argvExact(want.PID)
	if err == nil && len(argv) == 0 {
		err = errors.New("the argv is empty")
	}
	if err != nil {
		return nil, relFail("phase %s: the argv of the hub pid %d: %v", phase, want.PID, err)
	}
	if err := r.sameHub(want, "phase "+phase+", after the argv read"); err != nil {
		return nil, err
	}
	p, err := sdkArgExact(argv)
	if err != nil {
		return nil, relFail("phase %s: %v", phase, err)
	}
	if err := sdkUnder(p, root); err != nil {
		return nil, relFail("phase %s: %v", phase, err)
	}
	actual, err := dirDigest(p)
	if err != nil {
		return nil, relFail("phase %s: the files of the hub -sdk path %q: %v", phase, p, err)
	}
	expected, err := r.sdkDigestAt(commit)
	if err != nil {
		return nil, relFail("phase %s: the digest of %s:sdk/lua: %v", phase, commit, err)
	}
	if err := r.sameHub(want, "phase "+phase+", after the hashing"); err != nil {
		return nil, err
	}
	rec := &relSDKCheck{Phase: phase, PID: want.PID, Start: want.Start, Client: want.Client, Seq: want.Seq,
		Commit: commit, Path: p, Expected: expected, Actual: actual}
	if actual != expected {
		return rec, relFail("phase %s: the hub pid %d runs the SDK at %s with digest %s, not the digest %s of %s:sdk/lua",
			phase, want.PID, p, actual, expected, commit)
	}
	return rec, nil
}

// checkSDK runs the SDK byte check of a rehearsal check point and keeps its record. The bound hub
// is the hub record file of the lab run, or with file "" the hub of the last hub_started event (a
// rollback or a resume started OLD). The -sdk path must be inside the lab.
func (h *rehearsal) checkSDK(phase, file, commit string) error {
	return h.withLab(func(lr *releaser) error {
		var want *relHub
		if file != "" {
			if want = h.readLabHub(file); want == nil {
				return relFail("phase %s: no hub record %s in the lab run", phase, file)
			}
		} else {
			var err error
			if want, err = lr.currentHub(); err != nil {
				return relFail("phase %s: %v", phase, err)
			}
		}
		rec, err := lr.checkSDK(phase, want, commit, h.lab)
		if rec != nil {
			h.sdk = append(h.sdk, *rec)
		}
		if err != nil {
			return err
		}
		h.say("phase %s: the hub pid %d runs the SDK bytes of %s (%s)", phase, want.PID, commit[:12], rec.Actual)
		return nil
	})
}
