package db

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ReturnCancelledPublishedCustody binds the custody stamp to the exact
// cancelled run and publication evidence verified by branch synchronization.
// A newer active run or changed binding wins rather than receiving stale
// recovery authority. Publication provenance itself is never changed.
func (d *DB) ReturnCancelledPublishedCustody(run *Run, repo *Repo) (bool, error) {
	if run == nil || repo == nil || run.LastPushedSHA == nil || run.PushGeneration == nil ||
		run.PushTargetKind == nil || run.PushTargetFingerprint == nil || run.PushRef == nil ||
		run.TerminalHeadVerifiedAt == nil {
		return false, nil
	}
	result, err := d.sql.Exec(`UPDATE runs SET custody_returned_at = ?, updated_at = ?
		WHERE id = ? AND repo_id = ? AND branch = ? AND status = ? AND custody_returned_at IS NULL
		AND head_sha = ? AND last_pushed_sha = ? AND push_generation = ? AND terminal_head_verified_at = ?
		AND push_target_kind = ? AND push_target_fingerprint = ? AND push_ref = ? AND COALESCE(push_active, 0) = 0
		AND pr_state IS ? AND COALESCE(pr_state, '') NOT IN ('merged', 'closed')
		AND NOT EXISTS (SELECT 1 FROM runs other WHERE other.repo_id = runs.repo_id AND other.branch = runs.branch AND other.id <> runs.id
			AND other.status NOT IN (?, ?, ?, ?))
		AND EXISTS (SELECT 1 FROM repos WHERE repos.id = runs.repo_id AND repos.upstream_url = ? AND COALESCE(repos.fork_url, '') = ?)`,
		now(), now(), run.ID, repo.ID, run.Branch, string(types.RunCancelled),
		run.HeadSHA, *run.LastPushedSHA, *run.PushGeneration, *run.TerminalHeadVerifiedAt,
		*run.PushTargetKind, *run.PushTargetFingerprint, *run.PushRef, run.PRState,
		string(types.RunCompleted), string(types.RunFailed), string(types.RunCancelled), string(types.RunCIMonitorInterrupted),
		repo.UpstreamURL, repo.ForkURL)
	if err != nil {
		return false, fmt.Errorf("return cancelled published custody: %w", err)
	}
	count, err := result.RowsAffected()
	return count == 1, err
}
