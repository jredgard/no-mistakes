//go:build e2e

package e2e

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The local-target harness parks Review rather than running a hosted CI
// monitor. Seed its exact publication contract using a real push and persisted
// binding, then exercise the public abort/sync/rebase/fresh-run interfaces.
func TestAxiAbortPublishedThenRebaseThenFreshRunOnSameBranch(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	branch := "feature/abort-published-rebase"
	h.CommitChange(branch, "feature.txt", "unsafe\n", "add feature")
	operator := h.AddWorktree(branch)
	if out, err := h.RunInDir(operator, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(operator, "axi", "run", "--intent", "preserve fixes through cancellation and rebase"); err != nil || !strings.Contains(out, "sync-1") {
		t.Fatalf("review gate: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "sync-1"); err != nil {
		t.Fatalf("fix: %v\n%s", err, out)
	}
	run := h.ActiveRun(branch)
	if run == nil {
		t.Fatal("no active run")
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := h.runGit(context.Background(), dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	git(gateDir, "push", h.UpstreamDir, run.HeadSHA+":refs/heads/"+branch)
	git(gateDir, "update-ref", "refs/heads/"+branch, run.HeadSHA)
	database, err := db.Open(filepath.Join(h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.UpdateRunPushBinding(run.ID, db.PushBinding{
		HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(h.UpstreamDir), Ref: "refs/heads/" + branch,
	}); err != nil {
		t.Fatal(err)
	}
	git(operator, "fetch", "origin", branch)
	git(operator, "merge", "--ff-only", run.HeadSHA)
	if out, err := h.RunInDir(operator, "axi", "abort"); err != nil || !strings.Contains(out, "recover_custody") {
		t.Fatalf("abort did not offer custody recovery: %v\n%s", err, out)
	}
	if terminal := h.WaitForRun(branch, 30*time.Second); terminal.Status != types.RunCancelled {
		t.Fatalf("status = %s", terminal.Status)
	}
	if out, err := h.RunInDir(operator, "axi", "sync", "--recover"); err != nil || !strings.Contains(out, "state: user_owned") {
		t.Fatalf("recover: %v\n%s", err, out)
	}
	h.CommitChange("main", "advanced-base.txt", "new base\n", "advance base")
	git(h.WorkDir, "push", "origin", "main")
	git(operator, "fetch", "origin", "main")
	git(operator, "rebase", "origin/main")
	rebased := git(operator, "rev-parse", "HEAD")
	if rebased == run.HeadSHA {
		t.Fatal("rebase did not rewrite the published head")
	}
	if out, err := h.RunInDir(operator, "axi", "sync", "--check"); err != nil || !strings.Contains(out, "state: user_owned") {
		t.Fatalf("rebased branch stranded: %v\n%s", err, out)
	}
	if got := git(operator, "show", "HEAD:feature.txt"); got != "safe" {
		t.Fatalf("pipeline fix lost: %s", got)
	}
	if out, err := h.RunInDir(operator, "axi", "run", "--base-branch", "main", "--intent", "continue the same branch after abort and rebase"); err != nil || !strings.Contains(out, "gate:") {
		t.Fatalf("fresh run: %v\n%s", err, out)
	}
	if fresh := h.ActiveRun(branch); fresh == nil || fresh.ID == run.ID || fresh.SubmittedHeadSHA == nil || *fresh.SubmittedHeadSHA != rebased {
		t.Fatalf("fresh run = %+v", fresh)
	}
	if out, err := h.RunInDir(operator, "axi", "abort"); err != nil {
		t.Fatalf("cleanup abort: %v\n%s", err, out)
	}
}
