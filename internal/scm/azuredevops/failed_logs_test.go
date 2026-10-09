package azuredevops

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const failedPoliciesJSON = `[
 {"evaluationId":"selected","status":"rejected","configuration":{"type":{"displayName":"Build"},"settings":{"displayName":"CI validation"}},"context":{"buildId":101,"buildDefinitionName":"ci-build"}},
 {"evaluationId":"other","status":"rejected","configuration":{"type":{"displayName":"Build"},"settings":{"displayName":"Other build"}},"context":{"buildId":"102"}},
 {"status":"approved","configuration":{"type":{"displayName":"Build"},"settings":{"displayName":"CI validation"}},"context":{"buildId":103}},
 {"status":"rejected","configuration":{"type":{"displayName":"Status"},"settings":{"displayName":"External status"}},"context":{"buildId":104}},
 {"status":"rejected","configuration":{"type":{"displayName":"Required reviewers"}},"context":{"buildId":105}}
]`

// The REST timeline contains both aggregate job records and individual task
// records. Logs requested with application/json are a count/value line array.
const failedTimelineJSON = `{"records":[
 {"id":"task","parentId":"job","type":"Task","name":"Run tests","result":"failed","log":{"id":7}},
 {"id":"job","type":"Job","name":"Linux tests","result":"failed","log":{"id":8}},
 {"id":"cancelled","type":"Task","name":"Cancelled cleanup","result":"canceled","log":{"id":9}},
 {"id":"passed","type":"Task","name":"Compile","result":"succeeded","log":{"id":10}},
 {"id":"stage","type":"Stage","name":"Validation","result":"failed","log":{"id":11}},
 {"id":"no-log","type":"Task","name":"Setup","result":"failed","log":null},
 {"id":"duplicate","type":"Task","name":"Repeated record","result":"failed","log":{"id":7}}
]}`

func invokeBuildKey(resource, buildID string, logID int) string {
	key := "az devops invoke --area build --resource " + resource + " --route-parameters project=" + testProject + " buildId=" + buildID
	if logID > 0 {
		key += fmt.Sprintf(" logId=%d", logID)
	}
	return key + " --organization " + testOrg + " --api-version 7.1 --http-method GET --accept-media-type application/json --output json"
}

func policyListKey() string {
	return "az repos pr policy list --id 42 --organization " + testOrg + " --output json"
}

func logResponses() map[string]azdoTestResponse {
	return map[string]azdoTestResponse{
		policyListKey():                      {stdout: failedPoliciesJSON, stderr: "preview command notice\n"},
		invokeBuildKey("timeline", "101", 0): {stdout: failedTimelineJSON, stderr: "token refresh notice\n"},
		invokeBuildKey("logs", "101", 7):     {stdout: `{"count":2,"value":["test output","FAIL TestExample: expected 2, got 1"]}`},
		invokeBuildKey("logs", "101", 8):     {stdout: `{"count":1,"value":["Job failed: exit code 1"]}`},
	}
}

func TestFetchFailedCheckLogsFromTimeline(t *testing.T) {
	t.Parallel()
	responses := logResponses()
	var calls []string
	factory := azdoTestCmdFactory(responses)
	h := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return factory(ctx, name, args...)
	}, nil, testOrg, testProject, testRepo)
	logs, err := h.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "42"}, "feature", "synthetic-merge-sha", []string{"CI validation"})
	if err != nil {
		t.Fatal(err)
	}
	want := "=== Check \"CI validation\" / Job \"Linux tests\" / Task \"Run tests\" (build 101, log 7) ===\ntest output\nFAIL TestExample: expected 2, got 1\n\n" +
		"=== Check \"CI validation\" / Job \"Linux tests\" (build 101, log 8) ===\nJob failed: exit code 1"
	if logs != want {
		t.Fatalf("logs = %q, want %q", logs, want)
	}
	wantCalls := []string{policyListKey(), invokeBuildKey("timeline", "101", 0), invokeBuildKey("logs", "101", 7), invokeBuildKey("logs", "101", 8)}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %v, want %v (no cancelled, passing, duplicate, unrelated, or non-task/job logs)", calls, wantCalls)
	}
}

func TestFetchFailedCheckTargetLogsMatchesEvaluationIdentity(t *testing.T) {
	t.Parallel()
	responses := logResponses()
	responses[invokeBuildKey("timeline", "102", 0)] = azdoTestResponse{stdout: `{"records":[{"type":"Job","name":"Windows tests","result":"failed","log":{"id":12}}]}`}
	responses[invokeBuildKey("logs", "102", 12)] = azdoTestResponse{stdout: `{"count":1,"value":["Windows failure"]}`}
	h := newTestHost(responses)
	// A display-name collision must never redirect an identity-bound target.
	targets := []scm.CheckTarget{{Name: "CI validation", ProviderID: "azure-policy-evaluation:other"}, {Name: "External status"}, {Name: "CI validation", ProviderID: "azure-policy-evaluation:missing"}}
	logs, err := h.FetchFailedCheckTargetLogs(context.Background(), &scm.PR{Number: "42"}, "", "", targets)
	if err != nil || len(logs) != len(targets) {
		t.Fatalf("logs = %+v, error = %v", logs, err)
	}
	if logs[0].Target != targets[0] || logs[0].Err != nil || !strings.Contains(logs[0].Output, "Windows failure") || strings.Contains(logs[0].Output, "Linux") {
		t.Fatalf("identity-matched log = %+v", logs[0])
	}
	for _, log := range logs[1:] {
		if log.Err == nil || log.Output != "" {
			t.Fatalf("unsupported or missing target = %+v, want explicit missing evidence", log)
		}
	}
}

func TestFetchFailedCheckLogsUnavailableEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, key string
		response  azdoTestResponse
		wantErr   string
	}{
		{"no timeline", invokeBuildKey("timeline", "101", 0), azdoTestResponse{stdout: `null`}, "missing or malformed timeline"},
		{"no records", invokeBuildKey("timeline", "101", 0), azdoTestResponse{stdout: `{}`}, "missing or malformed timeline"},
		{"empty timeline", invokeBuildKey("timeline", "101", 0), azdoTestResponse{stdout: `{"records":[]}`}, "no failed task or job logs"},
		{"malformed timeline", invokeBuildKey("timeline", "101", 0), azdoTestResponse{stdout: `not JSON`}, "missing or malformed timeline"},
		{"timeline denied", invokeBuildKey("timeline", "101", 0), azdoTestResponse{stderr: "Build read denied", code: 1}, "Build read denied"},
		{"no build ID", policyListKey(), azdoTestResponse{stdout: `[{"status":"rejected","configuration":{"type":{"displayName":"Build"},"settings":{"displayName":"CI validation"}},"context":{}}]`}, "positive buildId"},
		{"invalid build ID", policyListKey(), azdoTestResponse{stdout: `[{"status":"rejected","configuration":{"type":{"displayName":"Build"},"settings":{"displayName":"CI validation"}},"context":{"buildId":"../102"}}]`}, "positive buildId"},
		{"policies denied", policyListKey(), azdoTestResponse{stderr: "policy read denied", code: 1}, "policy read denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			responses := logResponses()
			responses[tc.key] = tc.response
			logs, err := newTestHost(responses).FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "42"}, "", "", []string{"CI validation"})
			if logs != "" || err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("logs = %q, err = %v, want no logs and %q error", logs, err, tc.wantErr)
			}
		})
	}
}

func TestFetchFailedCheckLogsKeepsPartialEvidence(t *testing.T) {
	t.Parallel()
	for _, response := range []azdoTestResponse{
		{stderr: "log expired", code: 1},
		{stdout: `{"value":null}`},
		{stdout: `{"value":[]}`},
		{stdout: `{"value":[123]}`},
		{stdout: `not JSON`},
	} {
		responses := logResponses()
		responses[invokeBuildKey("logs", "101", 7)] = response
		logs, err := newTestHost(responses).FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "42"}, "", "", []string{"CI validation"})
		if !strings.Contains(logs, "Job failed: exit code 1") || err == nil || !strings.Contains(err.Error(), "log 7") {
			t.Fatalf("partial logs = %q, err = %v", logs, err)
		}
	}
}

func TestFetchFailedCheckLogsBoundsEachLogAndCheck(t *testing.T) {
	t.Parallel()
	responses := logResponses()
	for _, id := range []int{7, 8} {
		payload, err := json.Marshal(map[string]any{"count": 1, "value": []string{"BEGIN OMITTED\n" + strings.Repeat("é", maxFailedLogBytes) + fmt.Sprintf("\nTAIL LOG %d", id)}})
		if err != nil {
			t.Fatal(err)
		}
		responses[invokeBuildKey("logs", "101", id)] = azdoTestResponse{stdout: string(payload)}
	}
	// Two selected checks share the provider budget; each check has two logs.
	logs, err := newTestHost(responses).FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "42"}, "", "", []string{"CI validation", "CI validation"})
	if err != nil || len(logs) > maxFailedLogBytes || !utf8.ValidString(logs) {
		t.Fatalf("logs have %d bytes, valid UTF-8 %v, err %v", len(logs), utf8.ValidString(logs), err)
	}
	for _, text := range []string{"TAIL LOG 7", "TAIL LOG 8", "log truncated", `Job "Linux tests"`, `Task "Run tests"`} {
		if strings.Count(logs, text) < 2 {
			t.Fatalf("logs missing both copies of %q", text)
		}
	}
	if strings.Contains(logs, "BEGIN OMITTED") {
		t.Fatal("kept start of oversized log instead of tail")
	}
}

func TestFetchFailedCheckLogsNoTargetsAndMissingPR(t *testing.T) {
	t.Parallel()
	h := newTestHost(nil)
	logs, err := h.FetchFailedCheckLogs(context.Background(), nil, "", "", nil)
	if logs != "" || err != nil {
		t.Fatalf("no-target logs = %q, err = %v", logs, err)
	}
	_, err = h.FetchFailedCheckLogs(context.Background(), nil, "", "", []string{"CI validation"})
	if err == nil || !strings.Contains(err.Error(), "missing PR id") {
		t.Fatalf("missing-PR err = %v", err)
	}
}

func TestBoundedBuildLogTinyBudgets(t *testing.T) {
	t.Parallel()
	for budget := 0; budget < 100; budget++ {
		output := boundedBuildLog("=== é ===\n", strings.Repeat("é", 100), budget)
		if len(output) > budget || !utf8.ValidString(output) {
			t.Fatalf("budget %d: %q (%d bytes)", budget, output, len(output))
		}
	}
}

func TestFetchFailedCheckLogsBuildIDs(t *testing.T) {
	t.Parallel()
	for _, buildID := range []string{`12345678`, `"12345678"`} {
		responses := logResponses()
		responses[policyListKey()] = azdoTestResponse{stdout: `[{"status":"rejected","configuration":{"type":{"displayName":"Build"},"settings":{"displayName":"CI validation"}},"context":{"buildId":` + buildID + `}}]`}
		responses[invokeBuildKey("timeline", "12345678", 0)] = responses[invokeBuildKey("timeline", "101", 0)]
		for _, logID := range []int{7, 8} {
			responses[invokeBuildKey("logs", "12345678", logID)] = responses[invokeBuildKey("logs", "101", logID)]
		}
		logs, err := newTestHost(responses).FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "42"}, "", "", []string{"CI validation"})
		if err != nil || !strings.Contains(logs, "build 12345678") {
			t.Fatalf("buildId %s: logs = %q, err = %v", buildID, logs, err)
		}
	}
}

func TestFetchFailedCheckLogsHonorsCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	logs, err := newTestHost(logResponses()).FetchFailedCheckLogs(ctx, &scm.PR{Number: "42"}, "", "", []string{"CI validation"})
	if logs != "" || err == nil {
		t.Fatalf("cancelled logs = %q, err = %v", logs, err)
	}
}
