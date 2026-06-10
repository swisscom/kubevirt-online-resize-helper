# kubevirt-online-resize-helper

Propagates PVC volume expansions to running KubeVirt VMs for hotplugged block volumes.

## Problem

When a PVC is expanded in a KKP user cluster on a KubeVirt provider, the infra PVC is resized successfully but the VM guest never sees the new block device size. This is due to two bugs in KubeVirt:

1. **virt-controller** doesn't update `persistentVolumeClaimInfo.capacity` for hotplug volumes after PVC expansion
2. **virt-handler** doesn't call `virsh blockresize` to notify QEMU even after the capacity is updated

This operator bridges the gap until upstream fixes are available.

## How it works

1. Watches PVCs on the KubeVirt infra cluster for capacity changes
2. Identifies hotpluggable volumes by cross-referencing VMI `spec.volumes`
3. Annotates the VMI to force virt-controller to update `persistentVolumeClaimInfo`
4. Waits for VMI status to reflect the new capacity
5. Execs `virsh blockresize <domain> <device-path> 0` in the virt-launcher compute container
6. Guest immediately sees the new block device size; tenant kubelet retries `NodeExpandVolume` to resize the filesystem

As it can happen, that the `kubevirt-csi-driver` flags the volume as resized even though the kubelet did not pick up on it yet, the `csidriver` needs to be adjusted and the `requireRepublish` flag set to `true`.
This ensures that the PV is revisited by the kubelet and resized if required.

## Usage

```bash
kubevirt-online-resize-helper \
  --infra-kubeconfig=/path/to/infra-cluster-kubeconfig \
  --namespace=<infra-namespace>
```

### Flags

| Flag | Env | Description |
|------|-----|-------------|
| `--infra-kubeconfig` | `INFRA_KUBECONFIG` | Kubeconfig for the KubeVirt infra cluster |
| `--namespace` | `WATCH_NAMESPACE` | Namespace on the infra cluster to watch |

## Deployment

The operator runs in the KKP seed/management cluster and connects to the KubeVirt infra cluster via a kubeconfig Secret.

```bash
kubectl create secret generic infra-kubeconfig \
  --from-file=kubeconfig=/path/to/infra-kubeconfig \
  -n <seed-namespace>
```

Apply the manifests in `deploy/`:
- `rbac.yaml` — Role on the **infra cluster** (PVCs, VMIs, pods, pods/exec)
- `deployment.yaml` — Deployment on the **seed cluster**

## RBAC (on infra cluster)

```yaml
rules:
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

## Building

```bash
go build -o kubevirt-online-resize-helper .
```

Docker:
```bash
docker build -t kubevirt-online-resize-helper .
```

## Testing

```bash
go test ./...
```

## How it identifies hotplug volumes

A volume is targeted when:
- It appears in `vmi.Spec.Volumes[]` with `dataVolume.hotpluggable: true`
- The DataVolume/PVC name matches the PVC whose capacity changed
- The PVC expansion is complete (no `Resizing` or `FileSystemResizePending` conditions)

## Idempotency

The operator annotates processed PVCs with `skp.swisscom.com/last-resized-capacity`. If the PVC's current capacity matches the annotation, it is skipped.
