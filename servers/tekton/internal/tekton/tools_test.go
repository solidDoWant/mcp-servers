package tekton

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/mcp-servers/internal/mcptest"
)

func TestToolListMatchesGolden(t *testing.T) {
	mcptest.AssertToolsGolden(t, connect(t, newFakeAPI(t)), "testdata/tools.golden.json")
}

func TestListPipelineRunsReturnsNewestFirstWithTheirTrigger(t *testing.T) {
	api := newFakeAPI(t)
	api.onFixture(pipelineRunsPath(""), "pipelineruns.json")

	res := mcptest.CallTool(t, connect(t, api), "list_pipelineruns", map[string]any{"namespace": namespace})

	var out ListRunsOutput
	mcptest.Structured(t, res, &out)

	assert.Equal(t, 2, out.Total)
	require.Len(t, out.Runs, 2)

	failed := out.Runs[0]
	assert.Equal(t, "fixture-build", failed.Name)
	assert.Equal(t, "failed", failed.Status)
	assert.Equal(t, "Failed", failed.Reason)
	assert.Equal(t, "Tasks Completed: 2 (Failed: 1, Cancelled 0), Skipped: 0", failed.Message)
	assert.Equal(t, "2026-11-15T06:25:41Z", failed.Started)
	assert.Equal(t, "2026-11-15T06:25:50Z", failed.Finished)
	require.NotNil(t, failed.DurationSeconds)
	assert.InDelta(t, 9, *failed.DurationSeconds, 0.001)
	assert.Equal(t, &Trigger{Event: "pull_request"}, failed.Trigger)

	smoke := out.Runs[1]
	assert.Equal(t, "smoke-pull-request", smoke.Name)
	assert.Equal(t, "succeeded", smoke.Status)
	assert.Equal(t, &Trigger{
		Event:        "pull_request",
		Repository:   "infra-mk3",
		Branch:       "master",
		SourceBranch: "test/tekton-smoke",
		SHA:          "b45236fea7e018069d206642f27fe86e66f252bd",
		CommitTitle:  "test: Pipelines-as-Code smoke test (do not merge)",
		PullRequest:  "805",
		Sender:       "solidDoWant",
		URL:          "https://github.com/solidDoWant/infra-mk3/commit/b45236fea7e018069d206642f27fe86e66f252bd",
	}, smoke.Trigger)
}

func TestListPipelineRunsFiltersByStatusAndCountsBeforeTheLimit(t *testing.T) {
	api := newFakeAPI(t)
	api.onFixture(pipelineRunsPath(""), "pipelineruns.json")
	session := connect(t, api)

	t.Run("a status filter keeps only matching runs", func(t *testing.T) {
		var out ListRunsOutput
		mcptest.Structured(t, mcptest.CallTool(t, session, "list_pipelineruns", map[string]any{"namespace": namespace, "status": "succeeded"}), &out)

		assert.Equal(t, 1, out.Total)
		require.Len(t, out.Runs, 1)
		assert.Equal(t, "smoke-pull-request", out.Runs[0].Name)
	})

	t.Run("the total counts runs past the limit", func(t *testing.T) {
		var out ListRunsOutput
		mcptest.Structured(t, mcptest.CallTool(t, session, "list_pipelineruns", map[string]any{"namespace": namespace, "limit": 1}), &out)

		assert.Equal(t, 2, out.Total)
		require.Len(t, out.Runs, 1)
		assert.Equal(t, "fixture-build", out.Runs[0].Name)
	})
}

func TestBadInputIsRefusedWithoutCallingTheAPI(t *testing.T) {
	api := newFakeAPI(t)
	session := connect(t, api)

	cases := []struct {
		name, tool, want string
		args             map[string]any
	}{
		{"an empty namespace", "list_pipelineruns", "namespace is empty", map[string]any{"namespace": ""}},
		{"a namespace that isn't a DNS label", "list_pipelineruns", "not a valid namespace name", map[string]any{"namespace": "CI_Infra"}},
		{"an unknown status", "list_pipelineruns", "status must be one of", map[string]any{"namespace": namespace, "status": "broken"}},
		{"a limit over the maximum", "list_pipelineruns", "limit must be between 1 and 100", map[string]any{"namespace": namespace, "limit": 101}},
		{"a run name with a slash", "get_pipelinerun", "not a valid run name", map[string]any{"namespace": namespace, "name": "a/b"}},
		{"an empty TaskRun name", "get_logs", "taskrun is empty", map[string]any{"namespace": namespace, "taskrun": ""}},
		{"both a step and a sidecar", "get_logs", "not both", map[string]any{"namespace": namespace, "taskrun": "x", "step": "a", "sidecar": "b"}},
		{"too many log lines", "get_logs", "tail_lines must be between 1 and 5000", map[string]any{"namespace": namespace, "taskrun": "x", "tail_lines": 5001}},
	}

	for _, tc := range cases {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			res := mcptest.CallTool(t, session, tc.tool, tc.args)

			assert.True(t, res.IsError)
			assert.Contains(t, mcptest.Text(res), tc.want)
		})
	}

	assert.Zero(t, api.requestCount())
}

func TestAPIFailuresAreToolErrorsThatSayWhy(t *testing.T) {
	t.Run("a refusal says the identity was denied and quotes Kubernetes", func(t *testing.T) {
		api := newFakeAPI(t)
		api.on(pipelineRunsPath(""), http.StatusForbidden, forbidden)

		res := mcptest.CallTool(t, connect(t, api), "list_pipelineruns", map[string]any{"namespace": namespace})

		assert.True(t, res.IsError)
		assert.Contains(t, mcptest.Text(res), "Kubernetes denied it to this server's identity")
		assert.Contains(t, mcptest.Text(res), `User "authentik:someone" cannot list resource "pipelineruns"`)
	})

	t.Run("a missing run says it doesn't exist", func(t *testing.T) {
		res := mcptest.CallTool(t, connect(t, newFakeAPI(t)), "get_pipelinerun", map[string]any{"namespace": namespace, "name": "gone"})

		assert.True(t, res.IsError)
		assert.Contains(t, mcptest.Text(res), "could not get PipelineRun gone in namespace ci-infra-mk3: it doesn't exist")
	})

	t.Run("a server error gives its HTTP status", func(t *testing.T) {
		api := newFakeAPI(t)
		api.on(pipelineRunsPath(""), http.StatusInternalServerError, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"etcdserver: request timed out","reason":"InternalError","code":500}`)

		res := mcptest.CallTool(t, connect(t, api), "list_pipelineruns", map[string]any{"namespace": namespace})

		assert.True(t, res.IsError)
		assert.Contains(t, mcptest.Text(res), "Kubernetes returned HTTP 500")
		assert.Contains(t, mcptest.Text(res), "etcdserver: request timed out")
	})
}

func TestGetPipelineRunListsEachTaskWithItsFailedSteps(t *testing.T) {
	api := newFakeAPI(t)
	api.onFixture(pipelineRunsPath("fixture-build"), "pipelinerun-failed.json")
	api.onFixture(taskRunPath("fixture-build-lint"), "taskrun-succeeded.json")
	api.onFixture(taskRunPath("fixture-build-test"), "taskrun-failed.json")

	var out GetRunOutput
	mcptest.Structured(t, mcptest.CallTool(t, connect(t, api), "get_pipelinerun", map[string]any{"namespace": namespace, "name": "fixture-build"}), &out)

	assert.Equal(t, "fixture-build", out.Name)
	assert.Equal(t, "failed", out.Status)
	assert.Equal(t, []TaskSummary{
		{Task: "lint", TaskRun: "fixture-build-lint", Status: "succeeded", Reason: "Succeeded", Message: "All Steps have completed executing"},
		{Task: "test", TaskRun: "fixture-build-test", Status: "failed", Reason: "StepFailed", Message: `"step-unit" exited with code 1: Error`, FailedSteps: []string{"unit"}},
	}, out.Tasks)
}

func TestGetPipelineRunReportsATaskRunItCannotRead(t *testing.T) {
	api := newFakeAPI(t)
	api.onFixture(pipelineRunsPath("fixture-build"), "pipelinerun-failed.json")
	api.onFixture(taskRunPath("fixture-build-lint"), "taskrun-succeeded.json")

	var out GetRunOutput
	mcptest.Structured(t, mcptest.CallTool(t, connect(t, api), "get_pipelinerun", map[string]any{"namespace": namespace, "name": "fixture-build"}), &out)

	require.Len(t, out.Tasks, 2)
	assert.Equal(t, "unknown", out.Tasks[1].Status)
	assert.Contains(t, out.Tasks[1].Error, "it doesn't exist")
}

func TestGetTaskRunReportsEachStepsExitCodeAndTiming(t *testing.T) {
	api := newFakeAPI(t)
	api.onFixture(taskRunPath("fixture-build-test"), "taskrun-failed.json")

	var out GetTaskRunOutput
	mcptest.Structured(t, mcptest.CallTool(t, connect(t, api), "get_taskrun", map[string]any{"namespace": namespace, "name": "fixture-build-test"}), &out)

	zero, one := int32(0), int32(1)

	assert.Equal(t, "failed", out.Status)
	assert.Equal(t, "fixture-build-test-pod", out.Pod)
	assert.Empty(t, out.Task, "an inline task has no Task name")
	assert.Equal(t, []StepState{
		{Name: "setup", State: "terminated", ExitCode: &zero, Reason: "Completed", Started: "2026-11-15T06:25:50Z", Finished: "2026-11-15T06:25:50Z"},
		{Name: "unit", State: "terminated", ExitCode: &one, Reason: "Error", Started: "2026-11-15T06:25:50Z", Finished: "2026-11-15T06:25:50Z"},
	}, out.Steps)
}

func TestGetLogsReadsTheFirstFailedStepByDefault(t *testing.T) {
	api := newFakeAPI(t)
	api.onFixture(taskRunPath("fixture-build-test"), "taskrun-failed.json")
	api.on(logPath("fixture-build-test-pod"), http.StatusOK, fixture(t, "step-log.txt"))

	var out LogsOutput
	mcptest.Structured(t, mcptest.CallTool(t, connect(t, api), "get_logs", map[string]any{"namespace": namespace, "taskrun": "fixture-build-test"}), &out)

	assert.Equal(t, LogsOutput{
		Pod:       "fixture-build-test-pod",
		Container: "step-unit",
		Step:      "unit",
		Lines:     6,
		Log:       fixture(t, "step-log.txt"),
	}, out)

	reqs := api.requestsTo(logPath("fixture-build-test-pod"))
	require.Len(t, reqs, 1)
	assert.Equal(t, "step-unit", reqs[0].query.Get("container"))
	assert.Equal(t, "200", reqs[0].query.Get("tailLines"))
	assert.Equal(t, "mcp-tekton/test (+https://github.com/solidDoWant/mcp-servers)", reqs[0].userAgent)
}

func TestGetLogsReadsANamedStep(t *testing.T) {
	api := newFakeAPI(t)
	api.onFixture(taskRunPath("fixture-build-test"), "taskrun-failed.json")
	api.on(logPath("fixture-build-test-pod"), http.StatusOK, "setup: done\n")

	var out LogsOutput
	mcptest.Structured(t, mcptest.CallTool(t, connect(t, api), "get_logs", map[string]any{"namespace": namespace, "taskrun": "fixture-build-test", "step": "setup", "tail_lines": 50}), &out)

	assert.Equal(t, "step-setup", out.Container)
	assert.Equal(t, "setup: done\n", out.Log)
	assert.Equal(t, "50", api.requestsTo(logPath("fixture-build-test-pod"))[0].query.Get("tailLines"))
}

func TestGetLogsNamesTheStepsWhenTheStepIsUnknown(t *testing.T) {
	api := newFakeAPI(t)
	api.onFixture(taskRunPath("fixture-build-test"), "taskrun-failed.json")

	res := mcptest.CallTool(t, connect(t, api), "get_logs", map[string]any{"namespace": namespace, "taskrun": "fixture-build-test", "step": "build"})

	assert.True(t, res.IsError)
	assert.Contains(t, mcptest.Text(res), `no step "build"; this TaskRun's steps are [setup unit]`)
}

func TestGetLogsSaysTheLogIsGoneWithItsPod(t *testing.T) {
	api := newFakeAPI(t)
	api.onFixture(taskRunPath("fixture-build-test"), "taskrun-failed.json")

	res := mcptest.CallTool(t, connect(t, api), "get_logs", map[string]any{"namespace": namespace, "taskrun": "fixture-build-test"})

	assert.True(t, res.IsError)
	assert.Contains(t, mcptest.Text(res), "pod fixture-build-test-pod or its container step-unit is gone")
}

func TestGetLogsKeepsTheEndOfALongLogFromALineBoundary(t *testing.T) {
	var log strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&log, "%s line %04d\n", strings.Repeat("x", 60), i)
	}

	api := newFakeAPI(t)
	api.onFixture(taskRunPath("fixture-build-test"), "taskrun-failed.json")
	api.on(logPath("fixture-build-test-pod"), http.StatusOK, log.String())

	var out LogsOutput
	mcptest.Structured(t, mcptest.CallTool(t, connect(t, api), "get_logs", map[string]any{"namespace": namespace, "taskrun": "fixture-build-test", "tail_lines": 2000}), &out)

	assert.True(t, out.Truncated)
	assert.LessOrEqual(t, len(out.Log), maxLogBytes)
	assert.True(t, strings.HasPrefix(out.Log, strings.Repeat("x", 60)), "starts at a whole line")
	assert.True(t, strings.HasSuffix(out.Log, " line 1999\n"), "keeps the end")
	assert.Equal(t, strings.Count(out.Log, "\n"), out.Lines)
}
