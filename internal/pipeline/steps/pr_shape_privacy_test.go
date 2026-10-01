package steps

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestCompactPRShapeKeepsPublicationRedaction(t *testing.T) {
	t.Parallel()
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderAzureDevOps} {
		content := buildHomePathLeakPRContentFor(t, homePathLeakCase{
			name:       "compact published evidence",
			userIntent: "Keep fixtures under " + fixtureHome + "/tmp. " + strings.Repeat("Preserve the full intent sentence. ", 2500),
			agentBody:  "## What Changed\n\n- Stop writing under " + fixtureHome + "/tmp.",
		}, scm.MaxPRBodyChars(provider), provider)
		for _, forbidden := range homePathLeakNeedles {
			if strings.Contains(content.Body, forbidden) {
				t.Fatalf("published home leaked: %s", content.Body)
			}
		}
		if !strings.Contains(content.Body, "~/tmp") {
			t.Fatal("narrative path dropped rather than redacted")
		}
	}
}

func TestFullIntentCommentRedactsHomePathsAndForeignAttestations(t *testing.T) {
	t.Parallel()
	foreign := pipelineAttestationCommentPrefix + `{"head_sha":"foreign","steps":[]}` + pipelineAttestationCommentClosingToken
	sctx := &pipeline.StepContext{Ctx: context.Background(), UserIntent: "Inspect " + fixtureHome + "/tmp. " + foreign}
	host := &referenceHost{}
	if err := publishPRReferences(sctx, host, &scm.PR{Number: "42"}, fullIntentCommentNote); err != nil {
		t.Fatal(err)
	}
	if len(host.comments) != 1 || strings.Contains(host.comments[0], fixtureHome) || strings.Contains(host.comments[0], pipelineAttestationCommentPrefix) || !strings.Contains(host.comments[0], "~/tmp") || !strings.Contains(host.comments[0], "foreign") {
		t.Fatalf("comment privacy boundary failed: %v", host.comments)
	}
}
