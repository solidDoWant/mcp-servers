package tekton

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Instructions is sent to clients with the tool list.
const Instructions = `Read Tekton Pipelines CI runs and their logs. Read-only.
Typical flow for "why did the build fail": list_pipelineruns in the project's namespace, get_pipelinerun for the run (each task's TaskRun and failed steps), then get_logs for a failed step.
Calls run with this server's Kubernetes identity, which may only see some namespaces; a "denied" error means no access, not that nothing exists.`

const (
	defaultRunLimit     = 20
	maxRunLimit         = 100
	maxTaskRuns         = 50
	defaultTailLines    = 200
	maxTailLines        = 5000
	maxLogBytes         = 64 << 10
	maxMessage          = 2000
	maxResultValue      = 2000
	maxResults          = 50
	listRunsDescription = "List a namespace's Tekton PipelineRuns, newest first: status, timing, and for runs started by Pipelines-as-Code the Git event behind them (repository, branch, commit, pull request, sender). Returns names to pass to get_pipelinerun."
	getRunDescription   = "Get one PipelineRun: its status and each of its tasks' TaskRun, with the steps that failed. Use the TaskRun names with get_taskrun and get_logs."
	getTaskDescription  = "Get one TaskRun: its status, its pod, and each step's and sidecar's state, exit code and timing, plus its results."
	getLogsDescription  = "Read the end of one step's (or sidecar's) log from a TaskRun's pod. Without a step it reads the first failed step, else the last step. Logs are gone once the pod is deleted."
)

// Status values in tool output and in list_pipelineruns' filter.
const (
	statusRunning   = "running"
	statusSucceeded = "succeeded"
	statusFailed    = "failed"
	statusCancelled = "cancelled"
	statusPending   = "pending"
)

var statusFilters = []string{statusRunning, statusSucceeded, statusFailed, statusCancelled, statusPending}

// Tools registers the server's tools on s.
func Tools(c *Client) func(*mcp.Server) {
	return func(s *mcp.Server) {
		mcp.AddTool(s, &mcp.Tool{Name: "list_pipelineruns", Description: listRunsDescription, Annotations: readOnly("List PipelineRuns")}, c.listRunsTool)
		mcp.AddTool(s, &mcp.Tool{Name: "get_pipelinerun", Description: getRunDescription, Annotations: readOnly("Get a PipelineRun")}, c.getRunTool)
		mcp.AddTool(s, &mcp.Tool{Name: "get_taskrun", Description: getTaskDescription, Annotations: readOnly("Get a TaskRun")}, c.getTaskRunTool)
		mcp.AddTool(s, &mcp.Tool{Name: "get_logs", Description: getLogsDescription, Annotations: readOnly("Read a step's log")}, c.getLogsTool)
	}
}

func readOnly(title string) *mcp.ToolAnnotations {
	openWorld := false

	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: &openWorld}
}

// ListRunsInput is list_pipelineruns' input.
type ListRunsInput struct {
	Namespace string `json:"namespace" jsonschema:"the namespace the runs are in"`
	Status    string `json:"status,omitempty" jsonschema:"only runs in this state: running, succeeded, failed, cancelled or pending"`
	Limit     int    `json:"limit,omitempty" jsonschema:"how many runs to return, 1 to 100 (default 20)"`
}

// ListRunsOutput is list_pipelineruns' output.
type ListRunsOutput struct {
	Total int          `json:"total" jsonschema:"how many runs match, before the limit"`
	Runs  []RunSummary `json:"runs"`
}

// RunSummary describes one PipelineRun or TaskRun.
type RunSummary struct {
	Name            string   `json:"name"`
	Pipeline        string   `json:"pipeline,omitempty" jsonschema:"the Pipeline it runs; absent for one defined inline"`
	Status          string   `json:"status" jsonschema:"pending, running, succeeded, failed or cancelled"`
	Reason          string   `json:"reason,omitempty" jsonschema:"Tekton's reason for the status, e.g. Failed or PipelineRunTimeout"`
	Message         string   `json:"message,omitempty"`
	Created         string   `json:"created"`
	Started         string   `json:"started,omitempty"`
	Finished        string   `json:"finished,omitempty"`
	DurationSeconds *float64 `json:"duration_seconds,omitempty" jsonschema:"from start to finish, or to now while running"`
	Trigger         *Trigger `json:"trigger,omitempty" jsonschema:"the Git event behind a run Pipelines-as-Code started"`
}

// Trigger is the Git event Pipelines-as-Code started a run for.
type Trigger struct {
	Event        string `json:"event,omitempty" jsonschema:"e.g. pull_request or push"`
	Repository   string `json:"repository,omitempty"`
	Branch       string `json:"branch,omitempty" jsonschema:"the target branch"`
	SourceBranch string `json:"source_branch,omitempty" jsonschema:"a pull request's head branch"`
	SHA          string `json:"sha,omitempty"`
	CommitTitle  string `json:"commit_title,omitempty"`
	PullRequest  string `json:"pull_request,omitempty"`
	Sender       string `json:"sender,omitempty"`
	URL          string `json:"url,omitempty" jsonschema:"the commit on the Git host"`
}

func (c *Client) listRunsTool(ctx context.Context, _ *mcp.CallToolRequest, in ListRunsInput) (*mcp.CallToolResult, ListRunsOutput, error) {
	if err := checkNamespace(in.Namespace); err != nil {
		return nil, ListRunsOutput{}, err
	}

	if in.Status != "" && !slices.Contains(statusFilters, in.Status) {
		return nil, ListRunsOutput{}, fmt.Errorf("status must be one of %v, not %q", statusFilters, in.Status)
	}

	limit := in.Limit
	if limit == 0 {
		limit = defaultRunLimit
	}

	if limit < 1 || limit > maxRunLimit {
		return nil, ListRunsOutput{}, fmt.Errorf("limit must be between 1 and %d, not %d", maxRunLimit, in.Limit)
	}

	runs, err := c.ListPipelineRuns(ctx, in.Namespace)
	if err != nil {
		return nil, ListRunsOutput{}, err
	}

	slices.SortFunc(runs, func(a, b PipelineRun) int {
		return b.Metadata.CreationTimestamp.Compare(a.Metadata.CreationTimestamp.Time)
	})

	out := ListRunsOutput{Runs: []RunSummary{}}

	for _, run := range runs {
		summary := pipelineRunSummary(&run, time.Now())
		if in.Status != "" && summary.Status != in.Status {
			continue
		}

		out.Total++
		if len(out.Runs) < limit {
			out.Runs = append(out.Runs, summary)
		}
	}

	return nil, out, nil
}

// RunInput names one run.
type RunInput struct {
	Namespace string `json:"namespace" jsonschema:"the namespace the run is in"`
	Name      string `json:"name" jsonschema:"the run's name"`
}

// GetRunOutput is get_pipelinerun's output.
type GetRunOutput struct {
	RunSummary

	Tasks []TaskSummary `json:"tasks" jsonschema:"one per task the run started, in the order Tekton lists them"`
}

// TaskSummary is one task of a PipelineRun.
type TaskSummary struct {
	Task        string   `json:"task" jsonschema:"the task's name in the pipeline"`
	TaskRun     string   `json:"taskrun" jsonschema:"the TaskRun's name, for get_taskrun and get_logs"`
	Status      string   `json:"status"`
	Reason      string   `json:"reason,omitempty"`
	Message     string   `json:"message,omitempty"`
	FailedSteps []string `json:"failed_steps,omitempty"`
	Error       string   `json:"error,omitempty" jsonschema:"why the TaskRun couldn't be read"`
}

func (c *Client) getRunTool(ctx context.Context, _ *mcp.CallToolRequest, in RunInput) (*mcp.CallToolResult, GetRunOutput, error) {
	if err := checkRun(in); err != nil {
		return nil, GetRunOutput{}, err
	}

	run, err := c.PipelineRun(ctx, in.Namespace, in.Name)
	if err != nil {
		return nil, GetRunOutput{}, err
	}

	now := time.Now()
	out := GetRunOutput{RunSummary: pipelineRunSummary(run, now), Tasks: []TaskSummary{}}

	for _, child := range run.Status.ChildReferences {
		if child.Kind != "" && child.Kind != "TaskRun" {
			continue
		}

		if len(out.Tasks) == maxTaskRuns {
			out.Message = joinNote(out.Message, fmt.Sprintf("only the first %d tasks are listed", maxTaskRuns))

			break
		}

		task := TaskSummary{Task: child.PipelineTaskName, TaskRun: child.Name}

		tr, err := c.TaskRun(ctx, in.Namespace, child.Name)
		if err != nil {
			task.Status = "unknown"
			task.Error = err.Error()
		} else {
			s := taskRunSummary(tr, now)
			task.Status, task.Reason, task.Message = s.Status, s.Reason, s.Message
			task.FailedSteps = failedSteps(tr)
		}

		out.Tasks = append(out.Tasks, task)
	}

	return nil, out, nil
}

// GetTaskRunOutput is get_taskrun's output.
type GetTaskRunOutput struct {
	RunSummary

	Task     string        `json:"task,omitempty" jsonschema:"the Task it runs; absent for one defined inline"`
	Pod      string        `json:"pod,omitempty"`
	Steps    []StepState   `json:"steps"`
	Sidecars []StepState   `json:"sidecars,omitempty"`
	Results  []ResultValue `json:"results,omitempty"`
}

// StepState is one step's or sidecar's container state.
type StepState struct {
	Name     string `json:"name"`
	State    string `json:"state" jsonschema:"waiting, running or terminated"`
	ExitCode *int32 `json:"exit_code,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
	Started  string `json:"started,omitempty"`
	Finished string `json:"finished,omitempty"`
}

// ResultValue is one TaskRun result.
type ResultValue struct {
	Name  string `json:"name"`
	Value string `json:"value" jsonschema:"the value; arrays and objects as JSON"`
}

func (c *Client) getTaskRunTool(ctx context.Context, _ *mcp.CallToolRequest, in RunInput) (*mcp.CallToolResult, GetTaskRunOutput, error) {
	if err := checkRun(in); err != nil {
		return nil, GetTaskRunOutput{}, err
	}

	tr, err := c.TaskRun(ctx, in.Namespace, in.Name)
	if err != nil {
		return nil, GetTaskRunOutput{}, err
	}

	out := GetTaskRunOutput{
		RunSummary: taskRunSummary(tr, time.Now()),
		Pod:        tr.Status.PodName,
		Steps:      []StepState{},
	}

	if tr.Spec.TaskRef != nil {
		out.Task = tr.Spec.TaskRef.Name
	}

	for _, s := range tr.Status.Steps {
		out.Steps = append(out.Steps, stepState(s))
	}

	for _, s := range tr.Status.Sidecars {
		out.Sidecars = append(out.Sidecars, stepState(s))
	}

	for i, r := range tr.Status.Results {
		if i == maxResults {
			out.Message = joinNote(out.Message, fmt.Sprintf("only the first %d results are listed", maxResults))

			break
		}

		out.Results = append(out.Results, ResultValue{Name: r.Name, Value: resultString(r.Value)})
	}

	return nil, out, nil
}

// LogsInput is get_logs' input.
type LogsInput struct {
	Namespace string `json:"namespace" jsonschema:"the namespace the TaskRun is in"`
	TaskRun   string `json:"taskrun" jsonschema:"the TaskRun's name"`
	Step      string `json:"step,omitempty" jsonschema:"the step's name; default: the first failed step, else the last step"`
	Sidecar   string `json:"sidecar,omitempty" jsonschema:"a sidecar's name, to read its log instead of a step's"`
	TailLines int    `json:"tail_lines,omitempty" jsonschema:"how many lines from the end, 1 to 5000 (default 200)"`
}

// LogsOutput is get_logs' output.
type LogsOutput struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Step      string `json:"step,omitempty"`
	Sidecar   string `json:"sidecar,omitempty"`
	Lines     int    `json:"lines" jsonschema:"how many lines were returned"`
	Truncated bool   `json:"truncated" jsonschema:"the requested lines were over 64 KiB, so only their end is shown, from a line boundary"`
	Log       string `json:"log"`
}

func (c *Client) getLogsTool(ctx context.Context, _ *mcp.CallToolRequest, in LogsInput) (*mcp.CallToolResult, LogsOutput, error) {
	if in.TaskRun == "" {
		return nil, LogsOutput{}, errors.New("taskrun is empty")
	}

	if err := checkRun(RunInput{Namespace: in.Namespace, Name: in.TaskRun}); err != nil {
		return nil, LogsOutput{}, err
	}

	if in.Step != "" && in.Sidecar != "" {
		return nil, LogsOutput{}, errors.New("give a step or a sidecar, not both")
	}

	tail := in.TailLines
	if tail == 0 {
		tail = defaultTailLines
	}

	if tail < 1 || tail > maxTailLines {
		return nil, LogsOutput{}, fmt.Errorf("tail_lines must be between 1 and %d, not %d", maxTailLines, in.TailLines)
	}

	tr, err := c.TaskRun(ctx, in.Namespace, in.TaskRun)
	if err != nil {
		return nil, LogsOutput{}, err
	}

	if tr.Status.PodName == "" {
		return nil, LogsOutput{}, fmt.Errorf("TaskRun %s has no pod yet, so there are no logs", in.TaskRun)
	}

	out := LogsOutput{Pod: tr.Status.PodName}

	switch {
	case in.Sidecar != "":
		s, err := findContainer(tr.Status.Sidecars, in.Sidecar, "sidecar")
		if err != nil {
			return nil, LogsOutput{}, err
		}

		out.Container, out.Sidecar = s.Container, s.Name
	default:
		s, err := pickStep(tr, in.Step)
		if err != nil {
			return nil, LogsOutput{}, err
		}

		out.Container, out.Step = s.Container, s.Name
	}

	log, truncated, err := c.Logs(ctx, in.Namespace, tr.Status.PodName, out.Container, int64(tail), maxLogBytes)
	if err != nil {
		return nil, LogsOutput{}, err
	}

	out.Truncated = truncated
	out.Log = strings.ToValidUTF8(log, "�")
	out.Lines = strings.Count(out.Log, "\n")

	if out.Log != "" && !strings.HasSuffix(out.Log, "\n") {
		out.Lines++
	}

	return nil, out, nil
}

// pickStep finds the named step, or the first failed one, or the last one.
func pickStep(tr *TaskRun, name string) (ContainerState, error) {
	if name != "" {
		return findContainer(tr.Status.Steps, name, "step")
	}

	if len(tr.Status.Steps) == 0 {
		return ContainerState{}, fmt.Errorf("TaskRun %s reports no steps yet", tr.Metadata.Name)
	}

	for _, s := range tr.Status.Steps {
		if failed(s) {
			return s, nil
		}
	}

	return tr.Status.Steps[len(tr.Status.Steps)-1], nil
}

func findContainer(states []ContainerState, name, kind string) (ContainerState, error) {
	names := make([]string, 0, len(states))

	for _, s := range states {
		if s.Name == name {
			return s, nil
		}

		names = append(names, s.Name)
	}

	return ContainerState{}, fmt.Errorf("no %s %q; this TaskRun's %ss are %v", kind, name, kind, names)
}

func pipelineRunSummary(run *PipelineRun, now time.Time) RunSummary {
	s := summary(run.Metadata, run.Status.Conditions, run.Status.StartTime, run.Status.CompletionTime, now)

	if run.Spec.PipelineRef != nil {
		s.Pipeline = run.Spec.PipelineRef.Name
	}

	s.Trigger = trigger(run.Metadata)

	return s
}

func taskRunSummary(tr *TaskRun, now time.Time) RunSummary {
	return summary(tr.Metadata, tr.Status.Conditions, tr.Status.StartTime, tr.Status.CompletionTime, now)
}

func summary(meta metav1.ObjectMeta, conditions []Condition, start, finish *metav1.Time, now time.Time) RunSummary {
	s := RunSummary{Name: meta.Name, Created: timestamp(&meta.CreationTimestamp), Started: timestamp(start), Finished: timestamp(finish)}
	s.Status, s.Reason, s.Message = status(conditions)
	s.Message = cut(s.Message, maxMessage)

	if start != nil {
		end := now
		if finish != nil {
			end = finish.Time
		}

		d := end.Sub(start.Time).Seconds()
		s.DurationSeconds = &d
	}

	return s
}

// status maps Tekton's Succeeded condition to one word.
func status(conditions []Condition) (string, string, string) {
	for _, c := range conditions {
		if c.Type != "Succeeded" {
			continue
		}

		switch {
		case c.Status == "True":
			return statusSucceeded, c.Reason, c.Message
		case c.Status == "False" && strings.Contains(c.Reason, "Cancelled"):
			return statusCancelled, c.Reason, c.Message
		case c.Status == "False":
			return statusFailed, c.Reason, c.Message
		case c.Reason == "Pending" || c.Reason == "PipelineRunPending":
			return statusPending, c.Reason, c.Message
		default:
			return statusRunning, c.Reason, c.Message
		}
	}

	return statusPending, "", ""
}

// The labels and annotations Pipelines-as-Code puts on the runs it starts.
const pacPrefix = "pipelinesascode.tekton.dev/"

func trigger(meta metav1.ObjectMeta) *Trigger {
	get := func(key string) string {
		if v := meta.Annotations[pacPrefix+key]; v != "" {
			return v
		}

		return meta.Labels[pacPrefix+key]
	}

	t := Trigger{
		Event:        get("event-type"),
		Repository:   get("repository"),
		Branch:       get("branch"),
		SourceBranch: get("source-branch"),
		SHA:          get("sha"),
		CommitTitle:  cut(get("sha-title"), 200),
		PullRequest:  get("pull-request"),
		Sender:       get("sender"),
		URL:          get("sha-url"),
	}

	if t == (Trigger{}) {
		return nil
	}

	return &t
}

func stepState(s ContainerState) StepState {
	out := StepState{Name: s.Name}

	switch {
	case s.Terminated != nil:
		code := s.Terminated.ExitCode
		out.State, out.ExitCode, out.Reason = "terminated", &code, s.Terminated.Reason
		out.Message = cut(s.Terminated.Message, maxMessage)
		out.Started, out.Finished = timestamp(&s.Terminated.StartedAt), timestamp(&s.Terminated.FinishedAt)
	case s.Running != nil:
		out.State, out.Started = "running", timestamp(&s.Running.StartedAt)
	case s.Waiting != nil:
		out.State, out.Reason, out.Message = "waiting", s.Waiting.Reason, cut(s.Waiting.Message, maxMessage)
	default:
		out.State = "waiting"
	}

	return out
}

func failed(s ContainerState) bool {
	return s.Terminated != nil && s.Terminated.ExitCode != 0
}

func failedSteps(tr *TaskRun) []string {
	var names []string

	for _, s := range tr.Status.Steps {
		if failed(s) {
			names = append(names, s.Name)
		}
	}

	return names
}

func resultString(v any) string {
	if s, ok := v.(string); ok {
		return cut(s, maxResultValue)
	}

	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}

	return cut(string(b), maxResultValue)
}

func timestamp(t *metav1.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}

	return t.UTC().Format(time.RFC3339)
}

// cut shortens s to at most n runes, saying so.
func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}

	return string([]rune(s)[:n]) + fmt.Sprintf(" [cut at %d characters]", n)
}

func joinNote(message, note string) string {
	if message == "" {
		return note
	}

	return message + " (" + note + ")"
}

func checkNamespace(ns string) error {
	if ns == "" {
		return errors.New("namespace is empty")
	}

	if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
		return fmt.Errorf("%q is not a valid namespace name: %s", ns, strings.Join(errs, "; "))
	}

	return nil
}

func checkRun(in RunInput) error {
	if err := checkNamespace(in.Namespace); err != nil {
		return err
	}

	if in.Name == "" {
		return errors.New("name is empty")
	}

	if errs := validation.IsDNS1123Subdomain(in.Name); len(errs) > 0 {
		return fmt.Errorf("%q is not a valid run name: %s", in.Name, strings.Join(errs, "; "))
	}

	return nil
}
