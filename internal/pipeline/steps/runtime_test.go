package steps

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func TestRepositoryNodePinPrecedence(t *testing.T) {
	dir := t.TempDir()
	for name, text := range map[string]string{
		".node-version": "22.23.1\n", ".nvmrc": "v20.19.0\n", "package.json": `{"engines":{"node":">=18 <20"}}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		cfg                 config.Runtime
		remove, pin, source string
	}{
		{config.Runtime{Node: "24.22.0"}, "", "24.22.0", "runtime.node"},
		{config.Runtime{}, "", "22.23.1", ".node-version"},
		{config.Runtime{}, ".node-version", "v20.19.0", ".nvmrc"},
		{config.Runtime{}, ".nvmrc", ">=18 <20", "package.json engines.node"},
		{config.Runtime{}, "package.json", "", ""},
	} {
		if tc.remove != "" {
			if err := os.Remove(filepath.Join(dir, tc.remove)); err != nil {
				t.Fatal(err)
			}
		}
		pin, source, err := repositoryNodePin(dir, tc.cfg)
		if err != nil || pin != tc.pin || source != tc.source {
			t.Fatalf("got %q from %q (%v), want %q from %q", pin, source, err, tc.pin, tc.source)
		}
	}
}

func TestRepositoryNodePinUnreadableDoesNotFallThrough(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".node-version"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".nvmrc"), []byte("22.23.1"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repositoryNodePin(dir, config.Runtime{}); err == nil {
		t.Fatal("unreadable pin fell through")
	}
}

func TestRuntimePATHCompositionAndRestoration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture; PATH composition is covered on Windows by TestPrependRuntimePath")
	}
	dir := t.TempDir()
	node := fakeRuntimeNode(t, filepath.Join(t.TempDir(), "bin", "node"), "22.23.1")
	original := []string{"KEEP=value", "PATH=" + os.Getenv("PATH")}
	var logs []string
	sctx := &pipeline.StepContext{Ctx: context.Background(), WorkDir: dir, Config: &config.Config{Runtime: config.Runtime{Node: "22.23.1", NodePath: node}}, Env: original, Log: func(line string) { logs = append(logs, line) }}
	restore, err := prepareRepositoryRuntime(sctx)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := envValue(sctx.Env, "PATH")
	if path != filepath.Dir(node)+string(os.PathListSeparator)+os.Getenv("PATH") {
		t.Fatalf("PATH=%q", path)
	}
	if value, _ := envValue(sctx.Env, "KEEP"); value != "value" {
		t.Fatal("lost other environment")
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "Node 22.23.1") {
		t.Fatalf("logs=%v", logs)
	}
	output, code, err := runStepShellCommand(sctx, "node --version")
	if err != nil || code != 0 || strings.TrimSpace(output) != "v22.23.1" {
		t.Fatalf("command: %q, %d, %v", output, code, err)
	}
	restore()
	if !reflect.DeepEqual(sctx.Env, original) {
		t.Fatal("environment was not restored")
	}
}

func TestPrependRuntimePath(t *testing.T) {
	for _, env := range [][]string{nil, {"KEEP=value"}, {"PATH=", "KEEP=value"}, {"PATH=old", "PATH=stale", "KEEP=value"}} {
		out := prependRuntimePath(env, "selected")
		path, _ := envValue(out, "PATH")
		want := "selected"
		if old, _ := envValue(env, "PATH"); old != "" {
			want += string(os.PathListSeparator) + old
		}
		if path != want {
			t.Fatalf("PATH=%q want %q", path, want)
		}
		count := 0
		for _, entry := range out {
			if envKey(entry) == envKey("PATH=") {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("duplicate PATH: %v", out)
		}
	}
}

func TestInstalledNodeManagerOrderAndRanges(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX manager layout fixtures")
	}
	home := t.TempDir()
	env := []string{"HOME=" + home, "XDG_DATA_HOME=" + filepath.Join(home, "data"), "PATH=" + filepath.Join(home, "outer")}
	nvm := fakeRuntimeNode(t, filepath.Join(home, ".nvm", "versions", "node", "v22.23.1", "bin", "node"), "22.23.1")
	fnm := fakeRuntimeNode(t, filepath.Join(home, "data", "fnm", "node-versions", "v22.24.0", "installation", "bin", "node"), "22.24.0")
	volta := fakeRuntimeNode(t, filepath.Join(home, ".volta", "tools", "image", "node", "20.19.0", "bin", "node"), "20.19.0")
	fakeRuntimeNode(t, filepath.Join(home, "outer", "node"), "24.22.0")
	for _, tc := range []struct{ pin, want string }{{"22.23.1", nvm}, {"^22.0.0", fnm}, {"20.19.0", volta}} {
		match, err := compileNodeRange(tc.pin)
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := resolveInstalledNode(context.Background(), env, "", match)
		if err != nil || got != tc.want {
			t.Fatalf("%s: %q, %v, want %q", tc.pin, got, err, tc.want)
		}
	}
	match, _ := compileNodeRange("22.23.1")
	if _, _, err := resolveInstalledNode(context.Background(), env, fnm, match); err == nil {
		t.Fatal("explicit wrong-version path fell through to nvm")
	}
	match, _ = compileNodeRange("18.0.0")
	if _, _, err := resolveInstalledNode(context.Background(), env, "", match); err == nil {
		t.Fatal("missing pin used outer Node")
	}
}

func TestRuntimeMissingFailsValidationSteps(t *testing.T) {
	name := "node"
	if runtime.GOOS == "windows" {
		name = "node.exe"
	}
	for _, step := range []pipeline.Step{&TestStep{}, &ReviewStep{}, &DocumentStep{}, &LintStep{}} {
		t.Run(string(step.Name()), func(t *testing.T) {
			ctx := &pipeline.StepContext{Ctx: context.Background(), WorkDir: t.TempDir(), Config: &config.Config{Runtime: config.Runtime{Node: "22.23.1", NodePath: filepath.Join(t.TempDir(), name)}}}
			if _, err := step.Execute(ctx); err == nil || !strings.Contains(err.Error(), "22.23.1") || !strings.Contains(err.Error(), "install the pinned version") {
				t.Fatalf("missing runtime result: %v", err)
			}
		})
	}
}

func TestNoRuntimePinLeavesEnvironmentUntouched(t *testing.T) {
	env := []string{"PATH=original"}
	sctx := &pipeline.StepContext{Ctx: context.Background(), WorkDir: t.TempDir(), Config: &config.Config{}, Env: env, Log: func(string) { t.Fatal("logged runtime without pin") }}
	restore, err := prepareRepositoryRuntime(sctx)
	if err != nil {
		t.Fatal(err)
	}
	restore()
	if !reflect.DeepEqual(sctx.Env, env) {
		t.Fatal("changed unpinned environment")
	}
}

func fakeRuntimeNode(t *testing.T, path, version string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' 'v"+version+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNodeVersionRanges(t *testing.T) {
	for _, tc := range []struct {
		pin     string
		yes, no []string
	}{
		{"22.23.1", []string{"22.23.1"}, []string{"22.23.2", "24.22.0"}},
		{"22.x", []string{"22.23.1"}, []string{"23.0.0"}},
		{"~22.23.1", []string{"22.23.2"}, []string{"22.24.0", "22.23.0"}},
		{"^22.23.1", []string{"22.24.0"}, []string{"23.0.0"}},
		{">= 22.23.1 < 23 || ^24.0.0", []string{"22.23.1", "24.22.0"}, []string{"22.23.0", "23.0.0", "25.0.0"}},
		{"^0.2.1", []string{"0.2.3"}, []string{"0.3.0"}},
		{"<=22", []string{"22.23.1"}, []string{"23.0.0"}},
	} {
		match, err := compileNodeRange(tc.pin)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range tc.yes {
			parsed, _, _ := parseNodeVersion(v)
			if !match(parsed) {
				t.Errorf("%s should match %s", v, tc.pin)
			}
		}
		for _, v := range tc.no {
			parsed, _, _ := parseNodeVersion(v)
			if match(parsed) {
				t.Errorf("%s should not match %s", v, tc.pin)
			}
		}
	}
	for _, pin := range []string{"lts/*", "node", "system", "22.1.0-beta", "22; echo unsafe", "22 - 24", ">=garbage", "", "22 ||"} {
		if _, err := compileNodeRange(pin); err == nil {
			t.Errorf("accepted %q", pin)
		}
	}
}
