package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"

	pkgerrors "github.com/pkg/errors"
	dto "github.com/prometheus/client_model/go"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/node/api/statsv1alpha1"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
)

// mockProvider is a nodeutil.Provider implementation which mocks the
// management of its Pods.
type mockProvider struct {
	podStore    cache.Store
	podLister   corev1listers.PodLister
	podNotifier func(*corev1.Pod)
}

var (
	_ nodeutil.Provider = (*mockProvider)(nil)
	_ node.PodNotifier  = (*mockProvider)(nil)
)

// CreatePod takes a Kubernetes Pod and deploys it within the provider.
func (p *mockProvider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	log.G(ctx).WithField("pod", cache.MetaObjectToName(pod)).Debug("Called CreatePod")

	trueVal := true
	falseVal := false

	icss := make([]corev1.ContainerStatus, 0, len(pod.Spec.InitContainers))
	for _, c := range pod.Spec.InitContainers {
		cid := containerID(cache.MetaObjectToName(pod), c.Name)

		cs := corev1.ContainerStatus{
			Name: c.Name,
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode:    0,
					Reason:      "Completed",
					StartedAt:   pod.CreationTimestamp,
					FinishedAt:  pod.CreationTimestamp,
					ContainerID: cid,
				},
			},
			Ready:       true,
			Image:       c.Image,
			ImageID:     c.Image,
			ContainerID: cid,
			Started:     &falseVal,
		}

		icss = append(icss, cs)
	}
	pod.Status.InitContainerStatuses = icss

	css := make([]corev1.ContainerStatus, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		cs := corev1.ContainerStatus{
			Name: c.Name,
			State: corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{
					StartedAt: pod.CreationTimestamp,
				},
			},
			Ready:       true,
			Image:       c.Image,
			ImageID:     c.Image,
			ContainerID: containerID(cache.MetaObjectToName(pod), c.Name),
			Started:     &trueVal,
		}

		css = append(css, cs)
	}
	pod.Status.ContainerStatuses = css

	newCondition := func(t corev1.PodConditionType) corev1.PodCondition {
		return corev1.PodCondition{
			Type:               t,
			Status:             corev1.ConditionTrue,
			LastProbeTime:      pod.CreationTimestamp,
			LastTransitionTime: pod.CreationTimestamp,
		}
	}
	pod.Status.Conditions = append(pod.Status.Conditions,
		newCondition(corev1.ContainersReady),
		newCondition(corev1.PodInitialized),
		newCondition(corev1.PodReady),
	)

	pod.Status.Phase = corev1.PodRunning

	if err := p.podStore.Add(pod); err != nil {
		return fmt.Errorf("adding created Pod to internal store: %w", err)
	}

	p.podNotifier(pod)
	return nil
}

// UpdatePod takes a Kubernetes Pod and updates it within the provider.
func (p *mockProvider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	log.G(ctx).WithField("pod", cache.MetaObjectToName(pod)).Debug("Called UpdatePod")

	if err := p.podStore.Update(pod); err != nil {
		return fmt.Errorf("updating Pod in internal store: %w", err)
	}

	p.podNotifier(pod)
	return nil
}

// DeletePod takes a Kubernetes Pod and deletes it from the provider. Once a pod is deleted, the provider is
// expected to call the NotifyPods callback with a terminal pod status where all the containers are in a terminal
// state, as well as the pod. DeletePod may be called multiple times for the same pod.
func (p *mockProvider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	log.G(ctx).WithField("pod", cache.MetaObjectToName(pod)).Debug("Called DeletePod")

	if err := p.podStore.Delete(pod); err != nil {
		return fmt.Errorf("deleting Pod from internal store: %w", err)
	}

	falseVal := false

	finishedAt := metav1.Now()
	if pod.DeletionTimestamp != nil {
		finishedAt = *pod.DeletionTimestamp
	}

	for i, cs := range pod.Status.ContainerStatuses {
		var startedAt metav1.Time
		if cs.State.Running != nil {
			startedAt = cs.State.Running.StartedAt
		}

		pod.Status.ContainerStatuses[i].State = corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode:    0,
				Reason:      "Completed",
				StartedAt:   startedAt,
				FinishedAt:  finishedAt,
				ContainerID: containerID(cache.MetaObjectToName(pod), cs.Name),
			},
		}

		pod.Status.ContainerStatuses[i].Ready = false
		pod.Status.ContainerStatuses[i].Started = &falseVal
	}

	for i, c := range pod.Status.Conditions {
		switch c.Type {
		case corev1.ContainersReady, corev1.PodReady:
			pod.Status.Conditions[i].Status = corev1.ConditionFalse
			pod.Status.Conditions[i].Reason = "PodCompleted"
			pod.Status.Conditions[i].Message = ""
			pod.Status.Conditions[i].LastProbeTime = finishedAt
			pod.Status.Conditions[i].LastTransitionTime = finishedAt
		case corev1.PodInitialized:
			pod.Status.Conditions[i].Reason = "PodCompleted"
			pod.Status.Conditions[i].Message = ""
			pod.Status.Conditions[i].LastProbeTime = finishedAt
			pod.Status.Conditions[i].LastTransitionTime = finishedAt
		}
	}

	pod.Status.Phase = corev1.PodSucceeded

	p.podNotifier(pod)
	return nil
}

// GetPod retrieves a pod by name from the provider (can be cached).
// The Pod returned is expected to be immutable, and may be accessed
// concurrently outside of the calling goroutine. Therefore it is recommended
// to return a version after DeepCopy.
func (p *mockProvider) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	log.G(ctx).WithField("pod", cache.NewObjectName(namespace, name)).Debug("Called GetPod")

	pod, err := p.podLister.Pods(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			err = errdefs.AsNotFound(err)
		}
		// NOTE(antoineco): the usage of pkgerrors is required for errdefs to
		// be able to unwrap and assert its own behavioral error types.
		return nil, pkgerrors.Wrap(err, "retrieving Pod from internal store")
	}

	return pod, nil
}

// GetPodStatus retrieves the status of a pod by name from the provider.
// The PodStatus returned is expected to be immutable, and may be accessed
// concurrently outside of the calling goroutine. Therefore it is recommended
// to return a version after DeepCopy.
func (*mockProvider) GetPodStatus(context.Context, string, string) (*corev1.PodStatus, error) {
	// NOTE(antoineco): the provider implements node.PodNotifier, so this
	// method will never be called.
	panic("not implemented")
}

// GetPods retrieves a list of all pods running on the provider (can be cached).
// The Pods returned are expected to be immutable, and may be accessed
// concurrently outside of the calling goroutine. Therefore it is recommended
// to return a version after DeepCopy.
func (p *mockProvider) GetPods(ctx context.Context) ([]*corev1.Pod, error) {
	log.G(ctx).Debug("Called GetPods")

	pods, err := p.podLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("retrieving all Pods from internal store: %w", err)
	}

	return pods, nil
}

// GetContainerLogs retrieves the logs of a container by name from the provider.
func (p *mockProvider) GetContainerLogs(ctx context.Context, namespace string, podName string, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	panic("not implemented") // TODO: Implement
}

// RunInContainer executes a command in a container in the pod, copying data
// between in/out/err and the container's stdin/stdout/stderr.
func (p *mockProvider) RunInContainer(ctx context.Context, namespace string, podName string, containerName string, cmd []string, attach api.AttachIO) error {
	panic("not implemented") // TODO: Implement
}

// AttachToContainer attaches to the executing process of a container in the pod, copying data
// between in/out/err and the container's stdin/stdout/stderr.
func (p *mockProvider) AttachToContainer(ctx context.Context, namespace string, podName string, containerName string, attach api.AttachIO) error {
	panic("not implemented") // TODO: Implement
}

// GetStatsSummary gets the stats for the node, including running pods
func (p *mockProvider) GetStatsSummary(_ context.Context) (*statsv1alpha1.Summary, error) {
	panic("not implemented") // TODO: Implement
}

// GetMetricsResource gets the metrics for the node, including running pods
func (p *mockProvider) GetMetricsResource(_ context.Context) ([]*dto.MetricFamily, error) {
	panic("not implemented") // TODO: Implement
}

// PortForward forwards a local port to a port on the pod
func (p *mockProvider) PortForward(ctx context.Context, namespace string, pod string, port int32, stream io.ReadWriteCloser) error {
	panic("not implemented") // TODO: Implement
}

// NotifyPods instructs the notifier to call the passed in function when
// the pod status changes. It should be called when a pod's status changes.
//
// The provided pointer to a Pod is guaranteed to be used in a read-only
// fashion. The provided pod's PodStatus should be up to date when
// this function is called.
//
// NotifyPods must not block the caller since it is only used to register the callback.
// The callback passed into `NotifyPods` may block when called.
func (p *mockProvider) NotifyPods(_ context.Context, notifyFn func(*corev1.Pod)) {
	p.podNotifier = notifyFn
}

// containerID returns a container ID based on the given pod identity and
// container name.
func containerID(n cache.ObjectName, container string) string {
	sum := sha256.Sum256([]byte(fmt.Sprint(n, "/", container)))
	return fmt.Sprintf("%s://%x", providerName, sum)
}
