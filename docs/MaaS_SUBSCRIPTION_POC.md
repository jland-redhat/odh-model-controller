# MaaS Subscription Model v2 - Proof of Concept

## Overview

This document describes the Proof of Concept (PoC) implementation of the MaaS (Models-as-a-Service) Subscription Model v2 within the `odh-model-controller` operator. The PoC implements a subscription-based model access control system that integrates with Kuadrant for authentication, authorization, and rate limiting.

## Architecture

The PoC extends the `odh-model-controller` operator with three new Custom Resource Definitions (CRDs) and their corresponding controllers:

1. **MaaSModel** - Represents a model endpoint (internal KServe or external)
2. **MaaSAuthPolicy** - Defines access control policies for models based on OIDC subjects/groups
3. **MaaSSubscription** - Defines subscription plans with per-model token rate limits, quotas, and billing information

### Component Relationships

```
┌─────────────────┐
│   MaaSModel     │ ──┐
└─────────────────┘   │
                       │
┌─────────────────┐    │    ┌──────────────────┐
│ MaaSAuthPolicy  │ ───┼───▶│   AuthPolicy     │
└─────────────────┘    │    │   (Kuadrant)     │
                       │    └──────────────────┘
┌─────────────────┐    │
│MaaSSubscription │ ───┘    ┌──────────────────┐
└─────────────────┘          │TokenRateLimitPolicy│
                              │   (Kuadrant)      │
                              └──────────────────┘
                                       │
                                       ▼
                              ┌──────────────────┐
                              │    HTTPRoute     │
                              │  (Gateway API)   │
                              └──────────────────┘
```

## Custom Resource Definitions

### MaaSModel

The `MaaSModel` CRD represents an AI/ML model endpoint that can be either:
- An internal KServe `LLMInferenceService` (kind: `llmisvc`)
- An external model endpoint (kind: `ExternalModel`)

#### Spec Fields

- `modelRef`: Reference to the actual model
  - `kind`: Either `"llmisvc"` or `"ExternalModel"`
  - `name`: Name of the model resource
  - `namespace`: Namespace of the model resource (optional, defaults to MaaSModel namespace)

#### Status Fields

- `phase`: Current phase (`Pending`, `Ready`, `Unhealthy`, `Failed`)
- `endpoint`: Endpoint URL for the model
- `httpRouteName`: Name of the HTTPRoute associated with this model
- `httpRouteNamespace`: Namespace of the HTTPRoute associated with this model
- `conditions`: Array of conditions representing the model's state

#### Example

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModel
metadata:
  name: example
  namespace: llm
spec:
  modelRef:
    kind: llmisvc
    name: facebook-opt-125m-simulated
    namespace: llm
```

### MaaSAuthPolicy

The `MaaSAuthPolicy` CRD defines access control policies for models, specifying which users and groups can access specific models.

#### Spec Fields

- `modelRefs`: Array of model names that this policy applies to
- `subjects`: Defines who can access the models
  - `users`: Array of usernames
  - `groups`: Array of group references
    - `name`: Group name
- `meteringMetadata`: Optional metadata for metering/billing
  - `organizationID`: Organization identifier
  - `costCenter`: Cost center identifier
  - `labels`: Additional labels for categorization

#### Status Fields

- `phase`: Current phase (`Active`, `Failed`)
- `conditions`: Array of conditions representing the policy's state

#### Example

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: example-auth-policy
  namespace: default
spec:
  modelRefs:
    - example
  subjects:
    users:
      - alice
      - bob
    groups:
      - name: data-scientists
  meteringMetadata:
    organizationID: "org-123"
    costCenter: "engineering"
```

### MaaSSubscription

The `MaaSSubscription` CRD defines subscription plans with per-model token rate limits, quotas, and billing information.

#### Spec Fields

- `modelRefs`: Array of model references with their rate limits
  - `name`: Model name
  - `tokenRateLimits`: Array of rate limit configurations
    - `limit`: Token limit per window
    - `window`: Time window (e.g., "1m", "1h", "1d")
- `owner`: Defines the subscription owner
  - `users`: Array of usernames
  - `groups`: Array of group references
- `billingMetadata`: Optional metadata for billing
  - `organizationID`: Organization identifier
  - `costCenter`: Cost center identifier
  - `labels`: Additional labels for categorization

#### Status Fields

- `phase`: Current phase (`Active`, `Failed`)
- `conditions`: Array of conditions representing the subscription's state

#### Example

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: premium-subscription
  namespace: default
spec:
  modelRefs:
    - name: example
      tokenRateLimits:
        - limit: 1000
          window: 1m
        - limit: 10000
          window: 1h
  owner:
    users:
      - alice
    groups:
      - name: premium-users
  billingMetadata:
    organizationID: "org-123"
    costCenter: "engineering"
```

## Controllers

### MaaSModel Controller

The `MaaSModelReconciler` manages the lifecycle of `MaaSModel` resources.

#### Responsibilities

1. **HTTPRoute Management**:
   - For `llmisvc` kind: Validates that the HTTPRoute exists (created by LLMInferenceService controller)
   - For `ExternalModel` kind: Creates and manages the HTTPRoute
   - Uses label-based lookup to find HTTPRoutes for `llmisvc` models:
     - `app.kubernetes.io/name: <llmisvc-name>`
     - `app.kubernetes.io/component: llminferenceservice-router`
     - `app.kubernetes.io/part-of: llminferenceservice`

2. **AuthPolicy Creation**:
   - Creates a base `AuthPolicy` (Kuadrant) for each model
   - Targets the HTTPRoute associated with the model
   - Uses `kubernetes-user` authentication with `kubernetesTokenReview`

3. **Status Updates**:
   - Updates model status with HTTPRoute information
   - Tracks model phase and endpoint

#### Key Features

- **Label-based HTTPRoute Discovery**: Instead of relying on naming conventions, the controller uses Kubernetes labels to find HTTPRoutes, making it more robust and flexible.
- **Cross-namespace Support**: Handles models and HTTPRoutes in different namespaces.
- **Validation for llmisvc**: For internal KServe models, the controller validates that the HTTPRoute exists rather than creating it, preventing conflicts with the LLMInferenceService controller.

### MaaSAuthPolicy Controller

The `MaaSAuthPolicyReconciler` manages the lifecycle of `MaaSAuthPolicy` resources.

#### Responsibilities

1. **AuthPolicy Creation**:
   - Creates one `AuthPolicy` (Kuadrant) per model referenced in `MaaSAuthPolicy.Spec.ModelRefs`
   - Each `AuthPolicy` targets the specific HTTPRoute of that model
   - Sets authorization rules based on `MaaSAuthPolicy.Spec.Subjects`:
     - Validates user groups using CEL expressions: `"group-name" in auth.identity.user.groups`
     - Validates usernames using CEL expressions: `auth.identity.user.username == "username"`

2. **Group and User Validation**:
   - Combines group and user checks with OR logic
   - Uses CEL (Common Expression Language) for dynamic predicate evaluation

3. **Metering Metadata**:
   - Propagates metering metadata as annotations on the created `AuthPolicy`

#### Key Features

- **Per-model AuthPolicies**: Creates separate `AuthPolicy` resources for each model, allowing fine-grained access control.
- **CEL-based Authorization**: Uses Kuadrant's CEL expressions for flexible authorization rules.
- **Kubernetes Token Authentication**: Authenticates users via Kubernetes Service Account tokens.

### MaaSSubscription Controller

The `MaaSSubscriptionReconciler` manages the lifecycle of `MaaSSubscription` resources.

#### Responsibilities

1. **TokenRateLimitPolicy Creation**:
   - Creates one `TokenRateLimitPolicy` (Kuadrant) per model in the subscription
   - Each policy targets the specific HTTPRoute of that model
   - Configures rate limits based on `MaaSSubscription.Spec.ModelRefs[].TokenRateLimits`

2. **Subscription Validation**:
   - Validates subscription ID: `auth.identity.subscriptionId == "{subscriptionName}"`
   - Excludes models endpoint: `!request.path.endsWith("/v1/models")`
   - Validates owner groups/users: `("group1" in auth.identity.user.groups || ...)`

3. **Billing Metadata**:
   - Propagates billing metadata as annotations on the created `TokenRateLimitPolicy`

#### Key Features

- **Per-model Rate Limiting**: Creates separate `TokenRateLimitPolicy` resources for each model, allowing different rate limits per model.
- **Subscription ID Validation**: Ensures requests are associated with the correct subscription.
- **Owner Validation**: Validates that requests come from the subscription owner (groups or users).
- **Models Endpoint Exclusion**: Prevents rate limiting from applying to the `/v1/models` endpoint.

## HTTPRoute Discovery

The controllers use label-based discovery to find HTTPRoutes for `llmisvc` models, which is more reliable than naming conventions. The HTTPRoutes created by the LLMInferenceService controller have the following labels:

- `app.kubernetes.io/name: <llmisvc-name>`
- `app.kubernetes.io/component: llminferenceservice-router`
- `app.kubernetes.io/part-of: llminferenceservice`

This approach:
- **Decouples from naming conventions**: No need to know the exact HTTPRoute name format
- **More flexible**: Works even if HTTPRoute naming changes
- **More reliable**: Uses Kubernetes-native label selectors

## Installation

### Prerequisites

- Kubernetes cluster with Gateway API and Kuadrant installed
- `kubectl` configured to access the cluster
- `make` and `go` installed (for building from source)

### Install Script

The PoC includes an installation script that automates the setup:

```bash
cd odh-model-controller/scripts
./install-maas-subscription.sh
```

The script:
1. Generates CRDs using `make manifests`
2. Applies CRDs to the cluster
3. Updates RBAC permissions (annotates ClusterRole to prevent operator overwrites)
4. Optionally updates the `odh-model-controller` deployment

### Manual Installation

1. **Generate CRDs**:
   ```bash
   cd odh-model-controller
   make manifests
   ```

2. **Apply CRDs**:
   ```bash
   kubectl apply -f config/crd/bases/maas.opendatahub.io_maasmodels.yaml
   kubectl apply -f config/crd/bases/maas.opendatahub.io_maasauthpolicies.yaml
   kubectl apply -f config/crd/bases/maas.opendatahub.io_maassubscriptions.yaml
   ```

3. **Update RBAC**:
   ```bash
   kubectl apply -f config/rbac/role.yaml
   # Annotate ClusterRole to prevent opendatahub-operator from overwriting
   kubectl annotate clusterrole odh-model-controller-role opendatahub.io/managed='false' --overwrite
   ```

4. **Build and Deploy Controller**:
   ```bash
   make container-build IMG=quay.io/maas/odh-model-controller:latest CONTAINER_TOOL=podman
   podman push quay.io/maas/odh-model-controller:latest
   kubectl set image deployment/odh-model-controller manager=quay.io/maas/odh-model-controller:latest -n opendatahub
   ```

## Usage Examples

### Creating a MaaSModel

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModel
metadata:
  name: my-model
  namespace: llm
spec:
  modelRef:
    kind: llmisvc
    name: my-llm-service
    namespace: llm
```

### Creating a MaaSAuthPolicy

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: data-scientists-access
  namespace: default
spec:
  modelRefs:
    - my-model
  subjects:
    groups:
      - name: data-scientists
    users:
      - alice
  meteringMetadata:
    organizationID: "org-123"
    costCenter: "data-science"
```

### Creating a MaaSSubscription

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: premium-subscription
  namespace: default
spec:
  modelRefs:
    - name: my-model
      tokenRateLimits:
        - limit: 1000
          window: 1m
        - limit: 10000
          window: 1h
  owner:
    users:
      - alice
    groups:
      - name: premium-users
  billingMetadata:
    organizationID: "org-123"
    costCenter: "engineering"
```

## Technical Details

### Authentication

All `AuthPolicy` resources created by the controllers use `kubernetes-user` authentication with `kubernetesTokenReview`. This ensures that:
- Users authenticate via Kubernetes Service Account tokens
- Token validation is handled by Kubernetes API server
- Integration with existing Kubernetes RBAC

### Authorization

Authorization rules use CEL (Common Expression Language) expressions to validate:
- User groups: `"group-name" in auth.identity.user.groups`
- Usernames: `auth.identity.user.username == "username"`
- Combined with OR logic for multiple groups/users

### Rate Limiting

`TokenRateLimitPolicy` resources include:
- **Subscription ID validation**: `auth.identity.subscriptionId == "{subscriptionName}"`
- **Models endpoint exclusion**: `!request.path.endsWith("/v1/models")`
- **Owner validation**: Checks that requests come from subscription owner
- **Per-user counters**: Uses `auth.identity.userid` as the counter expression

### Cross-Namespace Support

The controllers handle resources across namespaces:
- `MaaSModel` can reference models in different namespaces
- `AuthPolicy` and `TokenRateLimitPolicy` are created in the same namespace as the HTTPRoute
- Owner references are only set when resources are in the same namespace (Kubernetes limitation)

### Status Tracking

All CRDs include status fields that track:
- **Phase**: Current state (`Pending`, `Ready`, `Failed`, etc.)
- **Conditions**: Detailed conditions with timestamps and messages
- **HTTPRoute Information**: For `MaaSModel`, tracks the associated HTTPRoute name and namespace

## RBAC Considerations

The `opendatahub-operator` manages the `odh-model-controller` ClusterRole and may overwrite manual changes. To prevent this:

1. Annotate the ClusterRole: `opendatahub.io/managed='false'`
2. Annotate the ClusterRoleBinding: `opendatahub.io/managed='false'`

The install script handles this automatically.

## Troubleshooting

### Controller Not Watching CRDs

If you see errors like:
```
failed to list *v1alpha1.MaaSSubscription: maassubscriptions.maas.opendatahub.io is forbidden
```

1. Verify RBAC permissions are applied:
   ```bash
   kubectl get clusterrole odh-model-controller-role -o yaml | grep maas.opendatahub.io
   ```

2. Ensure ClusterRole is annotated to prevent overwrites:
   ```bash
   kubectl annotate clusterrole odh-model-controller-role opendatahub.io/managed='false' --overwrite
   ```

3. Restart the controller pod:
   ```bash
   kubectl rollout restart deployment odh-model-controller -n opendatahub
   ```

### HTTPRoute Not Found

If you see errors about HTTPRoute not being found:

1. For `llmisvc` models, verify the HTTPRoute exists:
   ```bash
   kubectl get httproute -n <namespace> -l app.kubernetes.io/name=<llmisvc-name>
   ```

2. Check that the LLMInferenceService controller has created the HTTPRoute

3. Verify labels match:
   - `app.kubernetes.io/name: <llmisvc-name>`
   - `app.kubernetes.io/component: llminferenceservice-router`
   - `app.kubernetes.io/part-of: llminferenceservice`

### AuthPolicy Validation Errors

If `AuthPolicy` creation fails with validation errors:

1. Verify the authentication method is correct (should be `kubernetes-user` with `kubernetesTokenReview`)
2. Check that the `targetRef` points to a valid HTTPRoute
3. Ensure the HTTPRoute exists in the target namespace

## Future Enhancements

Potential improvements for future iterations:

1. **Webhook Validation**: Add admission webhooks to validate CRD specs before creation
2. **Status Conditions**: More detailed status conditions for better observability
3. **Metrics**: Prometheus metrics for reconciliation counts, errors, etc.
4. **Finalizers**: Proper cleanup of resources using finalizers
5. **Multi-cluster Support**: Support for models across multiple clusters
6. **Subscription Lifecycle**: Automatic subscription expiration and renewal
7. **Usage Tracking**: Integration with billing systems for usage tracking

## References

- [MaaS Subscription Model v2](../MaaS%20Subscription%20Model%20v2.md)
- [Kuadrant Documentation](https://docs.kuadrant.io/)
- [Gateway API Specification](https://gateway-api.sigs.k8s.io/)
- [Kubebuilder Documentation](https://book.kubebuilder.io/)
