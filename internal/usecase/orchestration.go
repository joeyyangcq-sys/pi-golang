package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"pi-golang/internal/entity"
)

const orchestrationHandoffLimit = 4000

var taskIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// OrchestrationRole lets the outer layer grant each conversation the minimum
// tool set it needs without coupling the usecase to concrete tools.
type OrchestrationRole string

const (
	OrchestrationPlanner  OrchestrationRole = "planner"
	OrchestrationWorker   OrchestrationRole = "worker"
	OrchestrationVerifier OrchestrationRole = "verifier"
)

// PlannedTask is one independently executable work package. Dependencies form
// a DAG; acceptance criteria are the contract between planner and worker.
type PlannedTask struct {
	ID                 string   `json:"id"`
	Title              string   `json:"title"`
	Objective          string   `json:"objective"`
	Dependencies       []string `json:"dependencies"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
}

// ExecutionPlan is generated before mutation begins and may be extended by a
// verifier with repair tasks after observing the actual workspace.
type ExecutionPlan struct {
	Goal  string        `json:"goal"`
	Tasks []PlannedTask `json:"tasks"`
}

// TaskResult is the structured handoff returned by one worker conversation.
type TaskResult struct {
	Status    string   `json:"status"`
	Summary   string   `json:"summary"`
	Evidence  []string `json:"evidence"`
	Remaining []string `json:"remaining"`
}

// VerificationResult is produced by a fresh verifier conversation after every
// execution wave. A failed verification can append concrete repair tasks.
type VerificationResult struct {
	Status      string        `json:"status"`
	Summary     string        `json:"summary"`
	Evidence    []string      `json:"evidence"`
	Failures    []string      `json:"failures"`
	RepairTasks []PlannedTask `json:"repair_tasks"`
	FinalAnswer string        `json:"final_answer"`
}

// PlanExecutionInput configures semantic work limits. These are circuit
// breakers; they do not determine how many conversations a valid plan uses.
type PlanExecutionInput struct {
	Run              RunInput
	MaxTasks         int
	ProtocolAttempts int
	WorkerAttempts   int
	MaxReplans       int
}

// AgentRunRecord makes every planner, worker and verifier turn visible to
// audit/reporting code. ConversationID groups protocol retries that continue
// in the same Agent conversation.
type AgentRunRecord struct {
	ConversationID string
	Role           OrchestrationRole
	TaskID         string
	Attempt        int
	Output         RunOutput
}

// PlanExecutionOutput is the aggregate result of plan-driven orchestration.
type PlanExecutionOutput struct {
	OrchestrationID string
	Completed       bool
	FinalAnswer     string
	Plan            ExecutionPlan
	TaskResults     map[string]TaskResult
	Runs            []AgentRunRecord
	Conversations   int
	Replans         int
	Usage           entity.TokenUsage
	Compactions     int
	Elapsed         time.Duration
	FailureReason   string
}

// ExecutePlan creates a task DAG, runs each ready task in its own fresh
// conversation, then asks a fresh verifier to inspect the integrated result.
// Verification failures may append repair tasks and resume the same scheduler.
func (uc *RunUsecase) ExecutePlan(
	ctx context.Context,
	newAgent func(OrchestrationRole) *entity.Agent,
	in PlanExecutionInput,
) (out PlanExecutionOutput, err error) {
	started := time.Now()
	defer func() { out.Elapsed = time.Since(started) }()
	if newAgent == nil {
		return out, errors.New("plan execution: agent factory is nil")
	}
	if in.MaxTasks < 1 || in.ProtocolAttempts < 1 || in.WorkerAttempts < 1 || in.MaxReplans < 0 {
		return out, errors.New("plan execution: invalid task or retry limits")
	}
	original := strings.TrimSpace(in.Run.UserPrompt)
	if original == "" {
		return out, errors.New("plan execution: user prompt is empty")
	}
	out.TaskResults = make(map[string]TaskResult)
	out.OrchestrationID = newRunID()
	in.Run.OrchestrationID = out.OrchestrationID

	plan, ok, planFailure, err := uc.generatePlan(ctx, newAgent, in, &out)
	if err != nil {
		return out, err
	}
	if !ok {
		out.FailureReason = planFailure
		out.FinalAnswer = planFailure
		return out, nil
	}
	if strings.TrimSpace(plan.Goal) == "" {
		plan.Goal = original
	}
	out.Plan = plan

	for {
		if err := uc.executePendingTasks(ctx, newAgent, in, original, &out); err != nil {
			return out, err
		}
		verification, verified, verifyFailure, err := uc.verifyPlan(ctx, newAgent, in, original, &out)
		if err != nil {
			return out, err
		}
		if verified && strings.EqualFold(verification.Status, "pass") {
			out.Completed = true
			out.FinalAnswer = strings.TrimSpace(verification.FinalAnswer)
			if out.FinalAnswer == "" {
				out.FinalAnswer = strings.TrimSpace(verification.Summary)
			}
			return out, nil
		}
		if out.Replans >= in.MaxReplans {
			out.FailureReason = verifyFailure
			out.FinalAnswer = verifyFailure
			return out, nil
		}
		if len(verification.RepairTasks) == 0 {
			out.FailureReason = "verification failed without executable repair tasks: " + verifyFailure
			out.FinalAnswer = out.FailureReason
			return out, nil
		}
		candidate := out.Plan
		candidate.Tasks = append(append([]PlannedTask{}, candidate.Tasks...), verification.RepairTasks...)
		if err := validateExecutionPlan(candidate, in.MaxTasks); err != nil {
			out.FailureReason = "invalid repair plan: " + err.Error()
			out.FinalAnswer = out.FailureReason
			return out, nil
		}
		out.Plan = candidate
		out.Replans++
	}
}

func (uc *RunUsecase) generatePlan(
	ctx context.Context,
	newAgent func(OrchestrationRole) *entity.Agent,
	in PlanExecutionInput,
	out *PlanExecutionOutput,
) (ExecutionPlan, bool, string, error) {
	prompt := plannerPrompt(in.Run.UserPrompt, in.MaxTasks)
	lastFailure := "planner did not return a valid execution plan"
	agent := newAgent(OrchestrationPlanner)
	conversationID := newRunID()
	out.Conversations++
	for attempt := 1; attempt <= in.ProtocolAttempts; attempt++ {
		runInput := in.Run
		runInput.UserPrompt = prompt
		runInput.TaskProfile = entity.TaskProfileAgentReadonly
		runInput.OrchestrationConversation = conversationID
		runInput.OrchestrationRole = string(OrchestrationPlanner)
		runInput.OrchestrationAttempt = attempt
		runOutput, err := uc.Execute(ctx, agent, runInput)
		recordAgentRun(out, AgentRunRecord{ConversationID: conversationID, Role: OrchestrationPlanner, Attempt: attempt, Output: runOutput})
		if err != nil {
			if ctx.Err() != nil {
				return ExecutionPlan{}, false, "", fmt.Errorf("plan execution: planner attempt %d: %w", attempt, err)
			}
			lastFailure = fmt.Sprintf("planner request failed: %v", err)
			prompt = plannerRetryPrompt(lastFailure)
			continue
		}
		var plan ExecutionPlan
		if runOutput.StopReason == RunStopFinalAnswer {
			if decodeJSONDocument(runOutput.FinalAnswer, &plan) == nil {
				if validationErr := validateExecutionPlan(plan, in.MaxTasks); validationErr == nil {
					return plan, true, "", nil
				} else {
					lastFailure = validationErr.Error()
				}
			} else {
				lastFailure = "planner output was not valid JSON"
			}
		} else {
			lastFailure = "planner stopped with " + string(runOutput.StopReason)
		}
		prompt = plannerRetryPrompt(lastFailure)
	}
	return ExecutionPlan{}, false, lastFailure, nil
}

func (uc *RunUsecase) executePendingTasks(
	ctx context.Context,
	newAgent func(OrchestrationRole) *entity.Agent,
	in PlanExecutionInput,
	original string,
	out *PlanExecutionOutput,
) error {
	for len(out.TaskResults) < len(out.Plan.Tasks) {
		progress := false
		for _, task := range out.Plan.Tasks {
			if _, done := out.TaskResults[task.ID]; done || !dependenciesComplete(task.Dependencies, out.TaskResults) {
				continue
			}
			result, err := uc.executeTask(ctx, newAgent, in, original, out, task)
			if err != nil {
				return err
			}
			out.TaskResults[task.ID] = result
			progress = true
		}
		if !progress {
			for _, task := range out.Plan.Tasks {
				if _, terminal := out.TaskResults[task.ID]; terminal {
					continue
				}
				out.TaskResults[task.ID] = TaskResult{
					Status: "blocked", Summary: "dependencies did not complete",
					Remaining: append([]string(nil), task.Dependencies...),
				}
			}
		}
	}
	return nil
}

func (uc *RunUsecase) executeTask(
	ctx context.Context,
	newAgent func(OrchestrationRole) *entity.Agent,
	in PlanExecutionInput,
	original string,
	out *PlanExecutionOutput,
	task PlannedTask,
) (TaskResult, error) {
	handoff := ""
	prompt := workerPrompt(original, out.Plan, task, out.TaskResults, handoff)
	lastResult := TaskResult{Status: "incomplete", Summary: "worker did not return a valid completion result"}
	agent := newAgent(OrchestrationWorker)
	conversationID := newRunID()
	out.Conversations++
	for attempt := 1; attempt <= in.WorkerAttempts; attempt++ {
		runInput := in.Run
		runInput.UserPrompt = prompt
		runInput.OrchestrationConversation = conversationID
		runInput.OrchestrationRole = string(OrchestrationWorker)
		runInput.OrchestrationTask = task.ID
		runInput.OrchestrationAttempt = attempt
		runInput.PlanRevision = out.Replans
		runOutput, err := uc.Execute(ctx, agent, runInput)
		recordAgentRun(out, AgentRunRecord{ConversationID: conversationID, Role: OrchestrationWorker, TaskID: task.ID, Attempt: attempt, Output: runOutput})
		if err != nil {
			if ctx.Err() != nil {
				return TaskResult{}, fmt.Errorf("plan execution: task %s attempt %d: %w", task.ID, attempt, err)
			}
			lastResult = TaskResult{Status: "incomplete", Summary: err.Error()}
			handoff = "Previous attempt failed: " + err.Error()
			prompt = workerRetryPrompt(handoff)
			continue
		}
		var result TaskResult
		parseErr := decodeTaggedJSON(runOutput.FinalAnswer, "TASK_RESULT:", &result)
		if parseErr == nil {
			lastResult = result
		}
		if runOutput.StopReason == RunStopFinalAnswer && parseErr == nil &&
			strings.EqualFold(result.Status, "complete") && len(result.Remaining) == 0 {
			return result, nil
		}
		handoff = taskAttemptHandoff(runOutput, parseErr)
		prompt = workerRetryPrompt(handoff)
	}
	if strings.TrimSpace(lastResult.Status) == "" || strings.EqualFold(lastResult.Status, "complete") {
		lastResult.Status = "incomplete"
	}
	return lastResult, nil
}

func (uc *RunUsecase) verifyPlan(
	ctx context.Context,
	newAgent func(OrchestrationRole) *entity.Agent,
	in PlanExecutionInput,
	original string,
	out *PlanExecutionOutput,
) (VerificationResult, bool, string, error) {
	prompt := verifierPrompt(original, out.Plan, out.TaskResults, out.Replans, in.MaxTasks-len(out.Plan.Tasks))
	lastFailure := "verifier did not return a valid result"
	agent := newAgent(OrchestrationVerifier)
	conversationID := newRunID()
	out.Conversations++
	for attempt := 1; attempt <= in.ProtocolAttempts; attempt++ {
		runInput := in.Run
		runInput.UserPrompt = prompt
		runInput.TaskProfile = entity.TaskProfileAgentReadonly
		runInput.OrchestrationConversation = conversationID
		runInput.OrchestrationRole = string(OrchestrationVerifier)
		runInput.OrchestrationAttempt = attempt
		runInput.PlanRevision = out.Replans
		runOutput, err := uc.Execute(ctx, agent, runInput)
		recordAgentRun(out, AgentRunRecord{ConversationID: conversationID, Role: OrchestrationVerifier, Attempt: attempt, Output: runOutput})
		if err != nil {
			if ctx.Err() != nil {
				return VerificationResult{}, false, "", fmt.Errorf("plan execution: verifier attempt %d: %w", attempt, err)
			}
			lastFailure = fmt.Sprintf("verifier request failed: %v", err)
			prompt = verifierRetryPrompt(lastFailure)
			continue
		}
		var result VerificationResult
		parseErr := decodeTaggedJSON(runOutput.FinalAnswer, "VERIFICATION_RESULT:", &result)
		if runOutput.StopReason == RunStopFinalAnswer && parseErr == nil {
			parseErr = validateVerificationResult(result, out.Plan, out.TaskResults, in.MaxTasks)
			if parseErr == nil {
				if strings.EqualFold(result.Status, "pass") {
					return result, true, "", nil
				}
				lastFailure = strings.TrimSpace(result.Summary + ": " + strings.Join(result.Failures, "; "))
				if lastFailure == ": " {
					lastFailure = "verification failed"
				}
				return result, false, lastFailure, nil
			}
		}
		if parseErr != nil {
			lastFailure = parseErr.Error()
		} else {
			lastFailure = "verifier stopped with " + string(runOutput.StopReason)
		}
		prompt = verifierRetryPrompt(lastFailure)
	}
	return VerificationResult{}, false, lastFailure, nil
}

func plannerPrompt(original string, maxTasks int) string {
	return fmt.Sprintf(`You are the planning stage of a plan-driven general agent.
Inspect the workspace with read-only tools when needed. Do not modify files.
Use the fewest cohesive work packages that can each be completed in one conversation.
Do not create separate tasks for planning, trivial edits, or verification that the final verifier already performs.
Each task will run in a fresh conversation and must have observable acceptance criteria.
Use dependencies to form a DAG. Keep the plan at or below %d tasks.

Original task:
%s

Return only one JSON object with this shape:
{"goal":"...","tasks":[{"id":"task-1","title":"...","objective":"...","dependencies":[],"acceptance_criteria":["..."]}]}`, maxTasks, original)
}

func plannerRetryPrompt(failure string) string {
	return fmt.Sprintf("Your previous plan was rejected: %s\nContinue this conversation and return corrected JSON only.", failure)
}

func workerPrompt(original string, plan ExecutionPlan, task PlannedTask, results map[string]TaskResult, handoff string) string {
	planJSON, _ := json.Marshal(plan)
	taskJSON, _ := json.Marshal(task)
	deps := dependencyResults(task.Dependencies, results)
	return fmt.Sprintf(`You are executing one work package from a persisted task graph in a fresh conversation.
Inspect the current workspace first. Complete only the assigned objective plus necessary integration work.
Use tools to implement and verify the acceptance criteria. Preserve correct work from earlier tasks.

Original task:
%s

Execution plan:
%s

Assigned task:
%s

Completed dependency results:
%s

Previous attempt handoff:
%s

Finish with exactly one line beginning TASK_RESULT: followed by compact JSON:
TASK_RESULT: {"status":"complete|incomplete|blocked","summary":"...","evidence":["commands or artifacts"],"remaining":["..."]}`,
		original, planJSON, taskJSON, deps, handoff)
}

func verifierPrompt(original string, plan ExecutionPlan, results map[string]TaskResult, revision, remainingCapacity int) string {
	planJSON, _ := json.Marshal(plan)
	resultsJSON, _ := json.Marshal(results)
	return fmt.Sprintf(`You are an independent verifier for a plan-driven general agent.
Inspect the actual workspace and run relevant validation commands. Do not trust worker claims without evidence.
Judge the complete original objective, integration between tasks, and every acceptance criterion.
If validation fails, propose only concrete repair tasks. Repair task IDs must be new. Dependencies may reference newly proposed repair tasks or existing tasks whose worker result status is complete.
At most %d repair tasks may be proposed in this revision.

Original task:
%s

Plan revision: %d
Plan:
%s

Worker results:
%s

Finish with exactly one line beginning VERIFICATION_RESULT: followed by compact JSON:
VERIFICATION_RESULT: {"status":"pass|fail","summary":"...","evidence":["..."],"failures":["..."],"repair_tasks":[],"final_answer":"user-facing result when status is pass"}`,
		remainingCapacity, original, revision, planJSON, resultsJSON)
}

func verifierRetryPrompt(failure string) string {
	return fmt.Sprintf("Your previous verifier response was rejected: %s\nContinue this conversation and return the required tagged JSON result only.", failure)
}

func workerRetryPrompt(handoff string) string {
	return fmt.Sprintf("Continue the same assigned task conversation. Correct the previous attempt using this handoff:\n%s\nFinish with the required TASK_RESULT line.",
		truncateOrchestrationText(handoff))
}

func taskAttemptHandoff(output RunOutput, parseErr error) string {
	var handoff strings.Builder
	fmt.Fprintf(&handoff, "Previous attempt stopped with %s", output.StopReason)
	if output.ProviderFinishReason != "" {
		fmt.Fprintf(&handoff, " (%s)", output.ProviderFinishReason)
	}
	handoff.WriteString(".\n")
	if parseErr != nil {
		handoff.WriteString("Structured result error: ")
		handoff.WriteString(parseErr.Error())
		handoff.WriteByte('\n')
	}
	handoff.WriteString(truncateOrchestrationText(output.FinalAnswer))
	return handoff.String()
}

func dependenciesComplete(dependencies []string, results map[string]TaskResult) bool {
	for _, dependency := range dependencies {
		result, ok := results[dependency]
		if !ok || !strings.EqualFold(result.Status, "complete") {
			return false
		}
	}
	return true
}

func dependencyResults(dependencies []string, results map[string]TaskResult) string {
	selected := make(map[string]TaskResult, len(dependencies))
	for _, dependency := range dependencies {
		if result, ok := results[dependency]; ok {
			selected[dependency] = result
		}
	}
	encoded, _ := json.Marshal(selected)
	return truncateOrchestrationText(string(encoded))
}

func validateExecutionPlan(plan ExecutionPlan, maxTasks int) error {
	if len(plan.Tasks) == 0 {
		return errors.New("plan must contain at least one task")
	}
	if len(plan.Tasks) > maxTasks {
		return fmt.Errorf("plan contains %d tasks, limit is %d", len(plan.Tasks), maxTasks)
	}
	byID := make(map[string]PlannedTask, len(plan.Tasks))
	for _, task := range plan.Tasks {
		if !taskIDPattern.MatchString(task.ID) {
			return fmt.Errorf("invalid task id %q", task.ID)
		}
		if _, exists := byID[task.ID]; exists {
			return fmt.Errorf("duplicate task id %q", task.ID)
		}
		if strings.TrimSpace(task.Objective) == "" || len(task.AcceptanceCriteria) == 0 {
			return fmt.Errorf("task %s needs an objective and acceptance criteria", task.ID)
		}
		byID[task.ID] = task
	}
	for _, task := range plan.Tasks {
		for _, dependency := range task.Dependencies {
			if dependency == task.ID {
				return fmt.Errorf("task %s depends on itself", task.ID)
			}
			if _, exists := byID[dependency]; !exists {
				return fmt.Errorf("task %s has missing dependency %s", task.ID, dependency)
			}
		}
	}
	visiting := make(map[string]bool)
	visited := make(map[string]bool)
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("task graph contains a cycle at %s", id)
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, dependency := range byID[id].Dependencies {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		visiting[id] = false
		visited[id] = true
		return nil
	}
	for id := range byID {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func validateVerificationResult(
	result VerificationResult,
	plan ExecutionPlan,
	results map[string]TaskResult,
	maxTasks int,
) error {
	switch {
	case strings.EqualFold(result.Status, "pass"):
		if len(result.Failures) > 0 || len(result.RepairTasks) > 0 {
			return errors.New("passing verification cannot contain failures or repair tasks")
		}
		return nil
	case !strings.EqualFold(result.Status, "fail"):
		return fmt.Errorf("invalid verification status %q", result.Status)
	}

	candidate := plan
	candidate.Tasks = append(append([]PlannedTask{}, plan.Tasks...), result.RepairTasks...)
	if err := validateExecutionPlan(candidate, maxTasks); err != nil {
		return err
	}
	repairIDs := make(map[string]bool, len(result.RepairTasks))
	for _, task := range result.RepairTasks {
		repairIDs[task.ID] = true
	}
	for _, task := range result.RepairTasks {
		for _, dependency := range task.Dependencies {
			if repairIDs[dependency] {
				continue
			}
			if prior, ok := results[dependency]; !ok || !strings.EqualFold(prior.Status, "complete") {
				return fmt.Errorf("repair task %s depends on incomplete task %s", task.ID, dependency)
			}
		}
	}
	return nil
}

func decodeTaggedJSON(text, tag string, target any) error {
	index := strings.LastIndex(text, tag)
	if index < 0 {
		return fmt.Errorf("missing %s result", strings.TrimSuffix(tag, ":"))
	}
	return decodeJSONDocument(text[index+len(tag):], target)
}

func decodeJSONDocument(text string, target any) error {
	text = strings.TrimSpace(text)
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start < 0 || end < start {
		return errors.New("JSON object not found")
	}
	decoder := json.NewDecoder(strings.NewReader(text[start : end+1]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

func recordAgentRun(out *PlanExecutionOutput, record AgentRunRecord) {
	out.Runs = append(out.Runs, record)
	addOrchestrationUsage(&out.Usage, record.Output.Usage)
	out.Compactions += record.Output.Compactions
}

func addOrchestrationUsage(total *entity.TokenUsage, usage entity.TokenUsage) {
	total.Input += usage.Input
	total.Output += usage.Output
	total.Total += usage.Total
	total.Reasoning += usage.Reasoning
	total.CacheRead += usage.CacheRead
	total.CacheWrite += usage.CacheWrite
}

func truncateOrchestrationText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= orchestrationHandoffLimit {
		return value
	}
	return "[Earlier content truncated]\n" + value[len(value)-orchestrationHandoffLimit:]
}
