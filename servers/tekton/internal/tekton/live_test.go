//go:build live

package tekton

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/mcp-servers/internal/mcpserver"
	"github.com/solidDoWant/mcp-servers/internal/mcptest"
)

// The live test needs a cluster with Tekton Pipelines: the kubeconfig in
// KUBECONFIG (or ~/.kube/config) and a namespace with at least one
// PipelineRun in TEKTON_LIVE_NAMESPACE. CI has neither, so there it skips.
func TestLiveReadsTheNewestRunAndItsLogs(t *testing.T) {
	ns := os.Getenv("TEKTON_LIVE_NAMESPACE")
	if ns == "" {
		t.Skip("TEKTON_LIVE_NAMESPACE is not set")
	}

	cfg, err := LoadConfig("live-test")
	require.NoError(t, err)

	client, err := NewClient(cfg)
	require.NoError(t, err)

	session := mcptest.Connect(t, mcpserver.Config{Name: "tekton", Version: "live-test", Instructions: Instructions, Register: Tools(client)})

	var runs ListRunsOutput
	mcptest.Structured(t, mcptest.CallTool(t, session, "list_pipelineruns", map[string]any{"namespace": ns, "limit": 1}), &runs)
	require.NotEmpty(t, runs.Runs, "namespace %s has no PipelineRuns", ns)

	var run GetRunOutput
	mcptest.Structured(t, mcptest.CallTool(t, session, "get_pipelinerun", map[string]any{"namespace": ns, "name": runs.Runs[0].Name}), &run)
	require.NotEmpty(t, run.Tasks)

	var tr GetTaskRunOutput
	mcptest.Structured(t, mcptest.CallTool(t, session, "get_taskrun", map[string]any{"namespace": ns, "name": run.Tasks[0].TaskRun}), &tr)
	assert.NotEmpty(t, tr.Steps)

	// The pod may be gone already; that is a tool error that says so.
	res := mcptest.CallTool(t, session, "get_logs", map[string]any{"namespace": ns, "taskrun": run.Tasks[0].TaskRun, "tail_lines": 20})
	if res.IsError {
		assert.Contains(t, mcptest.Text(res), "is gone")
	}
}
