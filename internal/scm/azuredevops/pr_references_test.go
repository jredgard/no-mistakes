package azuredevops

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestLinkPRWorkItemsScopesOrganization(t *testing.T) {
	t.Parallel()
	host := newTestHost(map[string]azdoTestResponse{
		"az repos pr work-item add --id 42 --work-items 420295 420193 --organization " + testOrg + " --output json": {stdout: "[]"},
	})
	if err := host.LinkPRWorkItems(context.Background(), &scm.PR{Number: "42"}, []string{"420295", "420193"}); err != nil {
		t.Fatal(err)
	}
	if err := host.LinkPRWorkItems(context.Background(), &scm.PR{}, []string{"420295"}); err == nil {
		t.Fatal("missing identity accepted")
	}
}

func TestEnsurePRCommentPreservesFullIntentAndAvoidsDuplicates(t *testing.T) {
	t.Parallel()
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "create", true: "already posted"}[existing], func(t *testing.T) {
			body := "## Full intent\n\n" + strings.Repeat("Keep \"quoted\" intent 😀 unchanged. ", 200)
			var calls [][]string
			factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
				calls = append(calls, args)
				response := azdoTestResponse{stdout: `{"value":[]}`}
				post := false
				for index, argument := range args {
					if argument == "--in-file" {
						post = true
						payload, err := os.ReadFile(args[index+1])
						if err != nil {
							t.Fatal(err)
						}
						var decoded struct {
							Comments []struct {
								Content     string `json:"content"`
								CommentType int    `json:"commentType"`
							} `json:"comments"`
							Status int `json:"status"`
						}
						if err := json.Unmarshal(payload, &decoded); err != nil {
							t.Fatal(err)
						}
						if len(decoded.Comments) != 1 || decoded.Comments[0].Content != body || decoded.Comments[0].CommentType != 1 || decoded.Status != 4 {
							t.Fatalf("comment payload changed: %s", payload)
						}
					}
				}
				if existing && !post {
					payload, _ := json.Marshal(map[string]any{"value": []any{map[string]any{"comments": []any{map[string]any{"content": body}}}}})
					response.stdout = string(payload)
				}
				key := name + " " + strings.Join(args, " ")
				return azdoTestCmdFactory(map[string]azdoTestResponse{key: response})(ctx, name, args...)
			}
			host := New(factory, nil, testOrg, testProject, testRepo)
			if err := host.EnsurePRComment(context.Background(), &scm.PR{Number: "42"}, body); err != nil {
				t.Fatal(err)
			}
			wantCalls := 2
			if existing {
				wantCalls = 1
			}
			if len(calls) != wantCalls {
				t.Fatalf("calls = %v", calls)
			}
			wantPrefix := []string{"devops", "invoke", "--area", "git", "--resource", "pullRequestThreads", "--route-parameters", "project=" + testProject, "repositoryId=" + testRepo, "pullRequestId=42", "--organization", testOrg, "--api-version", "7.1", "--output", "json"}
			if !reflect.DeepEqual(calls[0], wantPrefix) {
				t.Fatalf("unscoped thread read: %v", calls[0])
			}
		})
	}
}

func TestEnsurePRCommentFailsClosedOnIncompleteRead(t *testing.T) {
	t.Parallel()
	key := "az devops invoke --area git --resource pullRequestThreads --route-parameters project=" + testProject + " repositoryId=" + testRepo + " pullRequestId=42 --organization " + testOrg + " --api-version 7.1 --output json"
	for _, response := range []string{"{}", `{"value":null}`, "not json"} {
		host := newTestHost(map[string]azdoTestResponse{key: {stdout: response}})
		if err := host.EnsurePRComment(context.Background(), &scm.PR{Number: "42"}, "intent"); err == nil {
			t.Fatalf("incomplete read accepted: %s", response)
		}
	}
}
