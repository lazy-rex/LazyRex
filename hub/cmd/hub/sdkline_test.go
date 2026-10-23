package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The hub's SDK start line (the onboarding dry run, 2026-10-22).
func TestSDKLogLine(t *testing.T) {
	if got := sdkLogLine("/home/u/.lazyrex/sdk/lua"); got != "sdk: /home/u/.lazyrex/sdk/lua" {
		t.Errorf("a directory: %q", got)
	}
	if got := sdkLogLine(""); got != "sdk: none" {
		t.Errorf("none: %q", got)
	}
}

// An explicit -sdk resolves through links to a directory with boot.lua (the lab test
// TestRestartCommandLab shows that a later switch of the link changes nothing). A path that cannot
// be resolved, or a directory without boot.lua, stops the start.
func TestFindSDK(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	v1, v2 := filepath.Join(dir, "v1", "sdk", "lua"), filepath.Join(dir, "v2", "sdk", "lua")
	for _, d := range []string{v1, v2} {
		os.MkdirAll(d, 0o700)
		os.WriteFile(filepath.Join(d, "boot.lua"), nil, 0o600)
	}
	link := filepath.Join(dir, "current")
	os.Symlink(filepath.Join(dir, "v1"), link)
	got, err := findSDK(filepath.Join(link, "sdk", "lua"))
	if err != nil || got != v1 {
		t.Fatalf("through the link: %q, %v; want %s", got, err, v1)
	}
	if _, err := findSDK(filepath.Join(dir, "missing")); err == nil || !strings.Contains(err.Error(), "cannot be resolved") {
		t.Errorf("a missing -sdk: %v", err)
	}
	empty := filepath.Join(dir, "empty")
	os.Mkdir(empty, 0o700)
	if _, err := findSDK(empty); err == nil || !strings.Contains(err.Error(), "has no boot.lua") {
		t.Errorf("-sdk without boot.lua: %v", err)
	}
}

// The view path resolves through links; one that cannot be resolved stops the start; no view
// stays no view.
func TestResolveView(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "v1", "lazyrex-view")
	os.MkdirAll(filepath.Dir(real), 0o700)
	os.WriteFile(real, nil, 0o700)
	link := filepath.Join(dir, "view")
	os.Symlink(real, link)
	if got, err := resolveView(link); err != nil || got != real {
		t.Errorf("through the link: %q, %v; want %s", got, err, real)
	}
	if _, err := resolveView(filepath.Join(dir, "missing")); err == nil || !strings.Contains(err.Error(), "cannot be resolved") {
		t.Errorf("a missing view: %v", err)
	}
	if got, err := resolveView(""); got != "" || err != nil {
		t.Errorf("no view: %q, %v", got, err)
	}
}
