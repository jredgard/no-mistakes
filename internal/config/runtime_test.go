package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestRuntimeConfigRoundTripAndTrust(t *testing.T) {
	pushed, err := LoadRepoFromBytes([]byte("runtime:\n  node: '24.22.0'\n"))
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := LoadRepoFromBytes([]byte("runtime:\n  node: '22.23.1'\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, allow := range []bool{false, true} {
		effective := EffectiveRepoConfig(pushed, trusted, allow)
		if got := Merge(&GlobalConfig{}, effective).Runtime.Node; got != "22.23.1" {
			t.Fatalf("allow=%v: Node=%q", allow, got)
		}
		if got := EffectiveRepoConfig(pushed, nil, allow).Runtime; got != (Runtime{}) {
			t.Fatalf("pushed runtime leaked without trusted config: %+v", got)
		}
	}
	if pushed.Runtime.Node != "24.22.0" {
		t.Fatal("mutated pushed config")
	}
}

func TestRuntimeConfigValidation(t *testing.T) {
	for _, pin := range []string{"22", ">=22", "lts/*", "../../node", "22.23.1; echo unsafe", "22.23.1\n"} {
		if err := (Runtime{Node: pin}).Validate(); err == nil {
			t.Errorf("accepted pin %q", pin)
		}
	}
	for _, cfg := range []Runtime{
		{NodePath: filepath.Join(t.TempDir(), "node")},
		{Node: "22.23.1", NodePath: "relative/node"},
		{Node: "22.23.1", NodePath: filepath.Join(t.TempDir(), "other-binary")},
	} {
		if err := cfg.Validate(); err == nil {
			t.Errorf("accepted %+v", cfg)
		}
	}
	name := "node"
	if runtime.GOOS == "windows" {
		name = "node.exe"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := (Runtime{Node: "v22.23.1", NodePath: path}).Validate(); err != nil {
		t.Fatal(err)
	}
}
