package branchsync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCancelledPublishedBranchReturnsCustodyWithEveryPipelineCommitLocal(t *testing.T) {
	t.Parallel()
	for _, rebased := range []bool{false, true} {
		t.Run(map[bool]string{false: "equal", true: "rebased"}[rebased], func(t *testing.T) {
			f := newSyncFixture(t)
			mustRun(t, f.local, "fetch", f.remote, "refs/heads/feature/sync")
			mustRun(t, f.local, "merge", "--ff-only", f.pushed)
			if rebased {
				mustRun(t, f.local, "checkout", "main")
				mustWrite(t, filepath.Join(f.local, "base-advance.txt"), "new base\n")
				mustRun(t, f.local, "add", "base-advance.txt")
				mustRun(t, f.local, "commit", "-m", "advance base")
				mustRun(t, f.local, "checkout", "feature/sync")
				mustRun(t, f.local, "rebase", "main")
			}
			local := mustRun(t, f.local, "rev-parse", "HEAD")
			if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunCancelled, f.pushed); err != nil {
				t.Fatal(err)
			}
			state := f.service.Refresh(f.ctx)
			if state.NextAction == nil || state.NextAction.Code != "recover_custody" {
				t.Fatalf("cancelled published branch has no supported recovery: %+v", state)
			}
			recovered := f.service.Recover(f.ctx, false)
			if !recovered.Recovered || recovered.Changed || recovered.State != StateUserOwned {
				t.Fatalf("recovery = %+v", recovered)
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != local {
				t.Fatalf("recovery moved local head: %s != %s", got, local)
			}
			if got := mustRun(t, f.local, "rev-parse", "refs/no-mistakes/recover/"+f.run.ID); got != f.pushed {
				t.Fatalf("preservation anchor = %s, want %s", got, f.pushed)
			}
		})
	}
}

func TestCancelledPublishedRecoveryFailsClosed(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"active", "unverified", "missing fix", "dirty", "anchor conflict", "remote advanced", "branch race", "new run race"} {
		t.Run(mode, func(t *testing.T) {
			f := newSyncFixture(t)
			mustRun(t, f.local, "fetch", f.remote, "refs/heads/feature/sync")
			mustRun(t, f.local, "merge", "--ff-only", f.pushed)
			if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunCancelled, f.pushed); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "active":
				if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
			case "unverified":
				if err := f.db.UpdateRunStatus(f.run.ID, types.RunCancelled); err != nil {
					t.Fatal(err)
				}
				// Use an initially unverified terminal row, as an older
				// daemon would record.
				run, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.pushed, f.base)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.db.UpdateRunPushBinding(run.ID, db.PushBinding{HeadSHA: f.pushed, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/sync"}); err != nil {
					t.Fatal(err)
				}
				if err := f.db.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
					t.Fatal(err)
				}
				f.run = run
			case "missing fix":
				mustRun(t, f.local, "reset", "--hard", f.old)
			case "dirty":
				mustWrite(t, filepath.Join(f.local, "dirty.txt"), "dirty\n")
			case "anchor conflict":
				mustRun(t, f.local, "update-ref", "refs/no-mistakes/recover/"+f.run.ID, f.old)
			case "remote advanced":
				mustRun(t, f.local, "commit", "--allow-empty", "-m", "external push")
				mustRun(t, f.local, "push", f.remote, "HEAD:refs/heads/feature/sync")
			case "branch race":
				f.service.beforeRecoverRebind = func() { mustRun(t, f.local, "commit", "--allow-empty", "-m", "concurrent local commit") }
			case "new run race":
				f.service.beforeRecoverRebind = func() {
					if _, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.pushed, f.base); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := mustRun(t, f.local, "rev-parse", "HEAD")
			if recovered := f.service.Recover(f.ctx, false); recovered.Recovered {
				t.Fatalf("unsafe %s recovery succeeded: %+v", mode, recovered)
			}
			run, err := f.db.GetRun(f.run.ID)
			if err != nil || run.CustodyReturnedAt != nil {
				t.Fatalf("custody changed: %+v, %v", run, err)
			}
			if mode != "branch race" && mustRun(t, f.local, "rev-parse", "HEAD") != before {
				t.Fatal("refusal moved the local head")
			}
			if mode == "dirty" {
				if _, err := os.Stat(filepath.Join(f.local, "dirty.txt")); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
