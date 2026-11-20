package branchsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const replayBranchRef = "refs/heads/feature/recover"

func recordReviewStarts(t *testing.T, f *recoverFixture, heads ...string) {
	t.Helper()
	step, err := f.db.InsertStepResult(f.run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	for i, head := range heads {
		if _, err := f.db.InsertReviewStepRoundWithProvenance(step.ID, i+1, "initial", nil, nil, head, head, "", nil, nil, 1); err != nil {
			t.Fatal(err)
		}
	}
}

func commitReplayFile(t *testing.T, dir, path, content string) string {
	t.Helper()
	mustWrite(t, filepath.Join(dir, path), content)
	mustRun(t, dir, "add", path)
	mustRun(t, dir, "commit", "-m", "Change "+path)
	return mustRun(t, dir, "rev-parse", "HEAD")
}

func newReviewFixReplay(t *testing.T) *recoverFixture {
	t.Helper()
	var replay string
	f := newRebasedRecoverFixtureWithPipelineWork(t, types.RunCancelled, func(t *testing.T, pipeline string) {
		replay = mustRun(t, pipeline, "rev-parse", "HEAD")
		commitReplayFile(t, pipeline, "feature.txt", "feature one\nfeature two guarded\n")
	})
	recordReviewStarts(t, f, replay)
	mustRun(t, f.local, "push", f.remote, "HEAD:"+replayBranchRef)
	mustRun(t, f.gate, "update-ref", replayBranchRef, f.submitted)
	mustRun(t, f.gate, "update-ref", f.anchorRef(), f.preserved)
	return f
}

func TestRecoverRebasedPreservedHeadWithRecordedRoundsAdoptsRewrittenOperatorLines(t *testing.T) {
	t.Parallel()
	f := newReviewFixReplay(t)
	assertReplayRecovery(t, f, f.submitted, f.submitted)
}

// A published review fix is followed by a CI merge and another edit to an
// operator line. The first Review start proves the path through the merge.
func TestRecoverReplayThroughPublishedCIRepairMerge(t *testing.T) {
	t.Parallel()
	var replay, pushed string
	f := newRebasedRecoverFixtureWithPipelineWork(t, types.RunCancelled, func(t *testing.T, pipeline string) {
		replay = mustRun(t, pipeline, "rev-parse", "HEAD")
		pushed = commitReplayFile(t, pipeline, "fix.txt", "review fix\n")
		mustRun(t, pipeline, "push", filepath.Join(filepath.Dir(pipeline), "upstream.git"), pushed+":"+replayBranchRef)
		mustRun(t, pipeline, "checkout", "-b", "new-base", "origin/main")
		commitReplayFile(t, pipeline, "base.txt", "another base update\n")
		mustRun(t, pipeline, "checkout", "feature/recover")
		mustRun(t, pipeline, "merge", "--no-ff", "-m", "Merge the updated base for CI", "new-base")
		commitReplayFile(t, pipeline, "feature.txt", "feature one\nCI repair of feature two\n")
	})
	recordReviewStarts(t, f, replay)
	bindReplayPush(t, f, pushed)
	assertReplayRecovery(t, f, pushed, pushed)
}

func bindReplayPush(t *testing.T, f *recoverFixture, pushed string) {
	t.Helper()
	mustRun(t, f.gate, "fetch", f.remote, "+"+replayBranchRef+":"+replayBranchRef)
	mustRun(t, f.gate, "update-ref", f.anchorRef(), f.preserved)
	if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{
		HeadSHA: pushed, Ref: replayBranchRef,
		TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunPRState(f.run.ID, "open"); err != nil {
		t.Fatal(err)
	}
}

func newConflictReplay(t *testing.T, dropCleanPath bool) (*recoverFixture, string) {
	t.Helper()
	var firstReview, pushed, secondReview string
	f := newRebasedRecoverFixtureWithPipelineWork(t, types.RunCancelled, func(t *testing.T, pipeline string) {
		firstReview = mustRun(t, pipeline, "rev-parse", "HEAD")
		commitReplayFile(t, pipeline, "feature.txt", "feature one\npublished review fix\n")
		commitReplayFile(t, pipeline, "keep.txt", "published clean replay value\n")
		pushed = commitReplayFile(t, pipeline, "conflict.txt", "published value\n")
		mustRun(t, pipeline, "push", filepath.Join(filepath.Dir(pipeline), "upstream.git"), pushed+":"+replayBranchRef)
		mustRun(t, pipeline, "checkout", "-b", "new-base", "origin/main")
		commitReplayFile(t, pipeline, "conflict.txt", "base now owns this file too\n")
		mustRun(t, pipeline, "checkout", "feature/recover")
		if _, err := git.Run(t.Context(), pipeline, "rebase", "new-base"); err == nil {
			t.Fatal("expected the second rebase to stop at a conflict")
		}
		mustWrite(t, filepath.Join(pipeline, "conflict.txt"), "resolved value\n")
		mustRun(t, pipeline, "add", "conflict.txt")
		mustRun(t, pipeline, "-c", "core.editor=true", "rebase", "--continue")
		if dropCleanPath {
			mustRun(t, pipeline, "rm", "keep.txt")
			mustRun(t, pipeline, "commit", "-m", "Drop a cleanly replayed change")
		}
		secondReview = mustRun(t, pipeline, "rev-parse", "HEAD")
		commitReplayFile(t, pipeline, "repair.txt", "second review fix\n")
	})
	recordReviewStarts(t, f, firstReview, secondReview)
	bindReplayPush(t, f, pushed)
	return f, pushed
}

func TestRecoverReplayWithConflictAfterPublishingOffersExplicitChoice(t *testing.T) {
	t.Parallel()
	f, pushed := newConflictReplay(t, false)
	state := f.service.InspectCached(f.ctx)
	if state.NextAction == nil || state.NextAction.Command != "no-mistakes axi sync --recover --keep-local" ||
		!strings.Contains(state.Error, "--take-pipeline") || !strings.Contains(state.Error, "conflict.txt") {
		t.Fatalf("expected both recovery choices and the conflicting path, got %#v", state)
	}
	plain := f.service.Recover(f.ctx, false)
	if plain.Recovered || plain.Safety != "blocked_recover_diverged" {
		t.Fatalf("plain recovery accepted the conflict: %#v", plain)
	}
	for _, dir := range []string{f.gate, f.remote} {
		if err := os.WriteFile(filepath.Join(dir, "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	recovered := f.service.RecoverTakePipeline(f.ctx)
	if !recovered.Recovered || !recovered.Changed || recovered.Safety != SafetyPipelineRewritePending || recovered.Error != "" {
		t.Fatalf("explicit recovery = %#v", recovered)
	}
	checkReplayAdoption(t, f, pushed, pushed)
	if fresh := f.service.Refresh(f.ctx); fresh.Remote.Freshness != "live" || fresh.Safety != SafetyPipelineRewritePending || fresh.Error != "" {
		t.Fatalf("live status after recovery = %#v", fresh)
	}
}

func TestRecoverTakePipelineRefusesDroppedWorkOutsideConflict(t *testing.T) {
	t.Parallel()
	f, pushed := newConflictReplay(t, true)
	if state := f.service.InspectCached(f.ctx); state.TakePipeline != nil {
		t.Fatalf("offered explicit adoption despite a missing cleanly merged file: %#v", state)
	}
	if result := f.service.RecoverTakePipeline(f.ctx); result.Recovered || result.Changed || f.custodyReturned() {
		t.Fatalf("explicit adoption accepted a missing cleanly merged file: %#v", result)
	}
	if mustRun(t, f.local, "rev-parse", "HEAD") != f.submitted || mustRun(t, f.gate, "rev-parse", replayBranchRef) != pushed {
		t.Fatal("refused recovery moved a branch")
	}
}

func assertReplayRecovery(t *testing.T, f *recoverFixture, oldGate, published string) {
	t.Helper()
	state := f.service.InspectCached(f.ctx)
	if state.NextAction == nil || state.NextAction.Command != "no-mistakes axi sync --recover" {
		t.Fatalf("expected automatic replay recovery, got %#v", state)
	}
	recovered := f.service.Recover(f.ctx, false)
	if !recovered.Recovered || !recovered.Changed || recovered.Error != "" {
		t.Fatalf("recovery = %#v", recovered)
	}
	checkReplayAdoption(t, f, oldGate, published)
}

func checkReplayAdoption(t *testing.T, f *recoverFixture, oldGate, published string) {
	t.Helper()
	for _, ref := range []struct{ dir, name, want string }{
		{f.local, "HEAD", f.preserved},
		{f.local, f.localAnchorRef(), f.submitted},
		{f.gate, replayBranchRef, f.preserved},
		{f.gate, custody.RecoveryGateRef(f.run.ID), oldGate},
		{f.remote, replayBranchRef, published},
	} {
		if got := mustRun(t, ref.dir, "rev-parse", ref.name); got != ref.want {
			t.Errorf("%s in %s = %s, want %s", ref.name, filepath.Base(ref.dir), got, ref.want)
		}
	}
	followup := commitReplayFile(t, f.local, "followup.txt", "the next change\n")
	state := f.service.InspectCached(f.ctx)
	if state.Error != "" || state.NextAction == nil || state.NextAction.Code != "run_pipeline" {
		t.Fatalf("status with local follow-up = %#v", state)
	}
	plan, err := gate.PlanStaleBranchReconciliation(f.ctx, f.gate, f.local, "feature/recover", followup, "", nil)
	if err != nil || plan.Reconcile {
		t.Fatalf("fresh submission must be a mirror fast-forward, got %#v, %v", plan, err)
	}
}

func TestRecoverReplayRefusesOperatorCommitAfterSubmission(t *testing.T) {
	t.Parallel()
	f := newReviewFixReplay(t)
	later := commitReplayFile(t, f.local, "local-only.txt", "keep this local follow-up\n")
	for _, result := range []State{f.service.InspectCached(f.ctx), f.service.Recover(f.ctx, false), f.service.RecoverTakePipeline(f.ctx)} {
		if result.Recovered || result.Changed || result.TakePipeline != nil ||
			(result.NextAction != nil && result.NextAction.Command == "no-mistakes axi sync --recover") {
			t.Fatalf("recovery would drop an unsubmitted commit: %#v", result)
		}
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != later {
		t.Fatalf("lost local follow-up %s; HEAD is %s", later, got)
	}
}

func TestRecoverTakePipelineRechecksDirtyWorktreeAndPinnedHead(t *testing.T) {
	for _, condition := range []string{"dirty", "missing pin", "moved pin", "new local commit"} {
		t.Run(condition, func(t *testing.T) {
			t.Parallel()
			f, pushed := newConflictReplay(t, false)
			before := f.submitted
			switch condition {
			case "dirty":
				mustWrite(t, filepath.Join(f.local, "feature.txt"), "uncommitted work\n")
			case "missing pin":
				mustRun(t, f.gate, "update-ref", "-d", f.anchorRef())
			case "moved pin":
				mustRun(t, f.gate, "update-ref", f.anchorRef(), pushed)
			case "new local commit":
				before = commitReplayFile(t, f.local, "local-only.txt", "after submission\n")
			}
			if state := f.service.InspectCached(f.ctx); state.TakePipeline != nil {
				t.Fatalf("offered explicit adoption with %s: %#v", condition, state)
			}
			result := f.service.RecoverTakePipeline(f.ctx)
			if result.Recovered || result.Changed || f.custodyReturned() ||
				mustRun(t, f.local, "rev-parse", "HEAD") != before || mustRun(t, f.gate, "rev-parse", replayBranchRef) != pushed {
				t.Fatalf("explicit adoption changed custody or branches with %s: %#v", condition, result)
			}
		})
	}
}

func TestRecoverTakePipelinePreservesEditArrivingAfterBranchSwap(t *testing.T) {
	t.Parallel()
	f, pushed := newConflictReplay(t, false)
	f.service.afterRecoverBranchMove = func() {
		mustWrite(t, filepath.Join(f.local, "conflict.txt"), "local conflict resolution\n")
	}
	state := f.service.RecoverTakePipeline(f.ctx)
	if state.Recovered || state.Safety != "blocked_recover_worktree_busy" || f.custodyReturned() {
		t.Fatalf("recovery accepted a concurrent edit: %#v", state)
	}
	if got := readOptional(t, filepath.Join(f.local, "conflict.txt")); got != "local conflict resolution\n" {
		t.Fatalf("concurrent edit overwritten: %q", got)
	}
	if mustRun(t, f.local, "rev-parse", "HEAD") != f.submitted || mustRun(t, f.gate, "rev-parse", replayBranchRef) != pushed {
		t.Fatal("recovery failed to leave both branches at their original heads")
	}
}

func TestRecoverReplayMirrorRequiresPinAndCompareAndSwap(t *testing.T) {
	for _, race := range []string{"mirror branch moved", "mirror pin removed"} {
		t.Run(race, func(t *testing.T) {
			t.Parallel()
			f := newReviewFixReplay(t)
			wantGate := f.submitted
			f.service.beforeGateReset = func() {
				ref := custody.RecoveryGateRef(f.run.ID)
				if pinned := mustRun(t, f.gate, "rev-parse", ref); pinned != f.submitted {
					t.Fatalf("original mirror head was not pinned before the update: %s", pinned)
				}
				if race == "mirror branch moved" {
					wantGate = f.base
					mustRun(t, f.gate, "update-ref", replayBranchRef, wantGate, f.submitted)
				} else {
					mustRun(t, f.gate, "update-ref", "-d", ref)
				}
			}
			state := f.service.Recover(f.ctx, false)
			if state.Recovered || !state.Changed || state.Safety != "blocked_recover_gate_race" || f.custodyReturned() {
				t.Fatalf("recovery after %s = %#v", race, state)
			}
			if got := mustRun(t, f.gate, "rev-parse", replayBranchRef); got != wantGate {
				t.Fatalf("mirror overwritten after %s: %s, want %s", race, got, wantGate)
			}
			if race == "mirror pin removed" {
				f.service.beforeGateReset = nil
				if retry := f.service.Recover(f.ctx, false); !retry.Recovered || retry.Changed {
					t.Fatalf("could not resume a partially completed adoption: %#v", retry)
				}
				checkReplayAdoption(t, f, f.submitted, f.submitted)
			}
		})
	}
}

func TestSyncPublishedReviewFixUsingReplayPoint(t *testing.T) {
	t.Parallel()
	f := newReviewFixReplay(t)
	mustRun(t, f.gate, "push", f.remote, "+"+f.preserved+":"+replayBranchRef)
	bindReplayPush(t, f, f.preserved)
	if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunCompleted, f.preserved); err != nil {
		t.Fatal(err)
	}
	if plan := f.service.Refresh(f.ctx); plan.Safety != SafetySafeEquivalentAdvance {
		t.Fatalf("published fix did not offer equivalent advance: %#v", plan)
	}
	applied := f.service.Apply(f.ctx)
	if !applied.Changed || applied.State != StateSynchronized || applied.Error != "" {
		t.Fatalf("could not sync published review fix: %#v", applied)
	}
	if got := mustRun(t, f.local, "rev-parse", syncAnchorRef(f.run.ID)); got != f.submitted {
		t.Fatalf("sync anchor = %s, want %s", got, f.submitted)
	}
}

func TestRecoverReplayRefusesIndirectMirrorAnchorBeforeMovingLocalBranch(t *testing.T) {
	t.Parallel()
	f := newReviewFixReplay(t)
	mustRun(t, f.gate, "tag", "-a", "-m", "old mirror head", "mirror-tip", f.submitted)
	tag := mustRun(t, f.gate, "rev-parse", "refs/tags/mirror-tip")
	mustRun(t, f.gate, "update-ref", custody.RecoveryGateRef(f.run.ID), tag)

	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed || state.Safety != "blocked_recover_preserve_failed" || f.custodyReturned() {
		t.Fatalf("recovery accepted an indirect mirror pin: %#v", state)
	}
	if mustRun(t, f.local, "rev-parse", "HEAD") != f.submitted || mustRun(t, f.gate, "rev-parse", replayBranchRef) != f.submitted {
		t.Fatal("a branch moved without a direct mirror pin")
	}
}
