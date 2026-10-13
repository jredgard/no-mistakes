package citest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIStep_NoChangeFixRepollsGreenPublishedHead(t *testing.T) {
	for _, tc := range []struct {
		name       string
		conclusion string
		pending    bool
	}{
		{"green after external fix", `{"summary":"external fix already landed","code_change_needed":false}`, false},
		{"pending after external fix", `{"summary":"external fix already landed","code_change_needed":false}`, true},
		{"repair needed", `{"summary":"code repair needed","code_change_needed":true}`, false},
		{"unstructured conclusion", `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
			ag := &stepstest.MockAgent{AgentName: "test", RunFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(tc.conclusion)}, nil
			}}
			sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			checks := []string{
				`[{"name":"build","bucket":"fail","state":"FAILURE","completedAt":"2026-10-13T12:00:00Z"}]`,
			}
			wantPolls := 1
			if tc.pending {
				checks = append(checks, `[{"name":"build","bucket":"pending","state":"PENDING"}]`)
				wantPolls++
			}
			checks = append(checks, `[{"name":"build","bucket":"pass","state":"SUCCESS"}]`)
			sctx.Env = stepstest.FakeCIGHSequence(t, "OPEN", checks)
			for i, value := range sctx.Env {
				if strings.HasPrefix(value, "FAKE_CLI_PR_HEAD_SHA=") {
					sctx.Env[i] = "FAKE_CLI_PR_HEAD_SHA=" + headSHA
				}
			}
			prURL := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &prURL
			sctx.Config.CITimeout = time.Minute
			marked := 0
			sctx.MarkRunning = func() error { marked++; return nil }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sctx.Ctx = ctx
			polls := 0
			step := (&steps.CIStep{}).SetBaseBranchTip(func(context.Context) (string, bool) { return baseSHA, true }).SetWaitForNextPoll(func(ctx context.Context, _ time.Duration) error {
				polls++
				run, err := sctx.DB.GetRun(sctx.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if polls < wantPolls {
					if run.CIReadyAt != nil {
						t.Fatal("pending checks became ready")
					}
					return nil
				}
				if run.CIReadyAt == nil || marked != 1 {
					t.Fatalf("green re-poll did not resume readiness: ready=%v MarkRunning=%d", run.CIReadyAt, marked)
				}
				cancel()
				return ctx.Err()
			})
			gate, err := step.Execute(sctx)
			if err != nil || gate == nil || !gate.NeedsApproval {
				t.Fatalf("initial failure = %#v, err = %v", gate, err)
			}
			sctx.Fixing = true
			sctx.PreviousFindings = gate.Findings
			outcome, err := step.Execute(sctx)
			if outcome != nil || !errors.Is(err, context.Canceled) || polls != wantPolls {
				t.Fatalf("no-change fix = %#v, err = %v, polls = %d; want monitoring green", outcome, err, polls)
			}
			if len(ag.Calls) != 1 || stepstest.GitCmd(t, dir, "rev-parse", "HEAD") != headSHA || stepstest.GitCmd(t, dir, "status", "--porcelain") != "" {
				t.Fatal("no-change fix repeated the agent or changed the published head/worktree")
			}
		})
	}
}

func TestCIStep_NoChangeConclusionParksFreshFailures(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	ag := &stepstest.MockAgent{AgentName: "test", RunFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"summary":"external failure needs an operator","code_change_needed":false}`)}, nil
	}}
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = stepstest.FakeCIGHSequence(t, "OPEN", []string{
		`[{"name":"old build","bucket":"fail","state":"FAILURE","completedAt":"2026-10-13T12:00:00Z"}]`,
		`[{"name":"current build","bucket":"fail","state":"FAILURE"}]`,
	})
	prURL := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &prURL
	step := (&steps.CIStep{}).SetBaseBranchTip(func(context.Context) (string, bool) { return baseSHA, true })
	gate, err := step.Execute(sctx)
	if err != nil || gate == nil || !gate.NeedsApproval {
		t.Fatalf("initial failure = %#v, err = %v", gate, err)
	}
	sctx.Fixing = true
	sctx.PreviousFindings = gate.Findings
	gate, err = step.Execute(sctx)
	if err != nil || gate == nil || !gate.NeedsApproval || gate.AutoFixable {
		t.Fatalf("fresh failure = %#v, err = %v; want ask-user", gate, err)
	}
	findings, err := types.ParseFindingsJSON(gate.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Check != "current build" || findings.Items[0].Action != types.ActionAskUser || !strings.Contains(findings.Items[0].Description, "current build") || strings.Contains(gate.Findings, "old build") {
		t.Fatalf("no-change fix retained stale failures: %s", gate.Findings)
	}
	if findings.Summary != "external failure needs an operator" || len(ag.Calls) != 1 {
		t.Fatalf("conclusion lost or fixer repeated: findings=%s calls=%d", gate.Findings, len(ag.Calls))
	}
}
