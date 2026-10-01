package steps

import (
	"context"
	"errors"
	"os"
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

func TestPRDescriptionV184Golden(t *testing.T) {
	t.Parallel()
	golden, err := os.ReadFile("testdata/pr-v1.84.0.md")
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
			got := assemblePRBody(sctx, "## What Changed\n\n- Declare the concrete quotation detail response schema.\n- Verify the generated OpenAPI response contract.", risk, "", pipelineMD, scm.MaxPRBodyChars(provider), provider)
			if got+"\n" != string(golden) {
				t.Fatalf("description differs from v1.84.0 public shape:\n%s", got)
			}
		})
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
