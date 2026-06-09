package controller

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	ctrl "sigs.k8s.io/controller-runtime"

	kubevirtv1 "kubevirt.io/api/core/v1"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = kubevirtv1.AddToScheme(s)
	return s
}

// mockAnnotator records calls for testing.
type mockAnnotator struct {
	called bool
	key    string
	value  string
}

func (m *mockAnnotator) Annotate(_ context.Context, _ *kubevirtv1.VirtualMachineInstance, key, value string) error {
	m.called = true
	m.key = key
	m.value = value
	return nil
}

// mockExecutor records exec calls for testing.
type mockExecutor struct {
	called     bool
	domainName string
	devicePath string
	err        error
}

func (m *mockExecutor) BlockResize(_ context.Context, _, _, domainName, devicePath string) error {
	m.called = true
	m.domainName = domainName
	m.devicePath = devicePath
	return m.err
}

func newTestReconciler(objs ...client.Object) (*PVCReconciler, *mockAnnotator, *mockExecutor) {
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).Build()
	ann := &mockAnnotator{}
	exec := &mockExecutor{}
	return &PVCReconciler{client: c, annotator: ann, executor: exec}, ann, exec
}

func TestFindHotplugOwner_MatchesHotpluggableVolume(t *testing.T) {
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "test-vmi", Namespace: "ns1"},
		Spec: kubevirtv1.VirtualMachineInstanceSpec{
			Volumes: []kubevirtv1.Volume{
				{
					Name: "os-disk",
					VolumeSource: kubevirtv1.VolumeSource{
						DataVolume: &kubevirtv1.DataVolumeSource{Name: "os-dv"},
					},
				},
				{
					Name: "hotplug-vol",
					VolumeSource: kubevirtv1.VolumeSource{
						DataVolume: &kubevirtv1.DataVolumeSource{
							Name:         "pvc-abc123",
							Hotpluggable: true,
						},
					},
				},
			},
		},
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-abc123", Namespace: "ns1"},
		Status: corev1.PersistentVolumeClaimStatus{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")},
		},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(vmi, pvc).Build()
	r := &PVCReconciler{client: c}

	foundVMI, volName, err := r.findHotplugOwner(context.Background(), pvc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if foundVMI == nil {
		t.Fatal("expected to find VMI, got nil")
	}
	if foundVMI.Name != "test-vmi" {
		t.Errorf("expected VMI name 'test-vmi', got %q", foundVMI.Name)
	}
	if volName != "hotplug-vol" {
		t.Errorf("expected volume name 'hotplug-vol', got %q", volName)
	}
}

func TestFindHotplugOwner_SkipsNonHotpluggable(t *testing.T) {
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "test-vmi", Namespace: "ns1"},
		Spec: kubevirtv1.VirtualMachineInstanceSpec{
			Volumes: []kubevirtv1.Volume{
				{
					Name: "os-disk",
					VolumeSource: kubevirtv1.VolumeSource{
						DataVolume: &kubevirtv1.DataVolumeSource{Name: "pvc-abc123"},
					},
				},
			},
		},
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-abc123", Namespace: "ns1"},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(vmi, pvc).Build()
	r := &PVCReconciler{client: c}

	foundVMI, _, err := r.findHotplugOwner(context.Background(), pvc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if foundVMI != nil {
		t.Errorf("expected nil VMI for non-hotpluggable volume, got %q", foundVMI.Name)
	}
}

func TestFindHotplugOwner_NoMatchingPVC(t *testing.T) {
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "test-vmi", Namespace: "ns1"},
		Spec: kubevirtv1.VirtualMachineInstanceSpec{
			Volumes: []kubevirtv1.Volume{
				{
					Name: "hotplug-vol",
					VolumeSource: kubevirtv1.VolumeSource{
						DataVolume: &kubevirtv1.DataVolumeSource{
							Name:         "pvc-other",
							Hotpluggable: true,
						},
					},
				},
			},
		},
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-abc123", Namespace: "ns1"},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(vmi, pvc).Build()
	r := &PVCReconciler{client: c}

	foundVMI, _, err := r.findHotplugOwner(context.Background(), pvc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if foundVMI != nil {
		t.Errorf("expected nil VMI, got %q", foundVMI.Name)
	}
}

func TestReconcile_SkipsNonHotplugPVC(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "some-pvc", Namespace: "ns1"},
		Status: corev1.PersistentVolumeClaimStatus{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")},
		},
	}

	r, _, _ := newTestReconciler(pvc)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "some-pvc", Namespace: "ns1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Error("expected no requeue for non-hotplug PVC")
	}
}

func TestCapacityChangePredicate(t *testing.T) {
	pred := capacityChangePredicate()

	oldPVC := &corev1.PersistentVolumeClaim{
		Status: corev1.PersistentVolumeClaimStatus{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")},
		},
	}
	newPVC := &corev1.PersistentVolumeClaim{
		Status: corev1.PersistentVolumeClaimStatus{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")},
		},
	}

	if !pred.Update(event.UpdateEvent{ObjectOld: oldPVC, ObjectNew: newPVC}) {
		t.Error("predicate should pass when capacity changes")
	}

	samePVC := oldPVC.DeepCopy()
	if pred.Update(event.UpdateEvent{ObjectOld: oldPVC, ObjectNew: samePVC}) {
		t.Error("predicate should not pass when capacity is unchanged")
	}

	if pred.Create(event.CreateEvent{Object: newPVC}) {
		t.Error("predicate should not pass create events")
	}
}

func TestReconcile_RequeuesWhilePVCResizing(t *testing.T) {
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "test-vmi", Namespace: "ns1"},
		Spec: kubevirtv1.VirtualMachineInstanceSpec{
			Volumes: []kubevirtv1.Volume{
				{
					Name: "hotplug-vol",
					VolumeSource: kubevirtv1.VolumeSource{
						DataVolume: &kubevirtv1.DataVolumeSource{
							Name:         "pvc-abc123",
							Hotpluggable: true,
						},
					},
				},
			},
		},
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-abc123", Namespace: "ns1"},
		Status: corev1.PersistentVolumeClaimStatus{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")},
			Conditions: []corev1.PersistentVolumeClaimCondition{
				{Type: corev1.PersistentVolumeClaimResizing, Status: corev1.ConditionTrue},
			},
		},
	}

	r, _, _ := newTestReconciler(vmi, pvc)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "pvc-abc123", Namespace: "ns1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue while PVC is resizing")
	}
}

func TestReconcile_AnnotatesVMIOnCapacityMismatch(t *testing.T) {
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "test-vmi", Namespace: "ns1"},
		Spec: kubevirtv1.VirtualMachineInstanceSpec{
			Volumes: []kubevirtv1.Volume{
				{
					Name: "hotplug-vol",
					VolumeSource: kubevirtv1.VolumeSource{
						DataVolume: &kubevirtv1.DataVolumeSource{
							Name:         "pvc-abc123",
							Hotpluggable: true,
						},
					},
				},
			},
		},
		Status: kubevirtv1.VirtualMachineInstanceStatus{
			VolumeStatus: []kubevirtv1.VolumeStatus{
				{
					Name: "hotplug-vol",
					PersistentVolumeClaimInfo: &kubevirtv1.PersistentVolumeClaimInfo{
						Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")},
					},
				},
			},
		},
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-abc123", Namespace: "ns1"},
		Status: corev1.PersistentVolumeClaimStatus{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")},
		},
	}

	r, mock, _ := newTestReconciler(vmi, pvc)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "pvc-abc123", Namespace: "ns1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after annotating VMI")
	}
	if !mock.called {
		t.Error("expected annotator to be called")
	}
	if mock.key != AnnotationResizeTrigger {
		t.Errorf("expected annotation key %q, got %q", AnnotationResizeTrigger, mock.key)
	}
}

func TestReconcile_SkipsAlreadyProcessedPVC(t *testing.T) {
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "test-vmi", Namespace: "ns1"},
		Spec: kubevirtv1.VirtualMachineInstanceSpec{
			Volumes: []kubevirtv1.Volume{
				{
					Name: "hotplug-vol",
					VolumeSource: kubevirtv1.VolumeSource{
						DataVolume: &kubevirtv1.DataVolumeSource{
							Name:         "pvc-abc123",
							Hotpluggable: true,
						},
					},
				},
			},
		},
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pvc-abc123",
			Namespace: "ns1",
			Annotations: map[string]string{
				AnnotationLastResized: "4Gi",
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")},
		},
	}

	r, mock, _ := newTestReconciler(vmi, pvc)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "pvc-abc123", Namespace: "ns1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Error("expected no requeue for already-processed PVC")
	}
	if mock.called {
		t.Error("expected annotator NOT to be called for already-processed PVC")
	}
}

func TestPvcIsResizing(t *testing.T) {
	tests := []struct {
		name       string
		conditions []corev1.PersistentVolumeClaimCondition
		want       bool
	}{
		{"no conditions", nil, false},
		{"resizing", []corev1.PersistentVolumeClaimCondition{
			{Type: corev1.PersistentVolumeClaimResizing, Status: corev1.ConditionTrue},
		}, true},
		{"fs resize pending", []corev1.PersistentVolumeClaimCondition{
			{Type: corev1.PersistentVolumeClaimFileSystemResizePending, Status: corev1.ConditionTrue},
		}, true},
		{"condition false", []corev1.PersistentVolumeClaimCondition{
			{Type: corev1.PersistentVolumeClaimResizing, Status: corev1.ConditionFalse},
		}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pvc := &corev1.PersistentVolumeClaim{
				Status: corev1.PersistentVolumeClaimStatus{Conditions: tt.conditions},
			}
			if got := pvcIsResizing(pvc); got != tt.want {
				t.Errorf("pvcIsResizing() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVmiVolumeCapacity(t *testing.T) {
	vmi := &kubevirtv1.VirtualMachineInstance{
		Status: kubevirtv1.VirtualMachineInstanceStatus{
			VolumeStatus: []kubevirtv1.VolumeStatus{
				{
					Name: "vol1",
					PersistentVolumeClaimInfo: &kubevirtv1.PersistentVolumeClaimInfo{
						Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")},
					},
				},
				{Name: "vol2"}, // no PVC info
			},
		},
	}

	cap := vmiVolumeCapacity(vmi, "vol1")
	if cap.String() != "2Gi" {
		t.Errorf("expected 2Gi, got %s", cap.String())
	}

	cap = vmiVolumeCapacity(vmi, "vol2")
	if !cap.IsZero() {
		t.Errorf("expected zero, got %s", cap.String())
	}

	cap = vmiVolumeCapacity(vmi, "nonexistent")
	if !cap.IsZero() {
		t.Errorf("expected zero for missing volume, got %s", cap.String())
	}
}

func TestReconcile_FindsVirtLauncherPod(t *testing.T) {
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "test-vmi", Namespace: "ns1"},
		Spec: kubevirtv1.VirtualMachineInstanceSpec{
			Volumes: []kubevirtv1.Volume{
				{
					Name: "hotplug-vol",
					VolumeSource: kubevirtv1.VolumeSource{
						DataVolume: &kubevirtv1.DataVolumeSource{
							Name:         "pvc-abc123",
							Hotpluggable: true,
						},
					},
				},
			},
		},
		Status: kubevirtv1.VirtualMachineInstanceStatus{
			VolumeStatus: []kubevirtv1.VolumeStatus{
				{
					Name: "hotplug-vol",
					PersistentVolumeClaimInfo: &kubevirtv1.PersistentVolumeClaimInfo{
						Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")},
					},
				},
			},
		},
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-abc123", Namespace: "ns1"},
		Status: corev1.PersistentVolumeClaimStatus{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")},
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "virt-launcher-test-vmi-abc",
			Namespace: "ns1",
			Labels:    map[string]string{"kubevirt.io/domain": "test-vmi"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r, _, _ := newTestReconciler(vmi, pvc, pod)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "pvc-abc123", Namespace: "ns1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should not requeue — pod found, ready for exec (Task 4 placeholder returns success)
	if result.RequeueAfter != 0 {
		t.Errorf("unexpected requeue: %v", result)
	}
}

func TestReconcile_RequeuesWhenPodNotFound(t *testing.T) {
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "test-vmi", Namespace: "ns1"},
		Spec: kubevirtv1.VirtualMachineInstanceSpec{
			Volumes: []kubevirtv1.Volume{
				{
					Name: "hotplug-vol",
					VolumeSource: kubevirtv1.VolumeSource{
						DataVolume: &kubevirtv1.DataVolumeSource{
							Name:         "pvc-abc123",
							Hotpluggable: true,
						},
					},
				},
			},
		},
		Status: kubevirtv1.VirtualMachineInstanceStatus{
			VolumeStatus: []kubevirtv1.VolumeStatus{
				{
					Name: "hotplug-vol",
					PersistentVolumeClaimInfo: &kubevirtv1.PersistentVolumeClaimInfo{
						Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")},
					},
				},
			},
		},
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-abc123", Namespace: "ns1"},
		Status: corev1.PersistentVolumeClaimStatus{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")},
		},
	}

	// No pod exists
	r, _, _ := newTestReconciler(vmi, pvc)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "pvc-abc123", Namespace: "ns1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue when virt-launcher pod not found")
	}
}

func TestFindVirtLauncherPod_SkipsTerminatingPods(t *testing.T) {
	now := metav1.Now()
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "test-vmi", Namespace: "ns1"},
	}

	terminatingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "virt-launcher-old",
			Namespace:         "ns1",
			Labels:            map[string]string{"kubevirt.io/domain": "test-vmi"},
			DeletionTimestamp: &now,
			Finalizers:        []string{"test-finalizer"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r, _, _ := newTestReconciler(terminatingPod)

	pod, err := r.findVirtLauncherPod(context.Background(), vmi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pod != nil {
		t.Error("expected nil for terminating pod")
	}
}

func TestReconcile_ExecsBlockResizeAndMarksPVC(t *testing.T) {
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "my-vmi", Namespace: "ns1"},
		Spec: kubevirtv1.VirtualMachineInstanceSpec{
			Volumes: []kubevirtv1.Volume{
				{
					Name: "hotplug-vol",
					VolumeSource: kubevirtv1.VolumeSource{
						DataVolume: &kubevirtv1.DataVolumeSource{
							Name:         "pvc-abc123",
							Hotpluggable: true,
						},
					},
				},
			},
		},
		Status: kubevirtv1.VirtualMachineInstanceStatus{
			VolumeStatus: []kubevirtv1.VolumeStatus{
				{
					Name: "hotplug-vol",
					PersistentVolumeClaimInfo: &kubevirtv1.PersistentVolumeClaimInfo{
						Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")},
					},
				},
			},
		},
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-abc123", Namespace: "ns1"},
		Status: corev1.PersistentVolumeClaimStatus{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")},
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "virt-launcher-my-vmi-xyz",
			Namespace: "ns1",
			Labels:    map[string]string{"kubevirt.io/domain": "my-vmi"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r, _, execMock := newTestReconciler(vmi, pvc, pod)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "pvc-abc123", Namespace: "ns1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue after successful blockresize, got %v", result)
	}

	// Verify executor was called with correct args.
	if !execMock.called {
		t.Fatal("expected executor to be called")
	}
	if execMock.domainName != "ns1_my-vmi" {
		t.Errorf("expected domain 'ns1_my-vmi', got %q", execMock.domainName)
	}
	if execMock.devicePath != "/var/run/kubevirt/hotplug-disks/hotplug-vol" {
		t.Errorf("expected device path '/var/run/kubevirt/hotplug-disks/hotplug-vol', got %q", execMock.devicePath)
	}

	// Verify PVC was annotated as processed.
	var updatedPVC corev1.PersistentVolumeClaim
	if err := r.client.Get(context.Background(), types.NamespacedName{Name: "pvc-abc123", Namespace: "ns1"}, &updatedPVC); err != nil {
		t.Fatalf("failed to get PVC: %v", err)
	}
	if updatedPVC.Annotations[AnnotationLastResized] != "4Gi" {
		t.Errorf("expected last-resized annotation '4Gi', got %q", updatedPVC.Annotations[AnnotationLastResized])
	}
}

func TestReconcile_RequeuesOnExecFailure(t *testing.T) {
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "my-vmi", Namespace: "ns1"},
		Spec: kubevirtv1.VirtualMachineInstanceSpec{
			Volumes: []kubevirtv1.Volume{
				{
					Name: "hotplug-vol",
					VolumeSource: kubevirtv1.VolumeSource{
						DataVolume: &kubevirtv1.DataVolumeSource{
							Name:         "pvc-abc123",
							Hotpluggable: true,
						},
					},
				},
			},
		},
		Status: kubevirtv1.VirtualMachineInstanceStatus{
			VolumeStatus: []kubevirtv1.VolumeStatus{
				{
					Name: "hotplug-vol",
					PersistentVolumeClaimInfo: &kubevirtv1.PersistentVolumeClaimInfo{
						Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")},
					},
				},
			},
		},
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-abc123", Namespace: "ns1"},
		Status: corev1.PersistentVolumeClaimStatus{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")},
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "virt-launcher-my-vmi-xyz",
			Namespace: "ns1",
			Labels:    map[string]string{"kubevirt.io/domain": "my-vmi"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r, _, execMock := newTestReconciler(vmi, pvc, pod)
	execMock.err = fmt.Errorf("connection refused")

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "pvc-abc123", Namespace: "ns1"},
	})
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after exec failure")
	}
}
