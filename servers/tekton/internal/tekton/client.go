// Package tekton reads Tekton Pipelines runs and their logs through the
// Kubernetes API, with whatever identity the server is given.
package tekton

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	pipelineRuns = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "pipelineruns"}
	taskRuns     = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "taskruns"}
)

// maxListed bounds one list request. Tekton's pruners and Pipelines-as-Code
// keep a namespace's run count far below this.
const maxListed = 500

// Client reads Tekton objects and pod logs.
type Client struct {
	dynamic dynamic.Interface
	core    kubernetes.Interface
}

// LoadConfig returns the Kubernetes client configuration the way kubectl
// finds it: the files in KUBECONFIG, else ~/.kube/config, else the pod's
// service account. A kubeconfig's tokenFile is read again as it changes, so
// a token refreshed on disk takes effect without a restart.
func LoadConfig(version string) (*rest.Config, error) {
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load the Kubernetes client configuration: %w", err)
	}

	cfg.UserAgent = userAgent(version)

	return cfg, nil
}

func userAgent(version string) string {
	return "mcp-tekton/" + version + " (+https://github.com/solidDoWant/mcp-servers)"
}

// NewClient returns a client for the cluster cfg points at.
func NewClient(cfg *rest.Config) (*Client, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create the Kubernetes client: %w", err)
	}

	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create the Kubernetes client: %w", err)
	}

	return &Client{dynamic: dyn, core: core}, nil
}

// Condition is a Tekton status condition.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// ChildReference names a TaskRun a PipelineRun started.
type ChildReference struct {
	Kind             string `json:"kind,omitempty"`
	Name             string `json:"name"`
	PipelineTaskName string `json:"pipelineTaskName,omitempty"`
}

// PipelineRun holds the parts of a Tekton PipelineRun this server reports.
type PipelineRun struct {
	Metadata metav1.ObjectMeta `json:"metadata"`
	Spec     struct {
		PipelineRef *struct {
			Name string `json:"name,omitempty"`
		} `json:"pipelineRef,omitempty"`
	} `json:"spec"`
	Status struct {
		Conditions      []Condition      `json:"conditions,omitempty"`
		StartTime       *metav1.Time     `json:"startTime,omitempty"`
		CompletionTime  *metav1.Time     `json:"completionTime,omitempty"`
		ChildReferences []ChildReference `json:"childReferences,omitempty"`
	} `json:"status"`
}

// ContainerState is a step's or sidecar's container state, as Tekton
// reports it.
type ContainerState struct {
	Name                  string `json:"name"`
	Container             string `json:"container"`
	corev1.ContainerState `json:",inline"`
}

// Result is a TaskRun result.
type Result struct {
	Name  string `json:"name"`
	Value any    `json:"value"`
}

// TaskRun holds the parts of a Tekton TaskRun this server reports.
type TaskRun struct {
	Metadata metav1.ObjectMeta `json:"metadata"`
	Spec     struct {
		TaskRef *struct {
			Name string `json:"name,omitempty"`
			Kind string `json:"kind,omitempty"`
		} `json:"taskRef,omitempty"`
	} `json:"spec"`
	Status struct {
		Conditions     []Condition      `json:"conditions,omitempty"`
		StartTime      *metav1.Time     `json:"startTime,omitempty"`
		CompletionTime *metav1.Time     `json:"completionTime,omitempty"`
		PodName        string           `json:"podName,omitempty"`
		Steps          []ContainerState `json:"steps,omitempty"`
		Sidecars       []ContainerState `json:"sidecars,omitempty"`
		Results        []Result         `json:"results,omitempty"`
	} `json:"status"`
}

// ListPipelineRuns returns the namespace's PipelineRuns, in no set order.
func (c *Client) ListPipelineRuns(ctx context.Context, namespace string) ([]PipelineRun, error) {
	list, err := c.dynamic.Resource(pipelineRuns).Namespace(namespace).List(ctx, metav1.ListOptions{Limit: maxListed})
	if err != nil {
		return nil, apiError(err, "list PipelineRuns in namespace "+namespace)
	}

	runs := make([]PipelineRun, 0, len(list.Items))

	for _, item := range list.Items {
		var run PipelineRun
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &run); err != nil {
			return nil, fmt.Errorf("read PipelineRun %s: %w", item.GetName(), err)
		}

		runs = append(runs, run)
	}

	return runs, nil
}

// PipelineRun returns one PipelineRun.
func (c *Client) PipelineRun(ctx context.Context, namespace, name string) (*PipelineRun, error) {
	var run PipelineRun
	if err := c.get(ctx, pipelineRuns, namespace, name, "PipelineRun", &run); err != nil {
		return nil, err
	}

	return &run, nil
}

// TaskRun returns one TaskRun.
func (c *Client) TaskRun(ctx context.Context, namespace, name string) (*TaskRun, error) {
	var run TaskRun
	if err := c.get(ctx, taskRuns, namespace, name, "TaskRun", &run); err != nil {
		return nil, err
	}

	return &run, nil
}

func (c *Client) get(ctx context.Context, resource schema.GroupVersionResource, namespace, name, kind string, out any) error {
	obj, err := c.dynamic.Resource(resource).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return apiError(err, fmt.Sprintf("get %s %s in namespace %s", kind, name, namespace))
	}

	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, out); err != nil {
		return fmt.Errorf("read %s %s: %w", kind, name, err)
	}

	return nil
}

// maxLogRead bounds how much of a log Logs reads, however long its lines.
const maxLogRead = 16 << 20

// Logs returns the end of one container's log: its last tailLines lines, of
// which it keeps the last keepBytes bytes, starting at a line boundary.
// truncated says the start was dropped.
func (c *Client) Logs(ctx context.Context, namespace, pod, container string, tailLines int64, keepBytes int) (log string, truncated bool, err error) {
	stream, err := c.core.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
		TailLines: &tailLines,
	}).Stream(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, fmt.Errorf("pod %s or its container %s is gone from namespace %s, and the log with it: Tekton or a pruner deleted the pod after the run", pod, container, namespace)
		}

		return "", false, apiError(err, fmt.Sprintf("read the log of container %s in pod %s", container, pod))
	}

	defer func() { _ = stream.Close() }()

	tail := &tailBuffer{keep: keepBytes}

	n, err := io.Copy(tail, io.LimitReader(stream, maxLogRead))
	if err != nil {
		return "", false, fmt.Errorf("read the log of container %s in pod %s: %w", container, pod, err)
	}

	if n == maxLogRead {
		return "", false, fmt.Errorf("the last %d lines of container %s's log are over %d MiB; ask for fewer lines", tailLines, container, maxLogRead>>20)
	}

	return tail.String(), tail.dropped, nil
}

// tailBuffer keeps the last keep bytes written to it.
type tailBuffer struct {
	keep    int
	buf     []byte
	dropped bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.keep; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
		t.dropped = true
	}

	return len(p), nil
}

// String returns what's kept, from the first whole line on when the start
// was dropped.
func (t *tailBuffer) String() string {
	if !t.dropped {
		return string(t.buf)
	}

	if i := bytes.IndexByte(t.buf, '\n'); i >= 0 {
		return string(t.buf[i+1:])
	}

	return string(t.buf)
}

// apiError turns a Kubernetes API error into a message the model can act
// on. The API server's own message already names the user and what was
// refused.
func apiError(err error, what string) error {
	var status apierrors.APIStatus

	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("could not %s: it doesn't exist", what)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("could not %s: Kubernetes denied it to this server's identity: %w", what, err)
	case apierrors.IsUnauthorized(err):
		return fmt.Errorf("could not %s: Kubernetes rejected this server's credentials (an expired or invalid token): %w", what, err)
	case errors.As(err, &status):
		return fmt.Errorf("could not %s: Kubernetes returned HTTP %d: %w", what, status.Status().Code, err)
	default:
		return fmt.Errorf("could not %s: %w", what, err)
	}
}
