package github

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestEnsurePRCommentStreamsFullIntentAndAvoidsDuplicates(t *testing.T) {
	t.Parallel()
	body := "## Full intent\n\nKeep \"quotes\", newline\nand Unicode 😀."
	for _, existing := range []bool{false, true} {
		response := `{"comments":[]}`
		if existing {
			payload, _ := json.Marshal(map[string]any{"comments": []any{map[string]any{"body": body}}})
			response = string(payload)
		}
		responses := map[string]githubTestResponse{"gh pr view 42 --repo test/repo --json comments": {stdout: response}}
		if !existing {
			responses["gh pr comment 42 --repo test/repo --body-file -"] = githubTestResponse{stdout: "posted", wantStdin: body}
		}
		host := New(githubTestCmdFactory(responses), nil, "", "test/repo")
		if err := host.EnsurePRComment(context.Background(), &scm.PR{Number: "42"}, body); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEnsurePRCommentFailsClosed(t *testing.T) {
	t.Parallel()
	for _, response := range []string{"{}", `{"comments":null}`, "invalid"} {
		host := New(githubTestCmdFactory(map[string]githubTestResponse{"gh pr view 42 --repo test/repo --json comments": {stdout: response}}), nil, "", "test/repo")
		if err := host.EnsurePRComment(context.Background(), &scm.PR{Number: "42"}, "intent"); err == nil {
			t.Fatalf("incomplete response accepted: %s", response)
		}
	}
}
