# Deployment

The kubevirt-online-resize-helper runs in the **KKP seed/management cluster** and connects to the **KubeVirt infra cluster** via a kubeconfig.

## Prerequisites

- Access to the KKP seed cluster (where the operator will be deployed)
- A kubeconfig for the KubeVirt infra cluster with the following permissions in the target namespace:
  - PVCs: get, list, watch, patch
  - Pods: get, list
  - Pods/exec: create
  - VirtualMachineInstances (kubevirt.io): get, list, watch, patch

## Step 1: Create RBAC on the infra cluster

Apply `rbac.yaml` on the **infra cluster**, replacing `{{ .Namespace }}` with the infra namespace (e.g., the KKP cluster namespace like `vega-117-kd9i8d`):

```bash
export INFRA_NAMESPACE=<your-infra-namespace>

sed "s/{{ .Namespace }}/${INFRA_NAMESPACE}/g" rbac.yaml | kubectl apply --kubeconfig=<infra-kubeconfig> -f -
```

## Step 2: Create the kubeconfig Secret on the seed cluster

The kubeconfig must authenticate as the ServiceAccount created in Step 1 (or any identity with equivalent permissions).

```bash
export SEED_NAMESPACE=<namespace-where-operator-runs>

kubectl create secret generic infra-kubeconfig \
  --from-file=kubeconfig=/path/to/infra-cluster-kubeconfig \
  -n ${SEED_NAMESPACE}
```

## Step 3: Deploy the operator on the seed cluster

Apply `deployment.yaml` on the **seed cluster**, replacing the template variables:

| Variable | Description | Example |
|----------|-------------|---------|
| `{{ .SeedNamespace }}` | Namespace on seed cluster for the operator | `kubermatic` |
| `{{ .Image }}` | Container image | `ghcr.io/swisscom/kubevirt-online-resize-helper:latest` |
| `{{ .InfraNamespace }}` | Namespace on the infra cluster to watch | `vega-117-kd9i8d` |
| `{{ .InfraKubeconfigSecret }}` | Name of the Secret created in Step 2 | `infra-kubeconfig` |

```bash
export SEED_NAMESPACE=kubermatic
export IMAGE=ghcr.io/swisscom/kubevirt-online-resize-helper:latest
export INFRA_NAMESPACE=vega-117-kd9i8d
export INFRA_SECRET=infra-kubeconfig

sed -e "s|{{ .SeedNamespace }}|${SEED_NAMESPACE}|g" \
    -e "s|{{ .Image }}|${IMAGE}|g" \
    -e "s|{{ .InfraNamespace }}|${INFRA_NAMESPACE}|g" \
    -e "s|{{ .InfraKubeconfigSecret }}|${INFRA_SECRET}|g" \
    deployment.yaml | kubectl apply -f -
```

## Verification

Check the operator is running:

```bash
kubectl get pods -n ${SEED_NAMESPACE} -l app=kubevirt-online-resize-helper
```

Check logs:

```bash
kubectl logs -n ${SEED_NAMESPACE} -l app=kubevirt-online-resize-helper -f
```

## Testing a resize

1. Expand a PVC in the user cluster (e.g., `kubectl edit pvc <name>` or via KKP dashboard)
2. Operator logs should show:
   - `hotplug PVC capacity change detected`
   - `annotated VMI to trigger re-sync`
   - `blockresize completed successfully`
3. Verify inside the VM: `lsblk` should show the new size
4. The filesystem resize happens automatically via the tenant kubelet
