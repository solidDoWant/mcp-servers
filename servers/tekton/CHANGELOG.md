# tekton changelog

## 0.1.0

First release.

- List a namespace's PipelineRuns, newest first, with an optional status filter and, for Pipelines-as-Code runs, the Git event behind them.
- Read a PipelineRun with each task's TaskRun and failed steps, and a TaskRun with each step's and sidecar's state, exit code and timing.
- Read the end of a step's or sidecar's log, bounded to 5000 lines and 64 KiB.
