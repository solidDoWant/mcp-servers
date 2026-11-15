# tekton

Read [Tekton Pipelines](https://tekton.dev) CI runs and their logs through the Kubernetes API: which runs passed or failed, what triggered them, which steps failed, and the end of a step's log. Read-only.

## Tools

| Tool | What it does |
|---|---|
| `list_pipelineruns` | A namespace's PipelineRuns, newest first, optionally only one status (running, succeeded, failed, cancelled, pending). For runs [Pipelines-as-Code](https://pipelinesascode.com) started, the Git event behind them: repository, branch, commit, pull request and sender. |
| `get_pipelinerun` | One PipelineRun's status and each task's TaskRun, with the steps that failed. |
| `get_taskrun` | One TaskRun's status, pod, and each step's and sidecar's state, exit code and timing, plus its results. |
| `get_logs` | The end of one step's or sidecar's log (200 lines by default, at most 5000 lines and 64 KiB, cut at a line boundary). Without a step, the first failed step, else the last one. |

Everything is read with namespaced requests, so the identity needs no cluster-wide access. Messages and result values are cut at 2000 characters, and a PipelineRun lists at most 50 tasks.

## Settings

| Variable | Default | Meaning |
|---|---|---|
| `MCP_LISTEN_ADDR` | `:8080` | Where to serve MCP (`/mcp`) and the health check (`/healthz`). |
| `KUBECONFIG` | `~/.kube/config`, else the pod's service account | The Kubernetes client configuration, found the way kubectl finds it. |

The server can do exactly what its Kubernetes identity's RBAC allows. It needs `get` and `list` on `pipelineruns.tekton.dev` and `taskruns.tekton.dev`, and `get` on `pods/log`, in the namespaces it should see. A kubeconfig `tokenFile` is read again when it changes, so a token refreshed on disk (for example a short-lived token for the calling user) takes effect without a restart. The server needs network access to the Kubernetes API only.

## Tests

The unit tests replay responses recorded from a real cluster (`testdata/`), with names and hosts made generic. The live test (`make test-live`) needs a cluster: set `TEKTON_LIVE_NAMESPACE` to a namespace with at least one PipelineRun, with a kubeconfig that can read it. It skips without one, as it does in CI.
