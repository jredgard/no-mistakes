//go:build e2e

package steps

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

// Uses a real installed Node binary. A caller may provide Node 22 explicitly
// to reproduce the reported Node 24 host / Node 22 repository mismatch.
func TestTestStep_RealPinnedNodeCommand(t *testing.T) {
	node := os.Getenv("NO_MISTAKES_TEST_NODE_PATH")
	if node == "" {
		var err error
		node, err = exec.LookPath("node")
		if err != nil {
			t.Skip("integration requires an installed Node executable")
		}
	}
	output, err := exec.Command(node, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	version, count, err := parseNodeVersion(strings.TrimSpace(string(output)))
	if err != nil || count != 3 {
		t.Fatalf("invalid installed version: %q (%v)", output, err)
	}
	t.Logf("installed Node selected for command integration: %s", version.String())
	dir, baseSHA, headSHA := setupGitRepo(t)
	home := t.TempDir()
	name := "node"
	if runtime.GOOS == "windows" {
		name = "node.exe"
	}
	bin := filepath.Join(home, ".nvm", "versions", "node", "v"+version.String(), "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	// A concrete configured path also exercises Windows, where nvm's layout
	// differs. POSIX exercises automatic nvm discovery from .node-version.
	installed := filepath.Join(bin, name)
	if err := copyFile(node, installed, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".node-version"), []byte(version.String()+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var logs []string
	called := false
	ag := &mockAgent{name: "runtime-evidence", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		called = true
		out, code, err := runShellCommandWithEnv(ctx, opts.CWD, opts.Env, "node -p process.version")
		if err != nil || code != 0 || strings.TrimSpace(out) != "v"+version.String() {
			t.Fatalf("agent command: %q, %d, %v", out, code, err)
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"runtime verified","tested":["node -p process.version"],"testing_summary":"runtime verified","artifacts":[],"scenarios":[{"name":"runtime fixture","result":"pass","live":true,"evidence":"process output","reason":""}],"verdict":"go"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "node -p process.version"})
	sctx.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "NVM_DIR=" + filepath.Join(home, ".nvm")}
	if runtime.GOOS == "windows" {
		sctx.Config.Runtime = config.Runtime{Node: version.String(), NodePath: installed}
	}
	sctx.Log = func(line string) { logs = append(logs, line) }
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || outcome.ExitCode != 0 || !called {
		t.Fatalf("outcome=%+v, evidence called=%v", outcome, called)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "Runtime: Node "+version.String()) || !strings.Contains(strings.Join(logs, "\n"), "v"+version.String()) {
		t.Fatalf("missing runtime/command evidence: %v", logs)
	}
}
