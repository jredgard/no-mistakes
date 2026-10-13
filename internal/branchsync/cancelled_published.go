package branchsync

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A cancelled CI monitor cannot publish again. Once the operator has every
// pipeline change, including a clean local rebase, returning custody must not
// demand that they first undo that rebase or replace the PR. The reverse of
// the ordinary recovery containment proof establishes that the local tree
// carries the pipeline work; patch identity alone is never sufficient.
func (s *Service) classifyCancelledPublished(ctx context.Context, state *State, run *db.Run) {
	if run == nil || run.Status != types.RunCancelled || run.TerminalHeadVerifiedAt == nil ||
		!exactPushedBinding(s.Repo, run, state.Local.Branch) || !state.Local.Clean ||
		state.PRState == "merged" || state.PRState == "closed" ||
		!(state.Local.Head == run.HeadSHA || isAncestor(ctx, s.workDir(), run.HeadSHA, state.Local.Head) ||
			preservedContainsLocalWork(ctx, s.workDir(), run.HeadSHA, state.Local.Head)) {
		return
	}
	if run.CustodyReturnedAt != nil {
		s.classifyUserOwned(ctx, state)
		return
	}
	for _, dir := range []string{s.workDir(), s.GateDir} {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		if compatible, err := recoveryAnchorCompatible(ctx, dir, run.ID, run.HeadSHA); err != nil || !compatible {
			return
		}
	}
	state.Safety = "blocked_cancelled_published_recoverable"
	state.Error = "the cancelled run's pipeline changes are contained in the local branch; recover custody without moving the worktree before continuing on this branch"
	state.NextAction = &NextAction{Code: "recover_custody", Command: "no-mistakes axi sync --recover"}
}

func cancelledPublishedRecoveryOffered(state State) bool {
	return state.Safety == "blocked_cancelled_published_recoverable"
}

func (s *Service) recoverCancelledPublished(ctx context.Context, run *db.Run) State {
	// Preserve the original push binding and verify its live target. Recovery
	// does not authorize overwriting an out-of-band push or a retired PR.
	state := s.Refresh(ctx)
	if run == nil || state.Pipeline.RunID != run.ID || !cancelledPublishedRecoveryOffered(state) ||
		state.Remote.Freshness != "live" || state.Remote.ObservedHead != run.HeadSHA {
		return blockedPlan(state, state.State, "blocked_recover_assumptions_changed", "the cancelled run or its live push binding changed; custody was not returned")
	}
	for _, dir := range []string{s.workDir(), s.GateDir} {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		if err := custody.PreserveRecoveryHead(ctx, dir, run.ID, run.HeadSHA); err != nil {
			return blockedPlan(state, state.State, "blocked_recover_preserve_failed", fmt.Sprintf("the cancelled pipeline head could not be preserved in %s: %v; custody was not returned", dir, err))
		}
	}
	if s.beforeRecoverRebind != nil {
		s.beforeRecoverRebind()
	}
	// Re-read both local containment and the live binding after preservation.
	// No branch, worktree, gate lane, or remote is moved by this operation.
	recheck := s.Refresh(ctx)
	if !cancelledPublishedRecoveryOffered(recheck) || recheck.Pipeline != state.Pipeline ||
		recheck.Local != state.Local || recheck.Remote.ObservedHead != state.Remote.ObservedHead ||
		recheck.Remote.Freshness != "live" {
		return blockedPlan(recheck, recheck.State, "blocked_recover_assumptions_changed", "the branch or push binding changed while custody was being returned; the pipeline head stays preserved and custody was not returned")
	}
	returned, err := s.DB.ReturnCancelledPublishedCustody(run, s.Repo)
	if err != nil || !returned {
		return blockedPlan(recheck, recheck.State, "blocked_recover_assumptions_changed", "the run or repository binding changed before custody could be recorded; the pipeline head stays preserved")
	}
	after := s.InspectCached(ctx)
	after.Recovered = true
	return after
}
