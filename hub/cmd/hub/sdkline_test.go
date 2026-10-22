package main

import "testing"

// The hub's SDK start line (the onboarding dry run, 2026-10-22).
func TestSDKLogLine(t *testing.T) {
	if got := sdkLogLine("/home/u/.lazyrex/sdk/lua"); got != "sdk: /home/u/.lazyrex/sdk/lua" {
		t.Errorf("a directory: %q", got)
	}
	if got := sdkLogLine(""); got != "sdk: none" {
		t.Errorf("none: %q", got)
	}
}
