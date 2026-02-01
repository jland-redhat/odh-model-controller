#!/usr/bin/env bash

set -euo pipefail

# Install MaaS Subscription Model v2 CRDs and Operator
# This script installs the Custom Resource Definitions for:
# - MaaSAuthPolicy
# - MaaSSubscription  
# - MaaSModel
# And optionally updates the odh-model-controller deployment

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
CRD_DIR="$REPO_ROOT/config/crd/bases"
RBAC_DIR="$REPO_ROOT/config/rbac"

# Default values
NAMESPACE="${NAMESPACE:-opendatahub}"
OPERATOR_IMAGE="${OPERATOR_IMAGE:-quay.io/maas/odh-model-controller:latest}"
UPDATE_OPERATOR="${UPDATE_OPERATOR:-false}"
CONTAINER_TOOL="${CONTAINER_TOOL:-podman}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

check_prerequisites() {
    log_info "Checking prerequisites..."
    
    # Check if kubectl is available
    if ! command -v kubectl &> /dev/null; then
        log_error "kubectl is not installed or not in PATH"
        exit 1
    fi
    
    # Check if we can connect to cluster
    if ! kubectl cluster-info &> /dev/null; then
        log_error "Cannot connect to Kubernetes cluster"
        exit 1
    fi
    
    log_info "Prerequisites check passed"
}

install_crds() {
    log_info "Installing MaaS Subscription Model v2 CRDs..."
    
    local crds=(
        "maas.opendatahub.io_maasauthpolicies"
        "maas.opendatahub.io_maassubscriptions"
        "maas.opendatahub.io_maasmodels"
    )
    
    local crd_names=(
        "maasauthpolicies.maas.opendatahub.io"
        "maassubscriptions.maas.opendatahub.io"
        "maasmodels.maas.opendatahub.io"
    )
    
    for i in "${!crds[@]}"; do
        local crd_file="$CRD_DIR/${crds[$i]}.yaml"
        local crd_name="${crd_names[$i]}"
        if [[ ! -f "$crd_file" ]]; then
            log_error "CRD file not found: $crd_file"
            log_error "Please run 'make manifests' first to generate CRDs"
            exit 1
        fi
        
        log_info "Installing CRD: $crd_name"
        if kubectl apply -f "$crd_file"; then
            log_info "Successfully installed CRD: $crd_name"
        else
            log_error "Failed to install CRD: $crd_name"
            exit 1
        fi
    done
    
    log_info "Waiting for CRDs to be established..."
    for crd_name in "${crd_names[@]}"; do
        if kubectl wait --for=condition=Established --timeout=60s crd/"$crd_name" 2>/dev/null; then
            log_info "CRD $crd_name is established"
        else
            log_warn "CRD $crd_name may not be fully established yet"
        fi
    done
}

install_rbac() {
    log_info "Installing RBAC resources..."
    
    local rbac_file="$RBAC_DIR/role.yaml"
    if [[ ! -f "$rbac_file" ]]; then
        log_error "RBAC file not found: $rbac_file"
        exit 1
    fi
    
    log_info "Applying RBAC resources..."
    if kubectl apply -f "$rbac_file"; then
        log_info "Successfully installed RBAC resources"
    else
        log_error "Failed to install RBAC resources"
        exit 1
    fi
    
    # Annotate ClusterRole and ClusterRoleBinding to prevent opendatahub-operator from managing them
    log_info "Annotating ClusterRole and ClusterRoleBinding to prevent operator management..."
    kubectl annotate clusterrole odh-model-controller-role opendatahub.io/managed='false' --overwrite 2>/dev/null || {
        log_warn "Failed to annotate ClusterRole (may already be annotated)"
    }
    
    local binding_name="odh-model-controller-rolebinding-opendatahub"
    kubectl annotate clusterrolebinding "$binding_name" opendatahub.io/managed='false' --overwrite 2>/dev/null || {
        log_warn "Failed to annotate ClusterRoleBinding (may already be annotated or not found)"
    }
    
    log_info "Successfully annotated RBAC resources"
    
    # Patch the ClusterRole to add MaaS permissions
    # This is necessary because the opendatahub-operator manages the ClusterRole
    # and may overwrite direct edits, so we use a JSON patch
    log_info "Patching ClusterRole to add MaaS permissions..."
    if kubectl patch clusterrole odh-model-controller-role --type='json' -p='[
      {
        "op": "add",
        "path": "/rules/-",
        "value": {
          "apiGroups": ["maas.opendatahub.io"],
          "resources": ["maasauthpolicies", "maasmodels", "maassubscriptions"],
          "verbs": ["create", "delete", "get", "list", "patch", "update", "watch"]
        }
      },
      {
        "op": "add",
        "path": "/rules/-",
        "value": {
          "apiGroups": ["maas.opendatahub.io"],
          "resources": ["maasauthpolicies/finalizers", "maasmodels/finalizers", "maassubscriptions/finalizers"],
          "verbs": ["update"]
        }
      },
      {
        "op": "add",
        "path": "/rules/-",
        "value": {
          "apiGroups": ["maas.opendatahub.io"],
          "resources": ["maasauthpolicies/status", "maasmodels/status", "maassubscriptions/status"],
          "verbs": ["get", "patch", "update"]
        }
      }
    ]' 2>&1; then
        log_info "Successfully patched ClusterRole with MaaS permissions"
    else
        log_warn "Failed to patch ClusterRole. Checking if permissions already exist..."
        # Check if permissions already exist
        if kubectl get clusterrole odh-model-controller-role -o json | jq -e '.rules[] | select(.apiGroups[] == "maas.opendatahub.io")' > /dev/null 2>&1; then
            log_info "MaaS permissions already exist in ClusterRole"
        else
            log_error "Failed to add MaaS permissions to ClusterRole"
            log_error "You may need to manually patch the ClusterRole or update the opendatahub-operator manifests"
            exit 1
        fi
    fi
    
    log_info "Waiting for authorization cache to refresh (this may take up to 2 minutes)..."
    sleep 5
}

update_operator() {
    if [[ "$UPDATE_OPERATOR" != "true" ]]; then
        log_info "Skipping operator update (set UPDATE_OPERATOR=true to enable)"
        return
    fi
    
    log_info "Updating odh-model-controller deployment..."
    
    # Check if deployment exists
    if ! kubectl get deployment odh-model-controller -n "$NAMESPACE" &> /dev/null; then
        log_warn "Deployment odh-model-controller not found in namespace $NAMESPACE"
        log_warn "Skipping operator update"
        return
    fi
    
    # Update the image
    log_info "Updating deployment image to: $OPERATOR_IMAGE"
    if kubectl set image deployment/odh-model-controller -n "$NAMESPACE" manager="$OPERATOR_IMAGE"; then
        log_info "Image updated successfully"
    else
        log_error "Failed to update deployment image"
        exit 1
    fi
    
    # Wait for rollout
    log_info "Waiting for deployment rollout..."
    if kubectl rollout status deployment/odh-model-controller -n "$NAMESPACE" --timeout=120s; then
        log_info "Deployment rollout completed successfully"
    else
        log_error "Deployment rollout failed or timed out"
        exit 1
    fi
}

validate_installation() {
    log_info "Validating installation..."
    
    local crd_names=(
        "maasauthpolicies.maas.opendatahub.io"
        "maassubscriptions.maas.opendatahub.io"
        "maasmodels.maas.opendatahub.io"
    )
    
    local all_valid=true
    
    for crd_name in "${crd_names[@]}"; do
        if kubectl get crd "$crd_name" &> /dev/null; then
            log_info "✓ CRD $crd_name is installed"
        else
            log_error "✗ CRD $crd_name is not installed"
            all_valid=false
        fi
    done
    
    # Check if operator is running
    if kubectl get deployment odh-model-controller -n "$NAMESPACE" &> /dev/null; then
        local ready=$(kubectl get deployment odh-model-controller -n "$NAMESPACE" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
        local desired=$(kubectl get deployment odh-model-controller -n "$NAMESPACE" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "0")
        
        if [[ "$ready" == "$desired" && "$ready" != "0" ]]; then
            log_info "✓ odh-model-controller deployment is running ($ready/$desired replicas ready)"
        else
            log_warn "⚠ odh-model-controller deployment is not fully ready ($ready/$desired replicas ready)"
        fi
    else
        log_warn "⚠ odh-model-controller deployment not found in namespace $NAMESPACE"
    fi
    
    if [[ "$all_valid" == "true" ]]; then
        log_info "Installation validation passed"
        return 0
    else
        log_error "Installation validation failed"
        return 1
    fi
}

show_usage() {
    cat << EOF
Usage: $0 [OPTIONS]

Install MaaS Subscription Model v2 CRDs and optionally update the operator.

Options:
    -n, --namespace NAMESPACE       Target namespace (default: opendatahub)
    -i, --image IMAGE               Operator image (default: quay.io/maas/odh-model-controller:latest)
    -u, --update-operator           Update the operator deployment
    -c, --container-tool TOOL       Container tool to use: podman or docker (default: podman)
    -h, --help                      Show this help message

Environment Variables:
    NAMESPACE                       Target namespace
    OPERATOR_IMAGE                  Operator container image
    UPDATE_OPERATOR                 Set to 'true' to update operator (default: false)
    CONTAINER_TOOL                  Container tool: podman or docker

Examples:
    # Install CRDs only
    $0

    # Install CRDs and update operator
    $0 --update-operator

    # Install with custom namespace and image
    $0 --namespace my-namespace --image quay.io/myorg/odh-model-controller:v1.0.0 --update-operator

    # Using environment variables
    NAMESPACE=my-namespace OPERATOR_IMAGE=my-image:tag UPDATE_OPERATOR=true $0
EOF
}

parse_args() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            -n|--namespace)
                NAMESPACE="$2"
                shift 2
                ;;
            -i|--image)
                OPERATOR_IMAGE="$2"
                shift 2
                ;;
            -u|--update-operator)
                UPDATE_OPERATOR="true"
                shift
                ;;
            -c|--container-tool)
                CONTAINER_TOOL="$2"
                shift 2
                ;;
            -h|--help)
                show_usage
                exit 0
                ;;
            *)
                log_error "Unknown option: $1"
                show_usage
                exit 1
                ;;
        esac
    done
}

main() {
    parse_args "$@"
    
    log_info "========================================="
    log_info "MaaS Subscription Model v2 Installation"
    log_info "========================================="
    log_info "Namespace: $NAMESPACE"
    log_info "Operator Image: $OPERATOR_IMAGE"
    log_info "Update Operator: $UPDATE_OPERATOR"
    log_info "Container Tool: $CONTAINER_TOOL"
    log_info "========================================="
    echo
    
    check_prerequisites
    install_crds
    install_rbac
    
    if [[ "$UPDATE_OPERATOR" == "true" ]]; then
        update_operator
    fi
    
    validate_installation
    
    echo
    log_info "========================================="
    log_info "Installation completed successfully!"
    log_info "========================================="
    log_info ""
    log_info "Next steps:"
    log_info "1. Create a MaaSSubscription resource:"
    log_info "   kubectl apply -f - <<EOF"
    log_info "   apiVersion: maas.opendatahub.io/v1alpha1"
    log_info "   kind: MaaSSubscription"
    log_info "   metadata:"
    log_info "     name: my-subscription"
    log_info "   spec:"
    log_info "     owner:"
    log_info "       groups:"
    log_info "         - name: my-group"
    log_info "     modelRefs:"
    log_info "       - name: my-model"
    log_info "         tokenRateLimits:"
    log_info "           - limit: 1000"
    log_info "             window: 1h"
    log_info "   EOF"
    log_info ""
    log_info "2. Check the created resources:"
    log_info "   kubectl get maassubscriptions"
    log_info "   kubectl get tokenratelimitpolicies"
    log_info ""
}

main "$@"
