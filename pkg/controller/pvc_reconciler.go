package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kubevirtv1 "kubevirt.io/api/core/v1"
)

// VMIAnnotator abstracts VMI annotation patching for testability.
type VMIAnnotator interface {
	Annotate(ctx context.Context, vmi *kubevirtv1.VirtualMachineInstance, key, value string) error
}

// PodExecutor abstracts exec into a pod for testability.
type PodExecutor interface {
	BlockResize(ctx context.Context, namespace, podName, domainName, devicePath string) error
}

// PVCReconciler watches PVCs and triggers hotplug volume resize when capacity changes.
type PVCReconciler struct {
	client    client.Client
	annotator VMIAnnotator
	executor  PodExecutor
}

// defaultAnnotator implements VMIAnnotator using the k8s client.
type defaultAnnotator struct {
	client client.Client
}

func (a *defaultAnnotator) Annotate(ctx context.Context, vmi *kubevirtv1.VirtualMachineInstance, key, value string) error {
	var patch []byte
	if vmi.Annotations == nil {
		patch = []byte(fmt.Sprintf(
			`[{"op":"add","path":"/metadata/annotations","value":{%q:%q}}]`,
			key, value,
		))
	} else {
		// JSON pointer: '/' in key must be escaped as '~1'
		escapedKey := strings.ReplaceAll(key, "/", "~1")
		patch = []byte(fmt.Sprintf(
			`[{"op":"add","path":"/metadata/annotations/%s","value":"%s"}]`,
			escapedKey, value,
		))
	}
	return a.client.Patch(ctx, vmi, client.RawPatch(types.JSONPatchType, patch))
}

// SetupPVCReconciler registers the PVC reconciler with the manager.
func SetupPVCReconciler(mgr ctrl.Manager) error {
	cfg := mgr.GetConfig()
	r := &PVCReconciler{
		client:    mgr.GetClient(),
		annotator: &defaultAnnotator{client: mgr.GetClient()},
		executor:  NewRemoteExecutor(cfg),
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.PersistentVolumeClaim{}).
		WithEventFilter(capacityChangePredicate()).
		Complete(r)
}

const (
	AnnotationResizeTrigger    = "skp.swisscom.com/resize-trigger"
	AnnotationLastResized      = "skp.swisscom.com/last-resized-capacity"
	requeueInterval            = 5 * time.Second
)

func (r *PVCReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var pvc corev1.PersistentVolumeClaim
	if err := r.client.Get(ctx, req.NamespacedName, &pvc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Find the VMI that has this PVC as a hotpluggable volume.
	vmi, volumeName, err := r.findHotplugOwner(ctx, &pvc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if vmi == nil {
		return ctrl.Result{}, nil
	}

	pvcCap := pvcCapacity(&pvc)

	// Skip if already processed at this capacity.
	if pvc.Annotations != nil && pvc.Annotations[AnnotationLastResized] == pvcCap.String() {
		return ctrl.Result{}, nil
	}

	// Wait for PVC expansion to complete on infra level.
	if pvcIsResizing(&pvc) {
		logger.Info("PVC still resizing, requeuing", "pvc", pvc.Name)
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	// Get VMI's view of this volume's capacity.
	vmiCap := vmiVolumeCapacity(vmi, volumeName)

	logger.Info("checking capacity mismatch",
		"pvc", pvc.Name, "vmi", vmi.Name, "volume", volumeName,
		"pvcCapacity", pvcCap.String(), "vmiCapacity", vmiCap.String(),
	)

	// If VMI doesn't yet reflect the new capacity, annotate to force re-sync.
	if !pvcCap.Equal(vmiCap) {
		if err := r.annotateVMI(ctx, vmi); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("annotated VMI to trigger re-sync", "vmi", vmi.Name)
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	// VMI capacity is updated. Find the virt-launcher pod.
	pod, err := r.findVirtLauncherPod(ctx, vmi)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pod == nil {
		logger.Info("virt-launcher pod not found, requeuing", "vmi", vmi.Name)
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	// Exec virsh blockresize in the compute container.
	domainName := vmi.Namespace + "_" + vmi.Name
	devicePath := "/var/run/kubevirt/hotplug-disks/" + volumeName

	if err := r.executor.BlockResize(ctx, pod.Namespace, pod.Name, domainName, devicePath); err != nil {
		logger.Error(err, "blockresize exec failed, requeuing", "pod", pod.Name)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Mark PVC as processed.
	if err := r.markResized(ctx, &pvc, pvcCap.String()); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("blockresize completed successfully",
		"pvc", pvc.Name, "vmi", vmi.Name, "capacity", pvcCap.String(),
	)
	return ctrl.Result{}, nil
}

// markResized annotates the PVC to record the last successfully resized capacity.
func (r *PVCReconciler) markResized(ctx context.Context, pvc *corev1.PersistentVolumeClaim, capacity string) error {
	patch := []byte(fmt.Sprintf(
		`{"metadata":{"annotations":{%q:%q}}}`,
		AnnotationLastResized, capacity,
	))
	return r.client.Patch(ctx, pvc, client.RawPatch(types.MergePatchType, patch))
}

// findVirtLauncherPod finds the virt-launcher pod for a VMI.
func (r *PVCReconciler) findVirtLauncherPod(ctx context.Context, vmi *kubevirtv1.VirtualMachineInstance) (*corev1.Pod, error) {
	var podList corev1.PodList
	if err := r.client.List(ctx, &podList,
		client.InNamespace(vmi.Namespace),
		client.MatchingLabels{"kubevirt.io/domain": vmi.Name},
	); err != nil {
		return nil, fmt.Errorf("listing virt-launcher pods: %w", err)
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		if pod.Status.Phase == corev1.PodRunning {
			return pod, nil
		}
	}
	return nil, nil
}

// pvcIsResizing returns true if the PVC has a Resizing or FileSystemResizePending condition.
func pvcIsResizing(pvc *corev1.PersistentVolumeClaim) bool {
	for _, cond := range pvc.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		if cond.Type == corev1.PersistentVolumeClaimResizing ||
			cond.Type == corev1.PersistentVolumeClaimFileSystemResizePending {
			return true
		}
	}
	return false
}

// vmiVolumeCapacity returns the capacity reported in the VMI's VolumeStatus for a given volume name.
func vmiVolumeCapacity(vmi *kubevirtv1.VirtualMachineInstance, volumeName string) resource.Quantity {
	for _, vs := range vmi.Status.VolumeStatus {
		if vs.Name != volumeName {
			continue
		}
		if vs.PersistentVolumeClaimInfo == nil {
			return resource.Quantity{}
		}
		if vs.PersistentVolumeClaimInfo.Capacity == nil {
			return resource.Quantity{}
		}
		if cap, ok := vs.PersistentVolumeClaimInfo.Capacity[corev1.ResourceStorage]; ok {
			return cap
		}
	}
	return resource.Quantity{}
}

// annotateVMI patches the VMI with a timestamp annotation to force virt-controller re-sync.
func (r *PVCReconciler) annotateVMI(ctx context.Context, vmi *kubevirtv1.VirtualMachineInstance) error {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	return r.annotator.Annotate(ctx, vmi, AnnotationResizeTrigger, ts)
}

// findHotplugOwner returns the VMI and volume name if this PVC backs a hotpluggable volume.
func (r *PVCReconciler) findHotplugOwner(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (*kubevirtv1.VirtualMachineInstance, string, error) {
	var vmiList kubevirtv1.VirtualMachineInstanceList
	if err := r.client.List(ctx, &vmiList, client.InNamespace(pvc.Namespace)); err != nil {
		return nil, "", fmt.Errorf("listing VMIs: %w", err)
	}

	for i := range vmiList.Items {
		vmi := &vmiList.Items[i]
		for _, vol := range vmi.Spec.Volumes {
			if vol.DataVolume == nil {
				continue
			}
			if !vol.DataVolume.Hotpluggable {
				continue
			}
			if vol.DataVolume.Name == pvc.Name {
				return vmi, vol.Name, nil
			}
		}
	}

	return nil, "", nil
}

// capacityChangePredicate only passes update events where PVC status.capacity changed.
func capacityChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return false },
		DeleteFunc: func(e event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPVC, ok := e.ObjectOld.(*corev1.PersistentVolumeClaim)
			if !ok {
				return false
			}
			newPVC, ok := e.ObjectNew.(*corev1.PersistentVolumeClaim)
			if !ok {
				return false
			}
			oldCap := oldPVC.Status.Capacity[corev1.ResourceStorage]
			newCap := newPVC.Status.Capacity[corev1.ResourceStorage]
			return !oldCap.Equal(newCap)
		},
	}
}

// pvcCapacity returns the storage capacity from a PVC status, or zero if unset.
func pvcCapacity(pvc *corev1.PersistentVolumeClaim) resource.Quantity {
	if pvc.Status.Capacity == nil {
		return resource.Quantity{}
	}
	return pvc.Status.Capacity[corev1.ResourceStorage]
}
