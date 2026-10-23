package main

import "time"

// relRetiredTimeout bounds the run of "hub-H -retired-schema".
var relRetiredTimeout = 30 * time.Second

// relRetiredRec records the -retired-schema export of the H binary that a hub rehearsal read: the
// commit, the sha256 of the binary and of the output, and the entries. Absent: the binary has no
// export (an older hub), so no OLD table or column may go.
type relRetiredRec struct {
	Commit       string         `json:"commit"`
	BinSHA256    string         `json:"bin_sha256"`
	OutputSHA256 string         `json:"output_sha256"`
	Absent       bool           `json:"absent"`
	Entries      []retiredEntry `json:"entries"`
}

// exportRetired runs "bin -retired-schema" in the empty directory dir. Seam: not yet implemented.
func exportRetired(bin, dir string) (*retiredExport, []byte, bool, error) {
	return nil, nil, false, nil
}

// loadRetired reads the export of the H binary bin once. Seam: not yet implemented.
func (h *rehearsal) loadRetired(bin string, prov func(string) error) error { return nil }
