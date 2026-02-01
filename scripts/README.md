# MaaS Subscription Model v2 Installation Scripts

This directory contains installation and deployment scripts for the MaaS Subscription Model v2.

## install-maas-subscription.sh

Installs the MaaS Subscription Model v2 CRDs and optionally updates the odh-model-controller operator.

### Prerequisites

- `kubectl` configured to access your cluster
- CRDs generated (run `make manifests` in the repository root)
- (Optional) Container tool (`podman` or `docker`) if updating operator image

### Usage

```bash
# Install CRDs only
./scripts/install-maas-subscription.sh

# Install CRDs and update operator
./scripts/install-maas-subscription.sh --update-operator

# Custom namespace and image
./scripts/install-maas-subscription.sh \
  --namespace my-namespace \
  --image quay.io/myorg/odh-model-controller:v1.0.0 \
  --update-operator

# Using environment variables
NAMESPACE=my-namespace \
OPERATOR_IMAGE=my-image:tag \
UPDATE_OPERATOR=true \
./scripts/install-maas-subscription.sh
```

### Options

- `-n, --namespace NAMESPACE`: Target namespace (default: `opendatahub`)
- `-i, --image IMAGE`: Operator image (default: `quay.io/maas/odh-model-controller:latest`)
- `-u, --update-operator`: Update the operator deployment
- `-c, --container-tool TOOL`: Container tool to use: `podman` or `docker` (default: `podman`)
- `-h, --help`: Show help message

### What it does

1. **Checks prerequisites**: Verifies `kubectl` is available and cluster is accessible
2. **Installs CRDs**: Applies the three MaaS CRDs:
   - `MaaSAuthPolicy`
   - `MaaSSubscription`
   - `MaaSModel`
3. **Installs RBAC**: Applies the required RBAC resources
4. **Updates operator** (optional): Updates the odh-model-controller deployment with the new image
5. **Validates installation**: Verifies all CRDs are installed and operator is running

### Example Output

```
[INFO] =========================================
[INFO] MaaS Subscription Model v2 Installation
[INFO] =========================================
[INFO] Namespace: opendatahub
[INFO] Operator Image: quay.io/maas/odh-model-controller:latest
[INFO] Update Operator: true
[INFO] Container Tool: podman
[INFO] =========================================

[INFO] Checking prerequisites...
[INFO] Prerequisites check passed
[INFO] Installing MaaS Subscription Model v2 CRDs...
[INFO] Installing CRD: maasauthpolicies.maas.opendatahub.io
[INFO] Successfully installed CRD: maasauthpolicies.maas.opendatahub.io
...
[INFO] Installation completed successfully!
```

### Troubleshooting

**CRD files not found:**
```bash
# Generate CRDs first
cd /path/to/odh-model-controller
make manifests
```

**Operator deployment not found:**
- The script will warn if the deployment doesn't exist
- Ensure the odh-model-controller is deployed in the target namespace

**RBAC errors:**
- Ensure you have cluster-admin permissions or appropriate RBAC
- The script applies the RBAC from `config/rbac/role.yaml`

### Building the Operator Image

Before installing, build and push the operator image:

```bash
# Using Podman (default)
cd /path/to/odh-model-controller
make container-build IMG=quay.io/maas/odh-model-controller:latest CONTAINER_TOOL=podman
podman push quay.io/maas/odh-model-controller:latest

# Using Docker
make container-build IMG=quay.io/maas/odh-model-controller:latest CONTAINER_TOOL=docker
docker push quay.io/maas/odh-model-controller:latest
```

### Testing the Installation

After installation, test by creating a sample subscription:

```bash
kubectl apply -f - <<EOF
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: test-subscription
  namespace: default
spec:
  owner:
    groups:
      - name: "test-group"
  modelRefs:
    - name: test-model
      tokenRateLimits:
        - limit: 1000
          window: 1h
      billingRate:
        perToken: "0.000001"
  billingMetadata:
    organizationId: "test-org"
    costCenter: "test-center"
EOF

# Check the subscription
kubectl get maassubscription test-subscription

# Check if TokenRateLimitPolicy was created
kubectl get tokenratelimitpolicy -A
```
