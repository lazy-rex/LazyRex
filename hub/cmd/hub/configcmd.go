package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// configCmdMain is `lazyrex-hub config <hub.json>` (lead-devsh4): it reads the config with
// loadConfig, the code of the hub start, and prints what dev.sh needs, one record a line, the key
// first and the fields separated by one TAB:
//
//	server<TAB><value>
//	state<TAB><absolute path>
//	view<TAB><absolute path>              (only when set)
//	plugin<TAB><name><TAB><absolute dir>  (each local entry, config then managed, in load order)
//
// The name is the manifest's, read as the hub reads it (readPluginManifest). A local entry whose
// manifest the hub would refuse has no plugin line, and a warning on stderr: the hub starts with
// that entry refused, so the command does not fail. It starts nothing and changes no file. A relative
// state is resolved against the working directory, as the hub resolves it: run it where the hub runs.
// A config that loadConfig refuses, or a value with a TAB or a newline, exits 1 with one stderr line.
func configCmdMain(argv []string, stdout, stderr io.Writer) int {
	if len(argv) != 1 || strings.HasPrefix(argv[0], "-") {
		fmt.Fprintln(stderr, "usage: lazyrex-hub config <hub.json>")
		return 2
	}
	c, err := loadConfig(argv[0])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	state, err := filepath.Abs(c.State)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if c.managedErr != nil {
		fmt.Fprintf(stderr, "warning: %v\n", c.managedErr)
	}
	var lines [][]string
	lines = append(lines, []string{"server", c.Server}, []string{"state", state})
	if c.View != "" {
		lines = append(lines, []string{"view", c.View})
	}
	for _, e := range c.entries {
		if e.github || e.dir == "" {
			continue
		}
		m, _, err := readPluginManifest(e.dir)
		if err != nil {
			fmt.Fprintf(stderr, "warning: %s: %v\n", e.identity(), err)
			continue
		}
		lines = append(lines, []string{"plugin", m.Name, e.dir})
	}
	for _, l := range lines {
		for _, f := range l[1:] {
			if strings.ContainsAny(f, "\t\n\r") {
				fmt.Fprintf(stderr, "%s: the %s value %q has a TAB or a line break\n", argv[0], l[0], f)
				return 1
			}
		}
	}
	for _, l := range lines {
		fmt.Fprintln(stdout, strings.Join(l, "\t"))
	}
	return 0
}
