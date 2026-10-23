package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRRiskAssessmentPreservesInitialRound(t *testing.T) {
	t.Parallel()
	initial := `{"findings":[{"id":"review-1","severity":"error","file":"release.go","description":"Deploy rolls back on a gate violation."}],"risk_level":"high","risk_rationale":"The gate runs inside the release verifier, so violations roll back the deploy."}`
	original := "🚨 High: The gate runs inside the release verifier, so violations roll back the deploy."
	for _, test := range []struct {
		name          string
		level         string
		rationale     string
		status        types.StepStatus
		trigger       string
		remaining     bool
		single        bool
		initialClean  bool
		unreadable    bool
		noRoundResult bool
		transition    string
	}{
		{name: "high to low", level: "low", rationale: "Fix rounds moved the gate out of the release verifier.\n  Deploy rollback is unchanged.", status: types.StepStatusCompleted, trigger: "auto_fix", transition: "🚨 High -> ✅ Low: Fix rounds moved the gate out of the release verifier. Deploy rollback is unchanged."},
		{name: "high to medium", level: "medium", rationale: "Rollback was removed; release integration still changes.", status: types.StepStatusCompleted, trigger: "auto_fix", transition: "🚨 High -> ⚠️ Medium: Rollback was removed; release integration still changes."},
		{name: "same level", level: "high", rationale: "Still rolls back deploys.", status: types.StepStatusCompleted, trigger: "auto_fix"},
		{name: "single round", single: true},
		{name: "remaining finding", level: "low", rationale: "Some fixes landed.", status: types.StepStatusCompleted, trigger: "auto_fix", remaining: true},
		{name: "review not passed", level: "low", rationale: "Rollback removed.", status: types.StepStatusFailed, trigger: "auto_fix"},
		{name: "answer only", level: "low", rationale: "The answer disproved the risk.", status: types.StepStatusCompleted, trigger: "answer"},
		{name: "missing rationale", level: "low", status: types.StepStatusCompleted, trigger: "auto_fix"},
		{name: "unreadable final", status: types.StepStatusCompleted, trigger: "auto_fix", unreadable: true},
		{name: "missing round result", level: "low", rationale: "Rollback removed.", status: types.StepStatusCompleted, trigger: "auto_fix", noRoundResult: true},
		{name: "no initial findings to close", level: "low", rationale: "Risk was reconsidered.", status: types.StepStatusCompleted, trigger: "auto_fix", initialClean: true},
		{name: "legacy fix", level: "low", rationale: "Rollback removed.", status: types.StepStatusCompleted, trigger: "user_fix", transition: "🚨 High -> ✅ Low: Rollback removed."},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			first := initial
			if test.initialClean {
				first = strings.Replace(first, `[{"id":"review-1","severity":"error","file":"release.go","description":"Deploy rolls back on a gate violation."}]`, "[]", 1)
			}
			rounds := []*db.StepRound{{Round: 1, Trigger: "initial", FindingsJSON: &first}}
			current := first
			if !test.single {
				final := types.Findings{RiskLevel: test.level, RiskRationale: test.rationale, Items: []types.Finding{}}
				if test.remaining {
					final.Items = []types.Finding{{ID: "review-1", Description: "Rollback remains."}}
				}
				encoded, err := json.Marshal(final)
				if err != nil {
					t.Fatal(err)
				}
				current = string(encoded)
				if test.unreadable {
					current = `{"findings":[`
				}
				round := &db.StepRound{Round: 2, Trigger: test.trigger, FindingsJSON: &current}
				if test.noRoundResult {
					round.FindingsJSON = nil
				}
				rounds = append(rounds, round)
			}
			steps := []*db.StepResult{{ID: "review", StepName: types.StepReview, Status: test.status, FindingsJSON: &current}}
			want := original
			if test.transition != "" {
				want += "\n" + test.transition
			}
			for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderAzureDevOps, scm.ProviderGitLab, scm.ProviderGitea, scm.ProviderForgejo, scm.ProviderBitbucket} {
				pipelineMD, risk := buildPipelineSummaryFor(steps, map[string][]*db.StepRound{"review": rounds}, testPipelineHeadSHA, provider, pipelineAttestationPolicy{})
				if risk != want {
					t.Fatalf("%s risk = %q, want %q", provider, risk, want)
				}
				body := assemblePRBody(nil, "## What Changed\n\n- Move the release gate.", risk, "", pipelineMD, scm.MaxPRBodyChars(provider), provider)
				if !strings.Contains(body, "## Risk Assessment\n\n"+want+"\n\n## Pipeline") {
					t.Fatalf("%s lost the recorded risk section:\n%s", provider, body)
				}
			}
		})
	}
}

func TestPRRiskAssessmentKeepsHigherFinalLevelUnchanged(t *testing.T) {
	t.Parallel()
	initial := `{"findings":[{"id":"review-1","description":"Concern."}],"risk_level":"medium","risk_rationale":"Initial concern."}`
	final := `{"findings":[],"risk_level":"high","risk_rationale":"A broader concern remains."}`
	steps := []*db.StepResult{{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &final}}
	rounds := map[string][]*db.StepRound{"review": {{Round: 1, Trigger: "initial", FindingsJSON: &initial}, {Round: 2, Trigger: "auto_fix", FindingsJSON: &final}}}
	if risk := extractRiskLine(steps, rounds); risk != "⚠️ Medium: Initial concern." {
		t.Fatalf("risk = %q", risk)
	}
}

func TestPRRiskAssessmentBudgetKeepsTransition(t *testing.T) {
	t.Parallel()
	transition := "🚨 High -> ✅ Low: Fix rounds removed deploy rollback and preserved the release gate."
	for _, mode := range []string{config.PRAppendixFull, config.PRAppendixCollapsed} {
		for _, oversized := range []bool{false, true} {
			initial := "🚨 High: The release gate could roll back deployments."
			if oversized {
				initial += strings.Repeat(" Release verifier rollback risk 😀.", 3000)
			}
			marker := buildPipelineAttestation([]*db.StepResult{{StepName: types.StepReview, Status: types.StepStatusCompleted}}, nil, testPipelineHeadSHA)
			pipelineMD := "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + marker + "\n\n" + strings.Repeat("- ✅ review - passed\n", 500)
			sctx := &pipeline.StepContext{UserIntent: strings.Repeat("Preserve deploys. ", 400), Config: &config.Config{PR: config.PR{Appendix: mode}}}
			body := assemblePRBody(sctx, "## What Changed\n\n"+strings.Repeat("- Move the gate.\n", 500), initial+"\n"+transition, "## Testing\n\n"+strings.Repeat("Passed. ", 2000), pipelineMD, 4000, scm.ProviderAzureDevOps)
			if scm.PRBodyLen(body) > 4000 || !utf8.ValidString(body) {
				t.Fatalf("%s oversized=%t exceeded the Azure budget: %d", mode, oversized, scm.PRBodyLen(body))
			}
			for _, want := range []string{"## Risk Assessment\n\n🚨 High:", "\n" + transition, marker} {
				if !strings.Contains(body, want) {
					t.Fatalf("%s oversized=%t lost %q:\n%s", mode, oversized, want, body)
				}
			}
			if !oversized && !strings.Contains(body, initial+"\n"+transition) {
				t.Fatalf("%s truncated a risk section that fits:\n%s", mode, body)
			}
		}
	}
}

func TestPRRiskAssessmentGitHubByteBudgetKeepsTransition(t *testing.T) {
	t.Parallel()
	transition := "🚨 High -> ✅ Low: Fix rounds removed deploy rollback."
	risk := "🚨 High: " + strings.Repeat("Release rollback risk 😀. ", 5000) + "\n" + transition
	marker := buildPipelineAttestation([]*db.StepResult{{StepName: types.StepReview, Status: types.StepStatusCompleted}}, nil, testPipelineHeadSHA)
	pipelineMD := "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + marker
	body := buildPRBody("## What Changed\n\n- Move the gate.", risk, "", pipelineMD, nil, scm.ProviderGitHub)
	if len(body) > maxPullRequestBodyBytes || !strings.Contains(body, "\n"+transition) || !strings.Contains(body, marker) {
		t.Fatalf("GitHub byte budget lost the transition or attestation: %d bytes", len(body))
	}
}

func TestPRRiskAssessmentBudgetPrioritizesTransitionAndNeutralizesMarkers(t *testing.T) {
	t.Parallel()
	transition := "🚨 High -> ✅ Low: Rollback removed."
	initial := "🚨 High: " + strings.Repeat("Rollback risk. ", 200)
	for _, clamp := range []struct {
		measure func(string) int
		clamp   func(string, int) string
	}{
		{measure: scm.PRBodyLen, clamp: scm.ClampPRBody},
		{measure: func(text string) int { return len(text) }, clamp: clampPRBytes},
	} {
		budget := clamp.measure(transition)
		if got := clampRiskAssessment(initial+"\n"+transition, budget, clamp.measure, clamp.clamp); got != transition {
			t.Fatalf("tight budget lost the transition: %q", got)
		}
	}
	foreign := pipelineAttestationCommentPrefix + `{"head_sha":"foreign"}` + pipelineAttestationCommentClosingToken
	marker := buildPipelineAttestation([]*db.StepResult{{StepName: types.StepReview, Status: types.StepStatusCompleted}}, nil, testPipelineHeadSHA)
	pipelineMD := "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + marker
	risk := initial + foreign + "\n" + transition + " " + foreign
	body := foldedWithin(risk, "", pipelineMD, 1000)
	if len(body) > 1000 || !strings.Contains(body, transition) || strings.Count(body, pipelineAttestationCommentPrefix) != 1 || !strings.Contains(body, marker) {
		t.Fatalf("budgeted risk broke transition or attestation safety:\n%s", body)
	}
}

func TestPRDescriptionV184Golden(t *testing.T) {
	t.Parallel()
	golden, err := os.ReadFile(filepath.Join("testdata", "pr-v1.84.0.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []scm.Provider{scm.ProviderAzureDevOps, scm.ProviderGitHub} {
		t.Run(string(provider), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			sctx.UserIntent = "Declare the typed quotation detail response for AB#420295. Keep permissions and record scope unchanged."
			sctx.Run.HeadSHA = "482224448222444822244482224448222444822244"
			for _, name := range []types.StepName{types.StepIntent, types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI} {
				step, err := sctx.DB.InsertStepResult(sctx.Run.ID, name)
				if err != nil {
					t.Fatal(err)
				}
				status := types.StepStatusCompleted
				if name == types.StepPR {
					status = types.StepStatusRunning
				}
				if name == types.StepCI {
					status = types.StepStatusPending
				}
				if err := sctx.DB.UpdateStepStatus(step.ID, status); err != nil {
					t.Fatal(err)
				}
				if status == types.StepStatusCompleted {
					if _, err := sctx.DB.InsertStepRound(step.ID, 1, "initial", nil, nil, 10); err != nil {
						t.Fatal(err)
					}
				}
				if name == types.StepReview {
					if err := sctx.DB.SetStepFindings(step.ID, `{"findings":[],"risk_level":"low","risk_rationale":"The response contract is declared without changing permissions."}`); err != nil {
						t.Fatal(err)
					}
				}
			}
			pipelineMD, risk, _ := (&PRStep{}).buildPipelineSection(sctx, provider)
			for _, ending := range []struct{ name, value string }{{"LF", "\n"}, {"CRLF", "\r\n"}} {
				t.Run(ending.name, func(t *testing.T) {
					narrative := strings.ReplaceAll("## What Changed\n\n- Declare the concrete quotation detail response schema.\n- Verify the generated OpenAPI response contract.", "\n", ending.value)
					pipelineText := strings.ReplaceAll(pipelineMD, "\n", ending.value)
					want := strings.ReplaceAll(string(golden), "\r\n", "\n")
					got := assemblePRBody(sctx, narrative, risk, "", pipelineText, scm.MaxPRBodyChars(provider), provider)
					if got+"\n" != want || strings.Contains(got, "\r") {
						t.Fatalf("description differs from v1.84.0 public shape:\n%s", got)
					}
					got = buildPRBody(narrative, risk, "", pipelineText, sctx, provider)
					if got+"\n" != want || strings.Contains(got, "\r") {
						t.Fatalf("byte-budgeted description differs from v1.84.0 public shape:\n%s", got)
					}
				})
			}
		})
	}
}

func TestPRDescriptionNormalizesRecordedSectionLineEndings(t *testing.T) {
	t.Parallel()
	sctx := &pipeline.StepContext{UserIntent: "Keep permissions.\r\nKeep record scope."}
	for _, provider := range []scm.Provider{scm.ProviderAzureDevOps, scm.ProviderGitHub} {
		for _, body := range []string{
			assemblePRBody(sctx, "## What Changed\r\n\r\n- Declare response.\r\n- Verify contract.", "✅ Low: Safe.\r\nPermissions unchanged.", "## Testing\r\n\r\n- Contract passed.", "## Pipeline\r\n\r\n- ✅ review - passed", scm.MaxPRBodyChars(provider), provider),
			buildPRBody("## What Changed\r\n\r\n- Declare response.\r\n- Verify contract.", "✅ Low: Safe.\r\nPermissions unchanged.", "## Testing\r\n\r\n- Contract passed.", "## Pipeline\r\n\r\n- ✅ review - passed", sctx, provider),
		} {
			if strings.Contains(body, "\r") {
				t.Fatalf("%s description contains CRLF: %q", provider, body)
			}
			for _, want := range []string{"Keep permissions.\nKeep record scope.", "- Declare response.\n- Verify contract.", "✅ Low: Safe.\nPermissions unchanged.", "## Testing\n\n- Contract passed.", "## Pipeline\n\n- ✅ review - passed"} {
				if !strings.Contains(body, want) {
					t.Fatalf("%s description lost %q: %q", provider, want, body)
				}
			}
		}
	}
}

func TestPRIntentCompactionPreservesAllSections(t *testing.T) {
	t.Parallel()
	intent := "Keep the quoted requirement \"exactly as supplied\". " + strings.Repeat("Verify behavior with emoji 😀 and Unicode 漢字. ", 90)
	sctx := &pipeline.StepContext{UserIntent: intent}
	marker := buildPipelineAttestation([]*db.StepResult{{StepName: types.StepReview, Status: types.StepStatusCompleted}}, nil, testPipelineHeadSHA)
	pipelineMD := "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + marker + "\n\n- ✅ review - passed"
	got := assemblePRBody(sctx, "## What Changed\n\n- Declare the response.", "✅ Low: No behavior changes.", "## Testing\n\n- Contract check passed.", pipelineMD, 4000, scm.ProviderAzureDevOps)
	if scm.PRBodyLen(got) > 4000 || !utf8.ValidString(got) {
		t.Fatalf("invalid budgeted body: %d", scm.PRBodyLen(got))
	}
	for _, want := range []string{"## Intent", fullIntentCommentNote, "## What Changed\n\n- Declare the response.", prGeneratorLine, "## Risk Assessment\n\n✅ Low: No behavior changes.", "## Testing\n\n- Contract check passed.", marker, "- ✅ review - passed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	preview := strings.Split(strings.TrimPrefix(got, "## Intent\n\n"), "\n\n"+fullIntentCommentNote)[0]
	if !strings.HasSuffix(preview, ".") || !strings.HasPrefix(intent, preview) {
		t.Fatalf("preview is not complete original sentences: %q", preview)
	}
	noSentence := &pipeline.StepContext{UserIntent: strings.Repeat("quoted requirement ", 500)}
	got = assemblePRBody(noSentence, "## What Changed\n\n- Declare the response.", "✅ Low: Safe.", "", pipelineMD, 4000, scm.ProviderAzureDevOps)
	if !strings.HasPrefix(got, "## Intent\n\n"+fullIntentCommentNote+"\n\n## What Changed") {
		t.Fatalf("sentence-free intent was cut mid-quote:\n%s", got)
	}
}

func TestPRTitleUsesPrimaryWorkItem(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ intent, branch string }{
		{"Implement AB#420295. Supports AB#420193.", "feature/detail"},
		{"Declare response.", "refs/heads/AB#420295-detail"},
	} {
		sctx := &pipeline.StepContext{UserIntent: test.intent, Run: &db.Run{Branch: test.branch}}
		got, err := renderPRTitle(sctx, "fix(quotations): declare typed response")
		if err != nil || got != "fix(quotations): declare typed response (AB#420295)" {
			t.Fatalf("title = %q, %v", got, err)
		}
	}
}

func TestIntentCompactionDoesNotCutQuotedSentences(t *testing.T) {
	t.Parallel()
	for _, quoted := range []string{`"First quoted sentence. Second quoted sentence."`, "“First quoted sentence. Second quoted sentence.”"} {
		intent := "Keep scope. " + quoted + " Preserve the remainder."
		preview := compactIntentAtSentence(intent, len("Keep scope. ")+len(quoted)/2, func(text string) int { return len(text) })
		if preview != "Keep scope." {
			t.Fatalf("compaction cut into a quote: %q", preview)
		}
		preview = compactIntentAtSentence(intent, len("Keep scope. ")+len(quoted), func(text string) int { return len(text) })
		if preview != "Keep scope. "+quoted {
			t.Fatalf("compaction lost a complete quoted sentence: %q", preview)
		}
	}
}

func TestGitHubWorkItemBudgetInExplicitAppendixModes(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{config.PRAppendixMinimal, config.PRAppendixCollapsed} {
		sctx := &pipeline.StepContext{Run: &db.Run{Branch: "feature/AB#420295"}, Config: &config.Config{PR: config.PR{Appendix: mode}}}
		body := buildPRBody("## What Changed\n\n"+strings.Repeat("- Preserve behavior.\n", 5000), "✅ Low: Safe.", "", "", sctx, scm.ProviderGitHub)
		if len(body) > maxPullRequestBodyBytes || !strings.Contains(body, "AB#420295") {
			t.Fatalf("%s work item insertion exceeded the body budget: %d", mode, len(body))
		}
	}
	sctx := &pipeline.StepContext{Run: &db.Run{Branch: "feature/AB#42"}}
	if githubWorkItemLine(sctx, "AB#420295") != "- Related work items: AB#42" {
		t.Fatal("a longer work item id incorrectly satisfied a shorter one")
	}
}

func TestGitHubWorkItemsSurviveIntentCompaction(t *testing.T) {
	t.Parallel()
	sctx := &pipeline.StepContext{UserIntent: strings.Repeat("Preserve this sentence. ", 4000) + " Implement AB#420295.", Run: &db.Run{Branch: "feature/detail"}}
	body := buildPRBody("## What Changed\n\n- Declare the response.", "✅ Low: Safe.", "", "", sctx, scm.ProviderGitHub)
	if !strings.Contains(body, "- Related work items: AB#420295\n\n"+prGeneratorLine) || !strings.Contains(body, fullIntentCommentNote) || len(body) > maxPullRequestBodyBytes {
		t.Fatalf("GitHub linking reference lost:\n%s", body)
	}
	sctx = &pipeline.StepContext{UserIntent: "Keep permissions unchanged.", Run: &db.Run{Branch: "feature/AB#420295"}}
	body = buildPRBody("## What Changed\n\n- Declare the response.", "✅ Low: Safe.", "", "", sctx, scm.ProviderGitHub)
	if !strings.Contains(body, "- Related work items: AB#420295") {
		t.Fatal("branch work item was present only in the title")
	}
}

func TestGitHubOversizedCoreKeepsEverySection(t *testing.T) {
	t.Parallel()
	sctx := &pipeline.StepContext{UserIntent: strings.Repeat("Preserve intent. ", 5000)}
	marker := buildPipelineAttestation([]*db.StepResult{{StepName: types.StepReview, Status: types.StepStatusCompleted}}, nil, testPipelineHeadSHA)
	pipelineMD := "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + marker + "\n\n- ✅ review - passed"
	body := buildPRBody("## What Changed\n\n"+strings.Repeat("- Preserve behavior.\n", 5000), "✅ Low: "+strings.Repeat("Safe. ", 20000), "## Testing\n\n"+strings.Repeat("Passed. ", 20000), pipelineMD, sctx, scm.ProviderGitHub)
	if len(body) > maxPullRequestBodyBytes || !utf8.ValidString(body) {
		t.Fatalf("oversized core exceeded the byte budget: %d", len(body))
	}
	for _, want := range []string{"## Intent", fullIntentCommentNote, "## What Changed", prGeneratorLine, "## Risk Assessment\n\n✅ Low:", noMistakesPRSignature, marker, "- ✅ review - passed"} {
		if !strings.Contains(body, want) {
			t.Fatalf("oversized core lost %q", want)
		}
	}
}

type referenceHost struct {
	scm.Host
	comments   []string
	ids        []string
	commentErr error
	linkErr    error
}

func (host *referenceHost) EnsurePRComment(_ context.Context, _ *scm.PR, body string) error {
	host.comments = append(host.comments, body)
	return host.commentErr
}

func (host *referenceHost) LinkPRWorkItems(_ context.Context, _ *scm.PR, ids []string) error {
	host.ids = append(host.ids, ids...)
	return host.linkErr
}

func TestPRReferencesPublishFullIntentAndFailClosed(t *testing.T) {
	t.Parallel()
	sctx := &pipeline.StepContext{Ctx: context.Background(), UserIntent: "Implement AB#420295. Keep \"quoted text\" and Unicode 😀 unchanged.", Run: &db.Run{Branch: "feature/detail"}}
	host := &referenceHost{}
	if err := publishPRReferences(sctx, host, &scm.PR{Number: "42"}, "## Intent\n\n"+fullIntentCommentNote); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(host.comments, []string{"## Full intent\n\n" + sctx.UserIntent}) || !reflect.DeepEqual(host.ids, []string{"420295"}) {
		t.Fatalf("publication = %+v", host)
	}
	host.commentErr = errors.New("comment refused")
	if err := publishPRReferences(sctx, host, &scm.PR{Number: "42"}, fullIntentCommentNote); err == nil {
		t.Fatal("comment failure passed publication")
	}
	host = &referenceHost{linkErr: errors.New("link refused")}
	if err := publishPRReferences(sctx, host, &scm.PR{Number: "42"}, "complete intent"); err == nil {
		t.Fatal("work-item link failure passed publication")
	}
	sctx.Run.OmitIntent = true
	host = &referenceHost{}
	if err := publishPRReferences(sctx, host, &scm.PR{Number: "42"}, fullIntentCommentNote); err != nil || len(host.comments) != 0 || len(host.ids) != 0 {
		t.Fatalf("omitted intent leaked: %+v, %v", host, err)
	}
}

func TestPRDraftCannotReplaceRecordedIntentRiskOrPipeline(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: []byte(`{"title":"fix: typed response","body":"## Intent\n\nParaphrased.\n\n## What Changed\n\n- Declare response.\n\n## Risk\n\nAgent risk.\n\n## Pipeline\n\n- Intent: passed"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.UserIntent = "Declare the typed response for AB#420295. Keep permissions unchanged."
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(step.ID, `{"findings":[],"risk_level":"low","risk_rationale":"No permission changes."}`); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.InsertStepRound(step.ID, 1, "initial", nil, nil, 10); err != nil {
		t.Fatal(err)
	}
	content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", base, scm.ProviderAzureDevOps, 4000)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{sctx.UserIntent, prGeneratorLine, "## Risk Assessment\n\n✅ Low: No permission changes.", noMistakesPRSignature, "- ✅ review - passed"} {
		if !strings.Contains(content.Body, want) {
			t.Fatalf("missing %q:\n%s", want, content.Body)
		}
	}
	for _, forbidden := range []string{"Paraphrased.", "Agent risk.", "- Intent: passed"} {
		if strings.Contains(content.Body, forbidden) {
			t.Fatalf("agent overwrote deterministic section: %s", content.Body)
		}
	}
}
