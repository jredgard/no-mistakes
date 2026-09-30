package citest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIStep_GitHubConflictStillWaitsForQueuedCheck(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	sctx := stepstest.NewTestContext(t, &stepstest.MockAgent{AgentName: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = stepstest.FakeCIGHMergeable(t, "OPEN", `[{"name":"build","state":"QUEUED","bucket":"pending"}]`, "CONFLICTING")
	prURL := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 30 * time.Minute
	var logs []string
	sctx.Log = func(message string) { logs = append(logs, message) }
	polls := 0
	step := (&steps.CIStep{}).
		SetBaseBranchTip(func(context.Context) (string, bool) { return baseSHA, true }).
		SetWaitForNextPoll(func(context.Context, time.Duration) error {
			polls++
			return context.Canceled
		})
	outcome, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) || outcome != nil || polls != 1 ||
		!strings.Contains(strings.Join(logs, "\n"), "issues detected but checks still pending") {
		t.Fatalf("outcome = %+v, err = %v, polls = %d, want unchanged GitHub pending wait; logs: %v", outcome, err, polls, logs)
	}
}

func TestCIStep_AzureDevOpsConflictWithQueuedPolicyBuild(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name        string
		mergeStatus string
		policies    string
		wantWait    bool
	}{
		{name: "conflict with no checks", mergeStatus: "conflicts", policies: `[]`},
		{name: "queued build zero", mergeStatus: "conflicts", policies: `[{"status":"queued","configuration":{"type":{"displayName":"Build"}},"context":{"buildId":0}}]`},
		{name: "queued build absent", mergeStatus: "conflicts", policies: `[{"status":"queued","configuration":{"type":{"displayName":"Build"}}}]`},
		{name: "queued build with execution", mergeStatus: "conflicts", policies: `[{"status":"queued","configuration":{"type":{"displayName":"Build"}},"context":{"buildId":123}}]`, wantWait: true},
		{name: "running build", mergeStatus: "conflicts", policies: `[{"status":"running","configuration":{"type":{"displayName":"Build"}},"context":{"buildId":123}}]`, wantWait: true},
		{name: "running build without id", mergeStatus: "conflicts", policies: `[{"status":"running","configuration":{"type":{"displayName":"Build"}}}]`, wantWait: true},
		{name: "queued build alongside running check", mergeStatus: "conflicts", policies: `[{"status":"queued","configuration":{"type":{"displayName":"Build"}},"context":{"buildId":0}},{"status":"running","configuration":{"type":{"displayName":"Status"}}}]`, wantWait: true},
		{name: "queued status check", mergeStatus: "conflicts", policies: `[{"status":"queued","configuration":{"type":{"displayName":"Status"}},"context":{"buildId":0}}]`, wantWait: true},
		{name: "no conflict", mergeStatus: "succeeded", policies: `[{"status":"queued","configuration":{"type":{"displayName":"Build"}},"context":{"buildId":0}}]`, wantWait: true},
		{name: "unknown mergeability", mergeStatus: "queued", policies: `[{"status":"queued","configuration":{"type":{"displayName":"Build"}},"context":{"buildId":0}}]`, wantWait: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
			ag := &stepstest.MockAgent{AgentName: "test"}
			sctx := stepstest.NewTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
			binDir := stepstest.FakeCLIBinDir(t)
			stepstest.LinkFakeCLI(t, binDir, "az")
			sctx.Env = stepstest.FakeCLIEnv(binDir, map[string]string{
				"FAKE_CLI_MODE":      "ci-az",
				"FAKE_CLI_CHECKS":    testCase.policies,
				"FAKE_CLI_MERGEABLE": testCase.mergeStatus,
			})
			prURL := "https://dev.azure.com/myorg/myproject/_git/myrepo/pullrequest/42"
			sctx.Run.PRURL = &prURL
			sctx.Repo.UpstreamURL = "https://dev.azure.com/myorg/myproject/_git/myrepo"
			sctx.Config.CITimeout = 30 * time.Minute
			var logs []string
			sctx.Log = func(message string) { logs = append(logs, message) }
			polls := 0
			step := (&steps.CIStep{}).
				SetBaseBranchTip(func(context.Context) (string, bool) { return baseSHA, true }).
				SetWaitForNextPoll(func(context.Context, time.Duration) error {
					polls++
					return context.Canceled
				})
			outcome, err := step.Execute(sctx)
			if testCase.wantWait {
				if !errors.Is(err, context.Canceled) || outcome != nil || polls != 1 {
					t.Fatalf("outcome = %+v, err = %v, polls = %d, want pending wait; logs: %v", outcome, err, polls, logs)
				}
				if testCase.mergeStatus == "conflicts" && !strings.Contains(strings.Join(logs, "\n"), "issues detected but checks still pending") {
					t.Fatalf("running check did not defer conflict repair; logs: %v", logs)
				}
				return
			}
			if err != nil || outcome == nil || !outcome.AutoFixable || !outcome.NeedsApproval || polls != 0 {
				t.Fatalf("outcome = %+v, err = %v, polls = %d, want immediate conflict repair findings; logs: %v", outcome, err, polls, logs)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings.Items) != 1 || findings.Items[0].Category != types.FindingCategoryCIMergeConflict || findings.Items[0].Action != types.ActionAutoFix {
				t.Fatalf("findings = %+v, want the existing auto-fix conflict repair path", findings)
			}
			if strings.Contains(strings.Join(logs, "\n"), "checks passed") {
				t.Fatalf("queued build must not certify readiness; logs: %v", logs)
			}
		})
	}
}
