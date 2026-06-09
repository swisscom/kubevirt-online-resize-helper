package controller

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const computeContainer = "compute"

// RemoteExecutor implements PodExecutor using kubectl-exec style remote execution.
type RemoteExecutor struct {
	config *rest.Config
}

// NewRemoteExecutor creates a RemoteExecutor with the given rest config.
func NewRemoteExecutor(config *rest.Config) *RemoteExecutor {
	return &RemoteExecutor{config: config}
}

// BlockResize execs `virsh blockresize <domain> <devicePath> 0` in the compute container.
func (e *RemoteExecutor) BlockResize(ctx context.Context, namespace, podName, domainName, devicePath string) error {
	cmd := []string{"virsh", "blockresize", domainName, devicePath, "0"}

	clientset, err := kubernetes.NewForConfig(e.config)
	if err != nil {
		return fmt.Errorf("creating clientset: %w", err)
	}

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: computeContainer,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(e.config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("creating executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("exec failed: %w, stderr: %s", err, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "is resized") {
		return fmt.Errorf("unexpected blockresize output: %s, stderr: %s", output, stderr.String())
	}

	return nil
}
