package main

// Provenance of the binaries of a release (docs/releasing.md): a hub binary must be a clean build of
// its commit. Go records the VCS state in each binary (debug/buildinfo): vcs.revision is the commit,
// and vcs.modified is true when the tree had changes at the build.

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const relProvenanceFile = "provenance.txt"

// readBuildInfo returns the vcs.revision and vcs.modified of a Go binary.
func readBuildInfo(path string) (revision string, modified bool, err error) {
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	seen := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified, seen = s.Value == "true", true
		}
	}
	if revision == "" || !seen {
		return "", false, errors.New("the binary has no VCS build information (vcs.revision, vcs.modified)")
	}
	return revision, modified, nil
}

// checkProvenance checks that bin is a clean build of commit, then appends one record to
// <run>/provenance.txt: label, time, sha256, path, revision.
func (r *releaser) checkProvenance(bin, commit, label string) error {
	read := r.buildInfo
	if read == nil {
		read = readBuildInfo
	}
	rev, modified, err := read(bin)
	if err != nil {
		return relFail("the build information of %s: %v", bin, err)
	}
	if rev != commit {
		return relFail("%s is built from %s, not from %s", bin, rev, commit)
	}
	if modified {
		return relFail("%s is not a clean build (vcs.modified is true)", bin)
	}
	sum, err := sha256File(bin)
	if err != nil {
		return relFail("the sha256 of %s: %v", bin, err)
	}
	line := fmt.Sprintf("%s %s sha256 %s %s vcs.revision=%s vcs.modified=false\n", label, r.now().UTC().Format("2006-01-02T15:04:05Z"), sum, bin, rev)
	f, err := os.OpenFile(filepath.Join(r.run, relProvenanceFile), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		return relFail("%s is not written: %v", relProvenanceFile, err)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	r.say("provenance %s: sha256 %s, vcs.revision=%s, vcs.modified=false", label, sum, rev)
	return nil
}
