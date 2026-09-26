package usecase_test

import (
	"context"
	"strings"
	"testing"

	"pi-golang/internal/entity"
	"pi-golang/internal/usecase"
)

func TestExecutePlan_UsesFreshConversationForEveryPlannedTask(t *testing.T) {
	llm := &fakeLLM{responses: []entity.ChatResponse{
		{Content: `{"goal":"ship feature","tasks":[{"id":"inspect","title":"Inspect","objective":"inspect current code","dependencies":[],"acceptance_criteria":["record findings"]},{"id":"implement","title":"Implement","objective":"apply the change","dependencies":["inspect"],"acceptance_criteria":["tests pass"]}]}`},
		{Content: `TASK_RESULT: {"status":"complete","summary":"inspection complete","evidence":["read source"],"remaining":[]}`},
		{Content: `TASK_RESULT: {"status":"complete","summary":"implementation complete","evidence":["go test ./..."],"remaining":[]}`},
		{Content: `VERIFICATION_RESULT: {"status":"pass","summary":"verified","evidence":["go test ./..."],"failures":[],"repair_tasks":[],"final_answer":"feature shipped"}`},
	}}
	roles := make([]usecase.OrchestrationRole, 0, 4)
	factory := func(role usecase.OrchestrationRole) *entity.Agent {
		roles = append(roles, role)
		return newAgentWith(llm, nil, nil, nil)
	}

	out, err := usecase.NewRunUsecase(nil).ExecutePlan(context.Background(), factory, usecase.PlanExecutionInput{
		Run: usecase.RunInput{UserPrompt: "ship feature"}, MaxTasks: 8,
		ProtocolAttempts: 2, WorkerAttempts: 1, MaxReplans: 1,
	})
	if err != nil {
		t.Fatalf("ExecutePlan() error = %v", err)
	}
	if !out.Completed || out.FinalAnswer != "feature shipped" || len(out.Plan.Tasks) != 2 || len(out.Runs) != 4 {
		t.Fatalf("ExecutePlan() output = %+v", out)
	}
	wantRoles := []usecase.OrchestrationRole{
		usecase.OrchestrationPlanner, usecase.OrchestrationWorker,
		usecase.OrchestrationWorker, usecase.OrchestrationVerifier,
	}
	for i := range wantRoles {
		if roles[i] != wantRoles[i] {
			t.Fatalf("roles[%d] = %q, want %q", i, roles[i], wantRoles[i])
		}
	}
	calls := llm.callsSnapshot()
	if len(calls) != 4 {
		t.Fatalf("LLM calls = %d, want 4", len(calls))
	}
	for i, call := range calls {
		if len(call.Messages) != 1 || call.Messages[0].Role != entity.RoleUser {
			t.Fatalf("call %d inherited another conversation: %+v", i, call.Messages)
		}
	}
	if !strings.Contains(calls[2].Messages[0].Content, "inspection complete") {
		t.Fatalf("dependent task did not receive structured dependency handoff: %s", calls[2].Messages[0].Content)
	}
}

func TestExecutePlan_VerifierRepairsIncompleteTaskWithoutReplayingIt(t *testing.T) {
	llm := &fakeLLM{responses: []entity.ChatResponse{
		{Content: `{"goal":"fix","tasks":[{"id":"change","title":"Change","objective":"make change","dependencies":[],"acceptance_criteria":["works"]}]}`},
		{Content: `TASK_RESULT: {"status":"incomplete","summary":"partial change","evidence":["edited file"],"remaining":["missing edge case"]}`},
		{Content: `VERIFICATION_RESULT: {"status":"fail","summary":"test failed","evidence":["go test ./..."],"failures":["missing edge case"],"repair_tasks":[{"id":"repair-edge","title":"Repair edge","objective":"fix edge case","dependencies":[],"acceptance_criteria":["tests pass"]}],"final_answer":""}`},
		{Content: `TASK_RESULT: {"status":"complete","summary":"edge repaired","evidence":["go test ./..."],"remaining":[]}`},
		{Content: `VERIFICATION_RESULT: {"status":"pass","summary":"verified","evidence":["go test ./..."],"failures":[],"repair_tasks":[],"final_answer":"fixed and verified"}`},
	}}

	out, err := usecase.NewRunUsecase(nil).ExecutePlan(context.Background(), func(_ usecase.OrchestrationRole) *entity.Agent {
		return newAgentWith(llm, nil, nil, nil)
	}, usecase.PlanExecutionInput{
		Run: usecase.RunInput{UserPrompt: "fix"}, MaxTasks: 8,
		ProtocolAttempts: 1, WorkerAttempts: 1, MaxReplans: 2,
	})
	if err != nil {
		t.Fatalf("ExecutePlan() error = %v", err)
	}
	if !out.Completed || out.Replans != 1 || len(out.Plan.Tasks) != 2 || len(out.TaskResults) != 2 {
		t.Fatalf("repair execution output = %+v", out)
	}
	if out.FinalAnswer != "fixed and verified" {
		t.Fatalf("FinalAnswer = %q", out.FinalAnswer)
	}
	if got := out.TaskResults["change"].Status; got != "incomplete" {
		t.Fatalf("original worker status = %q, want incomplete", got)
	}
}

func TestExecutePlan_RejectsCyclicPlanThenRequestsCorrection(t *testing.T) {
	llm := &fakeLLM{responses: []entity.ChatResponse{
		{Content: `{"goal":"bad","tasks":[{"id":"a","title":"A","objective":"A","dependencies":["b"],"acceptance_criteria":["A"]},{"id":"b","title":"B","objective":"B","dependencies":["a"],"acceptance_criteria":["B"]}]}`},
		{Content: `{"goal":"good","tasks":[{"id":"a","title":"A","objective":"A","dependencies":[],"acceptance_criteria":["A"]}]}`},
		{Content: `TASK_RESULT: {"status":"complete","summary":"done","evidence":["A"],"remaining":[]}`},
		{Content: `VERIFICATION_RESULT: {"status":"pass","summary":"ok","evidence":["A"],"failures":[],"repair_tasks":[],"final_answer":"done"}`},
	}}

	out, err := usecase.NewRunUsecase(nil).ExecutePlan(context.Background(), func(_ usecase.OrchestrationRole) *entity.Agent {
		return newAgentWith(llm, nil, nil, nil)
	}, usecase.PlanExecutionInput{
		Run: usecase.RunInput{UserPrompt: "work"}, MaxTasks: 4,
		ProtocolAttempts: 2, WorkerAttempts: 1, MaxReplans: 0,
	})
	if err != nil {
		t.Fatalf("ExecutePlan() error = %v", err)
	}
	if !out.Completed || len(out.Runs) != 4 {
		t.Fatalf("corrected plan output = %+v", out)
	}
	calls := llm.callsSnapshot()
	if !strings.Contains(calls[1].Messages[0].Content, "cycle") {
		t.Fatalf("planner retry did not explain rejection: %s", calls[1].Messages[0].Content)
	}
}
