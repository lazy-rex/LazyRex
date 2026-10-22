package main

import (
	"bytes"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// The files of a new plugin. Each template gets {{.Name}}. tests/test.lua.tmpl becomes
// tests/<name>.lua.
//
//go:embed plugintemplate
var pluginTemplates embed.FS

// newPluginFiles renders the template files of the plugin name: plugin path to content.
func newPluginFiles(name string) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := fs.WalkDir(pluginTemplates, "plugintemplate", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		src, err := pluginTemplates.ReadFile(p)
		if err != nil {
			return err
		}
		t, err := template.New(p).Option("missingkey=error").Parse(string(src))
		if err != nil {
			return err
		}
		var b bytes.Buffer
		if err := t.Execute(&b, struct{ Name string }{name}); err != nil {
			return err
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(p, "plugintemplate/"), ".tmpl")
		if rel == "tests/test.lua" {
			rel = "tests/" + name + ".lua"
		}
		out[rel] = b.Bytes()
		return nil
	})
	return out, err
}

// pluginNew is lazyrex-hub plugin new <name> [-dir <path>]. It writes the files of a new plugin
// into a stage directory beside the destination, then renames the stage to the destination. So
// the destination has every file or none: a failure removes the stage. The destination must not
// exist, also not as an empty directory: os.Rename refuses an existing directory, so one that
// appears before the rename is refused too.
func pluginNew(name, dir string, w io.Writer) error {
	fail := func(err error) error { return fmt.Errorf("plugin new: %w; nothing is written", err) }
	if !pluginName.MatchString(name) {
		return fail(fmt.Errorf("the name %q must match [a-z0-9_-]+", name))
	}
	if dir == "" {
		dir = name
	}
	dest, err := filepath.Abs(dir)
	if err != nil {
		return fail(err)
	}
	if _, err := os.Lstat(dest); err == nil {
		return fail(fmt.Errorf("%s exists", dest))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fail(err)
	}
	files, err := newPluginFiles(name)
	if err != nil {
		return fail(err)
	}
	// The stage is in the destination's parent, so the rename stays on one filesystem.
	stage, err := os.MkdirTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".new-")
	if err != nil {
		return fail(err)
	}
	if err := writePluginFiles(stage, files); err != nil {
		os.RemoveAll(stage)
		return fail(err)
	}
	if err := os.Rename(stage, dest); err != nil {
		os.RemoveAll(stage)
		return fail(fmt.Errorf("%s: %w", dest, err))
	}
	if err := writeLines(w, []string{"plugin new: wrote " + dest + "\n", "Next: run lazyrex-hub test in that directory.\n"}); err != nil {
		return fmt.Errorf("plugin new: %s is written, but the result lines are not: %w", dest, err)
	}
	return nil
}

// writeNewFile is the file write of writePluginFiles; a test replaces it to inject a failure.
var writeNewFile = os.WriteFile

// writePluginFiles writes the files under root, with mode 0755 for the stage and its directories
// and 0644 for the files.
func writePluginFiles(root string, files map[string][]byte) error {
	if err := os.Chmod(root, 0o755); err != nil {
		return err
	}
	for rel, b := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := writeNewFile(p, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// pluginNewMain parses plugin new <name> [-dir <path>]. Exit 0: written; 1: refused or failed;
// 2: a usage error.
func pluginNewMain(args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintln(stderr, "usage: lazyrex-hub plugin new <name> [-dir <path>]")
		return 2
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usage()
	}
	flags := flag.NewFlagSet("plugin new", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("dir", "", "the directory of the new plugin (default: ./<name>)")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() > 0 {
		return usage()
	}
	if err := pluginNew(args[0], *dir, stdout); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
