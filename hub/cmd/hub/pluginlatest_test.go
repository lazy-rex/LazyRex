package main

import "testing"

func TestParseVersionTag(t *testing.T) {
	good := map[string]tagVersion{
		"v0.0.0":                              {"0", "0", "0"},
		"v1.2.3":                              {"1", "2", "3"},
		"v10.0.20":                            {"10", "0", "20"},
		"v123456789012345678901234567890.0.1": {"123456789012345678901234567890", "0", "1"},
	}
	for tag, want := range good {
		if got, ok := parseVersionTag(tag); !ok || got != want {
			t.Errorf("parseVersionTag(%q) = %v, %v; want %v, true", tag, got, ok, want)
		}
	}
	bad := []string{
		"", "v", "1.2.3", "V1.2.3", "v1.2", "v1.2.3.4", "v01.2.3", "v1.02.3", "v1.2.03", "v00.0.0",
		"v1.2.3-rc.1", "v1.2.3+build.5", "v1.2.3-rc.1+b", "v-1.2.3", "v1..3", "v1.2.x", " v1.2.3",
		"v1.2.3 ", "v1.2.3\n", "refs/tags/v1.2.3", "v1.2.3^{}", "release-1.2.3", "latest", "v١.2.3",
	}
	for _, tag := range bad {
		if got, ok := parseVersionTag(tag); ok {
			t.Errorf("parseVersionTag(%q) = %v, true; want false", tag, got)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	// Each case is lower < higher.
	cases := [][2]string{
		{"v1.9.0", "v1.10.0"},
		{"v9.99.99", "v10.0.0"},
		{"v0.0.0", "v0.0.1"},
		{"v0.9.9", "v1.0.0"},
		{"v1.2.9", "v1.2.10"},
		{"v1.2.3", "v1.3.0"},
		{"v99999999999999999999.0.0", "v100000000000000000000.0.0"},
		{"v1.0.99999999999999999999", "v1.0.100000000000000000000"},
	}
	for _, c := range cases {
		lo, ok1 := parseVersionTag(c[0])
		hi, ok2 := parseVersionTag(c[1])
		if !ok1 || !ok2 {
			t.Fatalf("parse %q %q: %v %v", c[0], c[1], ok1, ok2)
		}
		if got := compareVersions(lo, hi); got != -1 {
			t.Errorf("compareVersions(%s, %s) = %d, want -1", c[0], c[1], got)
		}
		if got := compareVersions(hi, lo); got != 1 {
			t.Errorf("compareVersions(%s, %s) = %d, want 1", c[1], c[0], got)
		}
		if got := compareVersions(lo, lo); got != 0 {
			t.Errorf("compareVersions(%s, %s) = %d, want 0", c[0], c[0], got)
		}
	}
}
