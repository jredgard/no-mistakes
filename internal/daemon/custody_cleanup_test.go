package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCancelledPrePushCleanupPreservesHeadBeforeRemovingWorktree(t *testing.T) {
	for _, conflicting := range []bool{false, true} {
		t.Run(map[bool]string{false: "anchors head", true: "retains conflicting checkout"}[conflicting], func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Close() })
			repo, submitted := setupTestGitRepo(t, p, d, "cleanup-custody")
			run, err := d.InsertRun(repo.ID, "feature", submitted, submitted)
			if err != nil {
				t.Fatal(err)
			}
			wt := p.WorktreeDir(repo.ID, run.ID)
			gate := p.RepoDir(repo.ID)
			if err := git.WorktreeAdd(context.Background(), gate, wt, submitted); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, wt, "config", "user.email", "test@test.com")
			gitCmd(t, wt, "config", "user.name", "Test")
			if err := os.WriteFile(filepath.Join(wt, "unpublished.txt"), []byte("only pipeline copy\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, wt, "add", "unpublished.txt")
			gitCmd(t, wt, "commit", "-m", "unpublished fix")
			head := gitOutput(t, wt, "rev-parse", "HEAD")
			if err := d.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
				t.Fatal(err)
			}
			if conflicting {
				gitCmd(t, wt, "update-ref", custody.RecoveryRef(run.ID), submitted)
			}
			m := NewRunManager(d, p, nil)
			m.removeRunWorktree(repo.ID, run.ID, gate, wt, "test cancellation")
			if conflicting {
				if got := gitOutput(t, wt, "rev-parse", "HEAD"); got != head {
					t.Fatalf("retained HEAD = %s, want %s", got, head)
				}
				if removableOrphanWorktree(d, orphanWorktree{gateDir: gate, dir: wt, repoID: repo.ID, runID: run.ID}) {
					t.Fatal("startup cleanup would delete retained head")
				}
			} else {
				if _, err := os.Stat(wt); !os.IsNotExist(err) {
					t.Fatalf("worktree not reclaimed: %v", err)
				}
				gitCmd(t, gate, "reflog", "expire", "--expire=now", "--all")
				gitCmd(t, gate, "gc", "--prune=now")
				if got := gitOutput(t, gate, "rev-parse", custody.RecoveryRef(run.ID)); got != head {
					t.Fatalf("recoverable ref = %s, want %s", got, head)
				}
				if got := gitOutput(t, gate, "show", custody.RecoveryRef(run.ID)+":unpublished.txt"); got != "only pipeline copy" {
					t.Fatalf("preserved content = %q", got)
				}
			}
		})
	}
}
