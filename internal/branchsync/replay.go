package branchsync

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

const takePipelineCommand = "no-mistakes axi sync --recover --take-pipeline"

// SafetyPipelineRewritePending marks a custody-returned branch whose adopted
// head is ready for validation over the run's recorded last push.
const SafetyPipelineRewritePending = "pipeline_rewrite_pending"

// A Review start from the selected run can prove that the branch was replayed
// intact before later pipeline fixes changed its lines. The proof covers the
// current local head and requires the recorded start to survive in the target.
func (s *Service) replayPointProves(ctx context.Context, dir string, env []string, run *db.Run, local, target string) bool {
	starts, err := s.DB.ReviewStarts(run.ID)
	return err == nil && replayContains(ctx, dir, env, starts, local, target)
}

func replayContains(ctx context.Context, dir string, env []string, starts []db.ReviewStart, local, target string) bool {
	for _, start := range starts {
		if !reviewStartReaches(ctx, dir, env, start.Head, target) {
			continue
		}
		base, err := git.RunWithEnv(ctx, dir, env, "merge-base", local, start.Head)
		if err == nil && base != "" && mergeTreeMatchesWithEnv(ctx, dir, env, base, local, start.Head, start.Head) {
			return true
		}
	}
	return false
}

func reviewStartReaches(ctx context.Context, dir string, env []string, start, target string) bool {
	exact, err := git.RunWithEnv(ctx, dir, env, "rev-parse", "--verify", "--end-of-options", start+"^{commit}")
	if err != nil || exact != start {
		return false
	}
	_, err = git.RunWithEnv(ctx, dir, env, "merge-base", "--is-ancestor", start, target)
	return err == nil
}

// The explicit choice covers a conflict at the start of a validation pass.
// Compare the published result (or the submission before any push) to the
// surviving Review start. Require every path Git merged cleanly to survive.
// This also keeps an ordinary dropped file from being excused by a conflict
// elsewhere. The contents of the conflicting paths are the operator's choice.
func (s *Service) takePipelineConflicts(ctx context.Context, state *State, run *db.Run) []string {
	local, preserved := state.Local.Head, run.HeadSHA
	if !state.Local.Clean || run.TerminalHeadVerifiedAt == nil || local == "" ||
		(local != ptr(run.SubmittedHeadSHA) && local != ptr(run.LastPushedSHA)) ||
		duplicateBranchCheckout(ctx, s.workDir(), state.Local.Branch) {
		return nil
	}
	gateDir := strings.TrimSpace(s.GateDir)
	pin := custody.RecoveryRef(run.ID)
	if gateDir == "" || !exactRecoveryRef(ctx, gateDir, pin, preserved) {
		return nil
	}
	if compatible, err := exactCommitRefCompatible(ctx, s.workDir(), custody.RecoveryLocalRef(run.ID), local); err != nil || !compatible {
		return nil
	}
	starts, err := s.DB.ReviewStarts(run.ID)
	if err != nil {
		return nil
	}
	env, cleanup := s.recoveryObjectOverlay(ctx, gateDir)
	if env == nil {
		return nil
	}
	defer cleanup()
	dir := s.workDir()
	source := ptr(run.LastPushedSHA)
	if source == "" {
		source = local
	}
	if source != local {
		base, err := git.RunWithEnv(ctx, dir, env, "merge-base", local, source)
		if err != nil || base == "" ||
			(!mergeTreeMatchesWithEnv(ctx, dir, env, base, local, source, source) &&
				!replayContains(ctx, dir, env, starts, local, source)) {
			return nil
		}
	}
	for _, start := range starts {
		if start.Initial && start.Head != source && reviewStartReaches(ctx, dir, env, start.Head, preserved) {
			if paths := conflictingReplayPaths(ctx, dir, env, source, start.Head); len(paths) > 0 {
				return paths
			}
		}
	}
	return nil
}

func conflictingReplayPaths(ctx context.Context, dir string, env []string, source, replay string) []string {
	// The stdin form reports each merge's status in stdout. NUL separation
	// preserves spaces, newlines and other literal filename characters.
	out, err := git.RunWithEnvAndInput(ctx, dir, env, replay+" "+source+"\n", "merge-tree", "--stdin", "-z", "--name-only")
	parts := strings.Split(out, "\x00")
	if err != nil || len(parts) < 4 || parts[0] != "0" {
		return nil
	}
	end := slices.Index(parts[2:], "")
	if end <= 0 {
		return nil
	}
	conflicts := parts[2 : 2+end]
	changed, err := git.RunWithEnv(ctx, dir, env, "diff-tree", "--no-commit-id", "--no-renames", "--raw", "--ignore-submodules=none", "-r", "-z", replay+"^{tree}", parts[1])
	if err != nil {
		return nil
	}
	// Raw diff records alternate metadata and literal paths when rename
	// detection is disabled. The leading metadata preserves path whitespace.
	if changed != "" {
		records := strings.Split(strings.TrimSuffix(changed, "\x00"), "\x00")
		if len(records)%2 != 0 {
			return nil
		}
		for i := 1; i < len(records); i += 2 {
			if !slices.Contains(conflicts, records[i]) {
				return nil
			}
		}
	}
	return slices.Compact(conflicts)
}

func exactRecoveryRef(ctx context.Context, dir, ref, expected string) bool {
	compatible, err := exactCommitRefCompatible(ctx, dir, ref, expected)
	if err != nil || !compatible {
		return false
	}
	target, exists, err := git.ExactRefTarget(ctx, dir, ref)
	return err == nil && exists && target == expected
}

// Keep the old lane reachable and snapshot it before changing the worktree.
// A retry can only reuse the original anchor or a mirror already at its target.
func (s *Service) prepareAdoptedMirror(ctx context.Context, run *db.Run, state State) (string, error) {
	ref := "refs/heads/" + state.Local.Branch
	if bare, err := git.RunBare(ctx, s.GateDir, "rev-parse", "--is-bare-repository"); err != nil || bare != "true" {
		return "", fmt.Errorf("the private mirror is not available as a bare repository")
	}
	if target, err := git.RunBare(ctx, s.GateDir, "symbolic-ref", "-q", ref); err == nil {
		return "", fmt.Errorf("mirror branch is symbolic to %s", target)
	}
	old, exists, err := git.ExactRefTarget(ctx, s.GateDir, ref)
	if err != nil {
		return "", err
	}
	if !exists {
		return strings.Repeat("0", len(run.HeadSHA)), nil
	}
	if old != run.HeadSHA {
		anchor := custody.RecoveryGateRef(run.ID)
		if err := custody.PreserveRecoveryAnchor(ctx, s.GateDir, anchor, old); err != nil {
			return "", err
		}
		if !exactRecoveryRef(ctx, s.GateDir, anchor, old) {
			return "", fmt.Errorf("the old mirror head is not directly pinned")
		}
	}
	return old, nil
}

func (s *Service) finishAdoptedMirror(ctx context.Context, run *db.Run, state State, old string) State {
	if s.beforeGateReset != nil {
		s.beforeGateReset()
	}
	changed := state.Changed
	ref := "refs/heads/" + state.Local.Branch
	preserved := run.HeadSHA
	worktreeBranch, branchErr := git.CurrentBranch(ctx, s.workDir())
	worktreeHead, headErr := git.HeadSHA(ctx, s.workDir())
	clean, _ := worktreeClean(ctx, s.workDir())
	freshRun, _, _ := s.inspect(ctx)
	if branchErr != nil || headErr != nil || worktreeBranch != state.Local.Branch || worktreeHead != preserved || !clean ||
		freshRun.Pipeline.RunID != run.ID || freshRun.Pipeline.CurrentHead != preserved || freshRun.State != StatePipelineOwned {
		state = blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the branch, worktree, or run changed before the mirror move; custody was not recorded")
		state.Changed = changed
		return state
	}
	// Verify the pins and move the branch in one ref transaction. Never
	// overwrite a ref that moved since the snapshot taken before adoption.
	transaction := fmt.Sprintf("verify %s %s\n", custody.RecoveryRef(run.ID), preserved)
	if old == preserved {
		transaction += fmt.Sprintf("verify %s %s\n", ref, preserved)
	} else {
		if !git.IsZeroSHA(old) {
			transaction += fmt.Sprintf("verify %s %s\n", custody.RecoveryGateRef(run.ID), old)
		}
		transaction += fmt.Sprintf("update %s %s %s\n", ref, preserved, old)
	}
	if _, err := git.RunWithInput(ctx, s.GateDir, transaction, "update-ref", "--no-deref", "--stdin"); err != nil {
		state = blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the mirror branch or a recovery anchor changed; custody was not recorded")
		state.Changed = changed
		state.NextAction = &NextAction{Code: "inspect_and_reconcile_manually", Command: "no-mistakes axi status"}
		return state
	}
	return s.finishRecover(ctx, run, changed)
}

func (s *Service) classifyPipelineRewritePending(ctx context.Context, state *State, run *db.Run) bool {
	if run.CustodyReturnedAt == nil || run.TerminalHeadVerifiedAt == nil || !terminalRunStatus(run.Status) ||
		run.HeadSHA == ptr(run.LastPushedSHA) || !isAncestor(ctx, s.workDir(), run.HeadSHA, state.Local.Head) ||
		isAncestor(ctx, s.workDir(), ptr(run.LastPushedSHA), state.Local.Head) {
		return false
	}
	state.State = StateCustodyReturned
	state.Relation = relationBetween(ctx, s.workDir(), state.Local.Head, ptr(run.LastPushedSHA))
	state.Safety = SafetyPipelineRewritePending
	state.Error = ""
	state.NextAction = &NextAction{Code: "run_pipeline", Command: `no-mistakes axi run --intent "<what the user set out to accomplish>"`}
	return true
}

func (s *Service) equivalentPublishedHead(ctx context.Context, run *db.Run, local, pushed string) bool {
	return equivalentDivergence(ctx, s.workDir(), local, pushed, run.BaseSHA) ||
		s.replayPointProves(ctx, s.workDir(), nil, run, local, pushed)
}

func (s *Service) recoverWithConflict(ctx context.Context, run *db.Run, state State) State {
	// The offer requires a preexisting mirror pin. Fetch a verified copy of
	// that exact object, then recheck the conflict proof at the move boundary.
	ref := custody.RecoveryRef(run.ID)
	if err := git.FetchRemoteRef(ctx, s.workDir(), s.GateDir, ref, run.HeadSHA); err != nil ||
		custody.PreserveRecoveryHead(ctx, s.workDir(), run.ID, run.HeadSHA) != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the pinned pipeline head could not be imported; no branch or worktree was moved")
	}
	return s.recoverAdoptPreserved(ctx, run, state, false, true)
}

func (s *Service) hasAdoptionAnchor(ctx context.Context, run *db.Run, state State) bool {
	ref := custody.RecoveryLocalRef(run.ID)
	head, exists, err := git.ExactRefTarget(ctx, s.workDir(), ref)
	return err == nil && exists && head != state.Local.Head && exactRecoveryRef(ctx, s.workDir(), ref, head)
}

type divergedMoveError struct {
	message  string
	worktree bool
}

func (err *divergedMoveError) Error() string {
	return err.message
}

// Swap the branch by old OID and use Git's two-tree update to preserve
// worktree edits that arrive after checking for a clean checkout.
func moveDivergedWorktree(ctx context.Context, dir, branch, old, target string, afterSwap func()) *divergedMoveError {
	ref := "refs/heads/" + branch
	current, err := git.CurrentBranch(ctx, dir)
	if err != nil || current != branch {
		return &divergedMoveError{message: "the checked-out branch changed before the move"}
	}
	if _, err := git.Run(ctx, dir, "update-ref", "--no-deref", ref, target, old); err != nil {
		return &divergedMoveError{message: "the local branch moved before it could be swapped"}
	}
	if afterSwap != nil {
		afterSwap()
	}
	// Git cannot hold the checked-out branch and index locks across these
	// calls. A checkout between this read and read-tree can still race.
	// Callers verify the final HEAD before reporting success.
	current, err = git.CurrentBranch(ctx, dir)
	if err == nil && current == branch {
		if _, err := git.Run(ctx, dir, "read-tree", "-m", "-u", old, target); err == nil {
			return nil
		}
	}
	failure := &divergedMoveError{message: "the checked-out branch changed during the move"}
	if err == nil && current == branch {
		failure.message = "the working tree changed during the move; no file was overwritten"
		failure.worktree = true
	}
	if _, rollbackErr := git.Run(ctx, dir, "update-ref", "--no-deref", ref, old, target); rollbackErr != nil {
		failure.message += fmt.Sprintf("; the branch could not be restored to %s", old)
	}
	return failure
}
