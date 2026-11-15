# tekton

[Tekton](https://tekton.dev)'s own MCP server, [tektoncd/mcp-server](https://github.com/tektoncd/mcp-server), packaged as an image. Upstream publishes no releases or images, so this folder pins an upstream commit and builds its command. There is no code of ours here.

## Tools

Upstream's tools, against Tekton Pipelines `v1` objects:

| Tools | What they do |
|---|---|
| `list_pipelineruns`, `list_taskruns`, `list_pipelines`, `list_tasks`, `list_stepactions` | List objects in one namespace or all of them, by label selector. |
| `get_pipelinerun`, `get_taskrun`, `get_pipeline`, `get_task` | Read one object. |
| `get_taskrun_logs` | Read the logs of every container in a TaskRun's pod. |
| `start_pipeline`, `start_task`, `restart_pipelinerun`, `restart_taskrun` | Create a PipelineRun or TaskRun. |
| `create_*`, `update_*`, `patch_pipeline`, `delete_*`, `delete_all_pipelineruns` | Change Tekton objects. |
| `list_artifacthub_*`, `install_artifacthub_*`, `trigger_artifacthub_*` | Find, install and run catalog Tasks and Pipelines from Artifact Hub. |

It also serves each object as an MCP resource (`tekton://pipelinerun/{namespace}/{name}` and so on).

## Settings

It talks to the Kubernetes API with the standard client configuration: `KUBECONFIG`, else the pod's service account. What it can do is exactly what that identity's RBAC allows. It watches Tekton objects through cluster-wide informers, so the identity needs list and watch on them in every namespace (or at least wherever they exist), and get on pods and `pods/log` to read logs.

| Flag | Default | Meaning |
|---|---|---|
| `-transport` | `http` | `http` (Streamable HTTP) or `stdio`. |
| `-address` | `:8080` | Where to serve MCP over HTTP. |

## Differences from the server contract

Upstream's code, so it doesn't follow `docs/server-contract.md` where that would mean changing it:

- No `/healthz`. Probe the TCP port instead.
- Logs are text, not JSON.
- MCP is served on every path, including `/mcp`.
- No tool-list golden file: the server can't start without a Kubernetes API.

## Updating

Renovate bumps the upstream pseudo-version in `go.mod`. Release a new version of this server for it, noting the upstream commit in `CHANGELOG.md`.
