/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package maas

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/odh-model-controller/api/maas/v1alpha1"
)

// MaaSSubscriptionReconciler reconciles a MaaSSubscription object
type MaaSSubscriptionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maassubscriptions,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maassubscriptions/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maassubscriptions/finalizers,verbs=update
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasmodels,verbs=get;list;watch
//+kubebuilder:rbac:groups=kuadrant.io,resources=tokenratelimitpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop
func (r *MaaSSubscriptionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logr.FromContextOrDiscard(ctx).WithValues("MaaSSubscription", req.NamespacedName)

	subscription := &maasv1alpha1.MaaSSubscription{}
	if err := r.Get(ctx, req.NamespacedName, subscription); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch MaaSSubscription")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !subscription.GetDeletionTimestamp().IsZero() {
		return r.handleDeletion(ctx, log, subscription)
	}

	// Reconcile TokenRateLimitPolicy for each model
	// IMPORTANT: TokenRateLimitPolicy targets the HTTPRoute for each model
	if err := r.reconcileTokenRateLimitPolicies(ctx, log, subscription); err != nil {
		log.Error(err, "failed to reconcile TokenRateLimitPolicies")
		r.updateStatus(ctx, subscription, "Failed", fmt.Sprintf("Failed to reconcile: %v", err))
		return ctrl.Result{}, err
	}

	r.updateStatus(ctx, subscription, "Active", "Successfully reconciled")
	return ctrl.Result{}, nil
}

func (r *MaaSSubscriptionReconciler) reconcileTokenRateLimitPolicies(ctx context.Context, log logr.Logger, subscription *maasv1alpha1.MaaSSubscription) error {
	// Create one TokenRateLimitPolicy per model in the subscription
	// Each policy targets the HTTPRoute for that specific model
	for _, modelRef := range subscription.Spec.ModelRefs {
		// Find the MaaSModel to determine HTTPRoute name and namespace
		httpRouteName, httpRouteNS, err := r.findHTTPRouteForModel(ctx, log, subscription.Namespace, modelRef.Name)
		if err != nil {
			log.Error(err, "failed to find HTTPRoute for model", "model", modelRef.Name)
			return fmt.Errorf("failed to find HTTPRoute for model %s: %w", modelRef.Name, err)
		}

		policyName := fmt.Sprintf("subscription-%s-model-%s", subscription.Name, modelRef.Name)

		// Use unstructured for TokenRateLimitPolicy since Go types may not be available
		policy := &unstructured.Unstructured{}
		policy.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "kuadrant.io",
			Version: "v1alpha1",
			Kind:    "TokenRateLimitPolicy",
		})
		policy.SetName(policyName)
		// Use the same namespace as the HTTPRoute
		policy.SetNamespace(httpRouteNS)

		// Set owner reference (only if in same namespace)
		if httpRouteNS == subscription.Namespace {
			if err := controllerutil.SetControllerReference(subscription, policy, r.Scheme); err != nil {
				return fmt.Errorf("failed to set controller reference: %w", err)
			}
		}

		// Build limits from subscription spec
		limitKey := fmt.Sprintf("%s-%s-tokens", subscription.Name, modelRef.Name)

		// Convert token rate limits to []interface{}
		var rates []interface{}
		for _, trl := range modelRef.TokenRateLimits {
			rates = append(rates, map[string]interface{}{
				"limit":  trl.Limit,
				"window": trl.Window,
			})
		}

		// Build predicate: check subscription ID, user groups, and exclude models endpoint
		// Format: auth.identity.subscriptionId == "{subscriptionName}" && !request.path.endsWith("/v1/models") && (group check)

		// Build predicate: check subscription ID, user groups, and exclude models endpoint
		// Format: auth.identity.subscriptionId == "{subscriptionName}" && !request.path.endsWith("/v1/models") && (group check)

		// Build group validation predicate - check if user is in any of the subscription owner groups
		// Groups are available as auth.identity.user.groups from kubernetesTokenReview
		var groupChecks []string
		for _, group := range subscription.Spec.Owner.Groups {
			// Check if user's groups array contains this group name
			// Using CEL expression: check if group name exists in auth.identity.user.groups array
			groupChecks = append(groupChecks, fmt.Sprintf(`"%s" in auth.identity.user.groups`, group.Name))
		}

		// Build user validation predicate
		var userChecks []string
		for _, user := range subscription.Spec.Owner.Users {
			// User ID is available as auth.identity.user.username from kubernetesTokenReview
			userChecks = append(userChecks, fmt.Sprintf(`auth.identity.user.username == "%s"`, user))
		}

		// Combine all checks with OR logic (user must match subscription owner)
		var ownerChecks []string
		ownerChecks = append(ownerChecks, groupChecks...)
		ownerChecks = append(ownerChecks, userChecks...)

		var predicate string
		if len(ownerChecks) > 0 {
			// User must be owner AND have correct subscription ID AND not be hitting models endpoint
			ownerPredicate := "(" + ownerChecks[0]
			for i := 1; i < len(ownerChecks); i++ {
				ownerPredicate += " || " + ownerChecks[i]
			}
			ownerPredicate += ")"
			predicate = fmt.Sprintf(`auth.identity.subscriptionId == "%s" && !request.path.endsWith("/v1/models") && %s`, subscription.Name, ownerPredicate)
		} else {
			// If no owner specified, just check subscription ID and models endpoint
			predicate = fmt.Sprintf(`auth.identity.subscriptionId == "%s" && !request.path.endsWith("/v1/models")`, subscription.Name)
		}

		// Build the spec - target the HTTPRoute
		spec := map[string]interface{}{
			"targetRef": map[string]interface{}{
				"group": "gateway.networking.k8s.io",
				"kind":  "HTTPRoute",
				"name":  httpRouteName,
			},
			"limits": map[string]interface{}{
				limitKey: map[string]interface{}{
					"rates": rates,
					"when": []interface{}{
						map[string]interface{}{
							"predicate": predicate,
						},
					},
					"counters": []interface{}{
						map[string]interface{}{
							"expression": "auth.identity.userid",
						},
					},
				},
			},
		}

		if err := unstructured.SetNestedMap(policy.Object, spec, "spec"); err != nil {
			return fmt.Errorf("failed to set spec: %w", err)
		}

		// Add billing metadata as annotations
		if subscription.Spec.BillingMetadata != nil {
			annotations := make(map[string]string)
			if subscription.Spec.BillingMetadata.OrganizationID != "" {
				annotations["maas.opendatahub.io/organization-id"] = subscription.Spec.BillingMetadata.OrganizationID
			}
			if subscription.Spec.BillingMetadata.CostCenter != "" {
				annotations["maas.opendatahub.io/cost-center"] = subscription.Spec.BillingMetadata.CostCenter
			}
			for k, v := range subscription.Spec.BillingMetadata.Labels {
				annotations[fmt.Sprintf("maas.opendatahub.io/label/%s", k)] = v
			}
			policy.SetAnnotations(annotations)
		}

		// Create or update the policy
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(policy.GroupVersionKind())
		key := client.ObjectKeyFromObject(policy)

		err = r.Get(ctx, key, existing)
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, policy); err != nil {
				return fmt.Errorf("failed to create TokenRateLimitPolicy for model %s: %w", modelRef.Name, err)
			}
			log.Info("TokenRateLimitPolicy created", "name", policyName, "model", modelRef.Name, "httpRoute", httpRouteName, "namespace", httpRouteNS)
		} else if err != nil {
			return fmt.Errorf("failed to get existing TokenRateLimitPolicy: %w", err)
		} else {
			// Update existing
			existing.SetAnnotations(policy.GetAnnotations())
			if err := unstructured.SetNestedMap(existing.Object, spec, "spec"); err != nil {
				return fmt.Errorf("failed to update spec: %w", err)
			}
			if err := r.Update(ctx, existing); err != nil {
				return fmt.Errorf("failed to update TokenRateLimitPolicy for model %s: %w", modelRef.Name, err)
			}
			log.Info("TokenRateLimitPolicy updated", "name", policyName, "model", modelRef.Name, "httpRoute", httpRouteName, "namespace", httpRouteNS)
		}
	}

	return nil
}

// findHTTPRouteForModel finds the HTTPRoute for a given model name
// It searches for MaaSModel resources and determines the HTTPRoute name based on the model kind
func (r *MaaSSubscriptionReconciler) findHTTPRouteForModel(ctx context.Context, log logr.Logger, defaultNS, modelName string) (string, string, error) {
	// List all MaaSModels and find the one with matching name
	maasModelList := &maasv1alpha1.MaaSModelList{}
	if err := r.List(ctx, maasModelList); err != nil {
		return "", "", fmt.Errorf("failed to list MaaSModels: %w", err)
	}

	// Find matching MaaSModel (try defaultNS first, then any namespace)
	var maasModel *maasv1alpha1.MaaSModel
	for i := range maasModelList.Items {
		if maasModelList.Items[i].Name == modelName {
			// Prefer the one in defaultNS if it exists
			if maasModelList.Items[i].Namespace == defaultNS {
				maasModel = &maasModelList.Items[i]
				break
			}
			// Otherwise, use the first match
			if maasModel == nil {
				maasModel = &maasModelList.Items[i]
			}
		}
	}

	if maasModel == nil {
		return "", "", fmt.Errorf("MaaSModel %s not found", modelName)
	}

	// Determine HTTPRoute name and namespace based on model kind
	var httpRouteName string
	httpRouteNS := maasModel.Namespace
	if maasModel.Spec.ModelRef.Namespace != "" {
		httpRouteNS = maasModel.Spec.ModelRef.Namespace
	}

	switch maasModel.Spec.ModelRef.Kind {
	case "llmisvc":
		// For llmisvc, find HTTPRoute using labels
		routeList := &gatewayapiv1.HTTPRouteList{}
		labelSelector := client.MatchingLabels{
			"app.kubernetes.io/name":      maasModel.Spec.ModelRef.Name,
			"app.kubernetes.io/component": "llminferenceservice-router",
			"app.kubernetes.io/part-of":   "llminferenceservice",
		}

		if err := r.List(ctx, routeList, client.InNamespace(httpRouteNS), labelSelector); err != nil {
			return "", "", fmt.Errorf("failed to list HTTPRoutes for LLMInferenceService %s: %w", maasModel.Spec.ModelRef.Name, err)
		}

		if len(routeList.Items) == 0 {
			return "", "", fmt.Errorf("HTTPRoute not found for LLMInferenceService %s in namespace %s", maasModel.Spec.ModelRef.Name, httpRouteNS)
		}

		httpRouteName = routeList.Items[0].Name
	case "ExternalModel":
		// For ExternalModel, use the MaaSModel HTTPRoute naming convention
		httpRouteName = fmt.Sprintf("maas-model-%s", maasModel.Name)
	default:
		return "", "", fmt.Errorf("unknown model kind: %s", maasModel.Spec.ModelRef.Kind)
	}

	// Verify the HTTPRoute exists
	httpRoute := &gatewayapiv1.HTTPRoute{}
	key := client.ObjectKey{
		Name:      httpRouteName,
		Namespace: httpRouteNS,
	}
	if err := r.Get(ctx, key, httpRoute); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", fmt.Errorf("HTTPRoute %s/%s not found for model %s", httpRouteNS, httpRouteName, modelName)
		}
		return "", "", fmt.Errorf("failed to get HTTPRoute %s/%s: %w", httpRouteNS, httpRouteName, err)
	}

	return httpRouteName, httpRouteNS, nil
}

func (r *MaaSSubscriptionReconciler) handleDeletion(ctx context.Context, log logr.Logger, subscription *maasv1alpha1.MaaSSubscription) (ctrl.Result, error) {
	// Clean up all TokenRateLimitPolicies for this subscription
	for _, modelRef := range subscription.Spec.ModelRefs {
		// Try to find the HTTPRoute to determine the correct namespace
		// If we can't find it, try the subscription namespace as fallback
		_, httpRouteNS, err := r.findHTTPRouteForModel(ctx, log, subscription.Namespace, modelRef.Name)
		if err != nil {
			log.Info("failed to find HTTPRoute for model during deletion, trying subscription namespace", "model", modelRef.Name, "error", err)
			httpRouteNS = subscription.Namespace
		}

		policyName := fmt.Sprintf("subscription-%s-model-%s", subscription.Name, modelRef.Name)
		policy := &unstructured.Unstructured{}
		policy.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "kuadrant.io",
			Version: "v1alpha1",
			Kind:    "TokenRateLimitPolicy",
		})
		policy.SetName(policyName)
		policy.SetNamespace(httpRouteNS)

		if err := r.Delete(ctx, policy); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "failed to delete TokenRateLimitPolicy", "name", policyName, "namespace", httpRouteNS)
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *MaaSSubscriptionReconciler) updateStatus(ctx context.Context, subscription *maasv1alpha1.MaaSSubscription, phase, message string) {
	subscription.Status.Phase = phase
	condition := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}
	if phase == "Failed" {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "ReconcileFailed"
	}

	// Update condition
	found := false
	for i, c := range subscription.Status.Conditions {
		if c.Type == condition.Type {
			subscription.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		subscription.Status.Conditions = append(subscription.Status.Conditions, condition)
	}

	r.Status().Update(ctx, subscription)
}

// SetupWithManager sets up the controller with the Manager.
func (r *MaaSSubscriptionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&maasv1alpha1.MaaSSubscription{}).
		Complete(r)
}
