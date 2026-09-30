package azuredevops

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestGetChecksIdentifiesUnstartedPolicyBuild(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name      string
		status    string
		kind      string
		context   string
		unstarted bool
	}{
		{name: "zero build id", status: "queued", kind: "Build", context: `{"buildId":0}`, unstarted: true},
		{name: "absent build id", status: "queued", kind: "Build", context: `{}`, unstarted: true},
		{name: "null context", status: "queued", kind: "Build", context: `null`, unstarted: true},
		{name: "null build id", status: "queued", kind: "Build", context: `{"buildId":null}`, unstarted: true},
		{name: "string zero build id", status: "queued", kind: "Build", context: `{"buildId":"0"}`, unstarted: true},
		{name: "allocated build", status: "queued", kind: "Build", context: `{"buildId":123}`},
		{name: "allocated string build", status: "queued", kind: "Build", context: `{"buildId":"123"}`},
		{name: "unknown build id", status: "queued", kind: "Build", context: `{"buildId":{}}`},
		{name: "running build", status: "running", kind: "Build", context: `{"buildId":123}`},
		{name: "running without build id", status: "running", kind: "Build", context: `{}`},
		{name: "status check", status: "queued", kind: "Status", context: `{"buildId":0}`},
		{name: "completed build", status: "approved", kind: "Build", context: `{"buildId":0}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			policyJSON := `[{"status":"` + testCase.status + `","configuration":{"type":{"displayName":"` + testCase.kind + `"},"settings":{"displayName":"arbitrary check name"}},"context":` + testCase.context + `}]`
			host := newTestHost(map[string]azdoTestResponse{
				"az repos pr policy list --id 42 --organization " + testOrg + " --output json": {stdout: policyJSON},
			})
			checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "42"})
			if err != nil {
				t.Fatal(err)
			}
			if len(checks) != 1 || checks[0].UnstartedPolicyBuild != testCase.unstarted {
				t.Fatalf("checks = %+v, want UnstartedPolicyBuild = %v", checks, testCase.unstarted)
			}
			if checks[0].Bucket != azStatusBucket(testCase.status) {
				t.Fatalf("bucket = %s, want unchanged %s", checks[0].Bucket, azStatusBucket(testCase.status))
			}
		})
	}
}
