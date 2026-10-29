package pipeline

import (
	"context"
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

func TestRunAgentInheritsStepEnvironment(t *testing.T) {
	stepEnv := []string{"PATH=selected-runtime", "KEEP=value"}
	invocationEnv := []string{"SPECIFIC=value"}
	sctx := &StepContext{Ctx: context.Background(), Env: stepEnv, Agent: &hangingAgent{name: "environment", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		want := []string{"PATH=selected-runtime", "KEEP=value", "SPECIFIC=value"}
		if !reflect.DeepEqual(opts.Env, want) {
			t.Fatalf("agent environment=%v, want %v", opts.Env, want)
		}
		return &agent.Result{}, nil
	}}}
	if _, err := sctx.RunAgent(agent.RunOpts{Env: invocationEnv}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sctx.Env, stepEnv) || !reflect.DeepEqual(invocationEnv, []string{"SPECIFIC=value"}) {
		t.Fatal("mutated caller environment")
	}
}
