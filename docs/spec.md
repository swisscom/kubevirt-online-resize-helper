# kubevirt-online-resize-helper

## Overview

A minimal Kubernetes operator that propagates PVC volume expansions to running KubeVirt VMs for hotplugged block volumes. It bridges a gap in KubeVirt where the `ExpandDisks` feature gate does not correctly handle online resize for hotplug volumes.

## Problem Statement

When a PVC is resized in a KKP user cluster backed by a KubeVirt provider:

1. The kubevirt-csi-driver successfully expands the infra PVC
2. The block device on the infra node reflects the new size
3. The mknod device node in the virt-launcher pod reflects the new size
4. **But:** virt-controller does not update `persistentVolumeClaimInfo.capacity` on the VMI for hotplug volumes
5. **And:** virt-handler never calls `virsh blockresize` to notify QEMU
6. The VM guest never sees the expanded disk

Manual workaround:
- Annotating the VMI forces virt-controller to update the capacity
- Running `virsh blockresize <domain> <device-path> 0` in the virt-launcher compute container notifies QEMU

This operator automates both steps.

## Architecture

```
┌──────────────────────────────────────┐
│ KKP Seed / Management Cluster        │
│                                      │
│  ┌────────────────────────────────┐  │
│  │ kubevirt-online-resize-helper  │  │
│  │ (connects to infra cluster     │  │
│  │  via --infra-kubeconfig)       │  │
│  └──────────────┬─────────────────┘  │
└─────────────────┼────────────────────┘
                  │ kubeconfig
                  ▼
┌──────────────────────────────────────┐
│ KubeVirt Infra Cluster               │
│ (namespace: <cluster-id>)            │
│                                      │
│  ┌─────────┐  ┌─────┐  ┌─────────┐  │
│  │   PVC   │  │ VMI │  │virt-    │  │
│  │ (infra) │  │     │  │launcher │  │
│  └─────────┘  └─────┘  └─────────┘  │
└──────────────────────────────────────┘
```

The operator runs in the seed cluster and connects to the KubeVirt infra cluster
using a kubeconfig passed via `--infra-kubeconfig`. All watches (PVCs, VMIs) and
operations (annotate VMI, exec into pods) happen on the remote infra cluster.

## Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Framework | controller-runtime | Matches kubevirt-csi-driver-operator |
| Namespace scope | Single namespace (flag) | Kubeconfig is namespace-scoped |
| Hotplug detection | Watch VMIs, extract `spec.volumes[].dataVolume.hotpluggable == true` | Exact matching, no name-pattern heuristics |
| VMI re-sync trigger | Annotate VMI | Minimal, non-invasive, triggers virt-controller reconcile |
| Block resize execution | kubectl-exec style (`remotecommand`) | No CRD or subresource API dependency |
| CRDs | None | Minimal footprint, state tracked via annotations |

## Reconciliation Flow

The operator has a single PVC reconciler:

### Trigger
PVC `status.capacity` changes (predicate filters non-capacity updates).

### Reconcile Logic

```
1. Is this PVC a hotpluggable volume?
   - List VMIs from cache
   - Check if any VMI has spec.volumes[].dataVolume.hotpluggable == true
     with name == pvc.Name
   - If not → skip

2. Is the PVC expansion complete?
   - Check PVC conditions: if Resizing or FileSystemResizePending present → requeue (5s)
   - If status.capacity reflects new size with no pending conditions → proceed

3. Does VMI already reflect the new capacity?
   - Compare pvc.Status.Capacity["storage"] with
     vmi.Status.VolumeStatus[].PersistentVolumeClaimInfo.Capacity["storage"]
   - If they match → check if blockresize already done (annotation), skip if so

4. Has the VMI been annotated?
   - If not: patch VMI annotation
     `skp.swisscom.com/resize-trigger: <unix-timestamp>`
   - Requeue (5s) to wait for status update

5. Has the VMI status been updated?
   - If VMI capacity still stale → requeue (5s, max 60s backoff)

6. Exec blockresize
   - Find virt-launcher pod: label `kubevirt.io/domain=<vmi.Name>`, container `compute`
   - Domain name: read from VMI (namespace_name format)
   - Device path: /var/run/kubevirt/hotplug-disks/<volume-name>
   - Exec: `virsh blockresize <domain> <device-path> 0`
   - On success: annotate PVC with
     `skp.swisscom.com/last-resized-capacity: <new-capacity>`

7. Done — tenant kubelet automatically retries NodeExpandVolume
```

## Identifying Hotplug Volumes

A volume is considered a hotplug volume when:
- It appears in `vmi.Spec.Volumes[]` with `dataVolume.hotpluggable: true`
- The DataVolume/PVC name matches the PVC being reconciled

VM OS disk DataVolumes have an owner reference to the VirtualMachine object. Hotplug DataVolumes created by the kubevirt-csi-driver do NOT have this owner reference.

## Domain Name Resolution

The libvirt domain name follows KubeVirt's convention:
```
<namespace>_<vmi-name>
```

This is derived from `api.VMINamespaceKeyFunc(vmi)` in KubeVirt source code.

## RBAC Requirements

```yaml
- apiGroups: [""]
  resources: [persistentvolumeclaims]
  verbs: [get, list, watch, patch]
- apiGroups: [""]
  resources: [pods]
  verbs: [get, list]
- apiGroups: [""]
  resources: [pods/exec]
  verbs: [create]
- apiGroups: [kubevirt.io]
  resources: [virtualmachineinstances]
  verbs: [get, list, watch, patch]
```

## Configuration

| Flag/Env | Description | Default |
|----------|-------------|---------|
| `--infra-kubeconfig` / `INFRA_KUBECONFIG` | Path to kubeconfig for the KubeVirt infra cluster | Required |
| `--namespace` / `WATCH_NAMESPACE` | Namespace to watch on the infra cluster | Required |

## Annotations Used

| Annotation | On | Purpose |
|------------|----|---------| 
| `skp.swisscom.com/resize-trigger` | VMI | Forces virt-controller re-sync (value: unix timestamp) |
| `skp.swisscom.com/last-resized-capacity` | PVC | Tracks last successfully resized capacity to avoid re-processing |

## Error Handling

| Scenario | Behavior |
|----------|----------|
| PVC expansion still in progress | Requeue with 5s delay |
| VMI not found (deleted) | Skip, no error |
| VMI status not updated after annotation | Requeue with backoff, max 120s |
| virt-launcher pod not found | Requeue with 10s delay |
| `virsh blockresize` fails | Log error, requeue with 30s delay |
| Exec timeout | Requeue with 30s delay |
| PVC already processed (annotation matches) | Skip |

## Deployment

- Single replica (no leader election required for single-namespace operation)
- Deployed as a Deployment in the KKP seed cluster
- Receives a kubeconfig with access to the infra namespace
- Container image built from Dockerfile

## Future Considerations

- If KubeVirt upstream fixes the `ExpandDisks` gap for hotplug volumes, this operator becomes unnecessary
- Could be integrated into the kubevirt-csi-driver-operator as an additional controller
- DataVolume `spec.storage.resources.requests` could optionally be patched for consistency
