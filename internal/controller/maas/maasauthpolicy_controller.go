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

// MaaSAuthPolicyReconciler reconciles a MaaSAuthPolicy object
type MaaSAuthPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasauthpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasauthpolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasauthpolicies/finalizers,verbs=update
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasmodels,verbs=get;list;watch
//+kubebuilder:rbac:groups=kuadrant.io,resources=authpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop
func (r *MaaSAuthPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logr.FromContextOrDiscard(ctx).WithValues("MaaSAuthPolicy", req.NamespacedName)

	policy := &maasv1alpha1.MaaSAuthPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch MaaSAuthPolicy")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !policy.GetDeletionTimestamp().IsZero() {
		return r.handleDeletion(ctx, log, policy)
	}

	// Reconcile AuthPolicy for each model in ModelRefs
	// Each AuthPolicy targets the HTTPRoute associated with the MaaSModel
	if err := r.reconcileModelAuthPolicies(ctx, log, policy); err != nil {
		log.Error(err, "failed to reconcile model AuthPolicies")
		r.updateStatus(ctx, policy, "Failed", fmt.Sprintf("Failed to reconcile: %v", err))
		return ctrl.Result{}, err
	}

	r.updateStatus(ctx, policy, "Active", "Successfully reconciled")
	return ctrl.Result{}, nil
}

func (r *MaaSAuthPolicyReconciler) reconcileModelAuthPolicies(ctx context.Context, log logr.Logger, policy *maasv1alpha1.MaaSAuthPolicy) error {
	// Create one AuthPolicy per model in ModelRefs
	// Each AuthPolicy targets the HTTPRoute associated with the MaaSModel
	for _, modelName := range policy.Spec.ModelRefs {
		// Find the MaaSModel to determine HTTPRoute name and namespace
		httpRouteName, httpRouteNS, err := r.findHTTPRouteForModel(ctx, log, policy.Namespace, modelName)
		if err != nil {
			log.Error(err, "failed to find HTTPRoute for model", "model", modelName)
			return fmt.Errorf("failed to find HTTPRoute for model %s: %w", modelName, err)
		}

		authPolicyName := fmt.Sprintf("maas-auth-%s-model-%s", policy.Name, modelName)
		
		// Use unstructured for AuthPolicy to match the YAML structure
		authPolicy := &unstructured.Unstructured{}
		authPolicy.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "kuadrant.io",
			Version: "v1",
			Kind:    "AuthPolicy",
		})
		authPolicy.SetName(authPolicyName)
		// Use the same namespace as the HTTPRoute
		authPolicy.SetNamespace(httpRouteNS)

		// Set owner reference (only if in same namespace)
		if httpRouteNS == policy.Namespace {
			if err := controllerutil.SetControllerReference(policy, authPolicy, r.Scheme); err != nil {
				return fmt.Errorf("failed to set controller reference: %w", err)
			}
		}

		// Build subjects for authorization
		var groupNames []string
		for _, group := range policy.Spec.Subjects.Groups {
			groupNames = append(groupNames, group.Name)
		}

		// Build authorization rules that check groups
		// Groups are available as auth.identity.user.groups from kubernetesTokenReview
		var whenConditions []interface{}
		if len(groupNames) > 0 {
			// Add group check - user must be in one of the specified groups
			// Using CEL expression to check if any group matches
			for _, groupName := range groupNames {
				whenConditions = append(whenConditions, map[string]interface{}{
					"selector": "auth.identity.user.groups",
					"operator": "contains",
					"value":    groupName,
				})
			}
		}

		// Add user checks if specified
		var userConditions []interface{}
		for _, user := range policy.Spec.Subjects.Users {
			userConditions = append(userConditions, map[string]interface{}{
				"selector": "auth.identity.user.username",
				"operator": "eq",
				"value":    user,
			})
		}

		// Combine all conditions with OR logic (any match grants access)
		var allConditions []interface{}
		allConditions = append(allConditions, whenConditions...)
		allConditions = append(allConditions, userConditions...)

		// Build the spec matching the AuthPolicy YAML structure
		rule := map[string]interface{}{
			"authentication": map[string]interface{}{
				"kubernetes-user": map[string]interface{}{
					"kubernetesTokenReview": map[string]interface{}{},
				},
			},
		}

		// Add authorization if we have conditions
		if len(allConditions) > 0 {
			rule["authorization"] = map[string]interface{}{
				"maas-access": map[string]interface{}{
					"when": allConditions,
				},
			}
		}

		spec := map[string]interface{}{
			"targetRef": map[string]interface{}{
				"group": "gateway.networking.k8s.io",
				"kind":  "HTTPRoute",
				"name":  httpRouteName,
			},
			"rules": []interface{}{rule},
		}

		if err := unstructured.SetNestedMap(authPolicy.Object, spec, "spec"); err != nil {
			return fmt.Errorf("failed to set spec: %w", err)
		}

		// Add metering metadata as annotations if present
		if policy.Spec.MeteringMetadata != nil {
			annotations := make(map[string]string)
			if policy.Spec.MeteringMetadata.OrganizationID != "" {
				annotations["maas.opendatahub.io/organization-id"] = policy.Spec.MeteringMetadata.OrganizationID
			}
			if policy.Spec.MeteringMetadata.CostCenter != "" {
				annotations["maas.opendatahub.io/cost-center"] = policy.Spec.MeteringMetadata.CostCenter
			}
			for k, v := range policy.Spec.MeteringMetadata.Labels {
				annotations[fmt.Sprintf("maas.opendatahub.io/label/%s", k)] = v
			}
			authPolicy.SetAnnotations(annotations)
		}

		// Create or update the policy
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(authPolicy.GroupVersionKind())
		key := client.ObjectKeyFromObject(authPolicy)
		
		err = r.Get(ctx, key, existing)
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, authPolicy); err != nil {
				return fmt.Errorf("failed to create AuthPolicy for model %s: %w", modelName, err)
			}
			log.Info("AuthPolicy created", "name", authPolicyName, "model", modelName, "httpRoute", httpRouteName, "namespace", httpRouteNS)
		} else if err != nil {
			return fmt.Errorf("failed to get existing AuthPolicy: %w", err)
		} else {
			// Update existing
			existing.SetAnnotations(authPolicy.GetAnnotations())
			if err := unstructured.SetNestedMap(existing.Object, spec, "spec"); err != nil {
				return fmt.Errorf("failed to update spec: %w", err)
			}
			if err := r.Update(ctx, existing); err != nil {
				return fmt.Errorf("failed to update AuthPolicy for model %s: %w", modelName, err)
			}
			log.Info("AuthPolicy updated", "name", authPolicyName, "model", modelName, "httpRoute", httpRouteName, "namespace", httpRouteNS)
		}
	}

	return nil
}

// findHTTPRouteForModel finds the HTTPRoute for a given model name
// It searches for MaaSModel resources and determines the HTTPRoute name based on the model kind
func (r *MaaSAuthPolicyReconciler) findHTTPRouteForModel(ctx context.Context, log logr.Logger, defaultNS, modelName string) (string, string, error) {
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

func (r *MaaSAuthPolicyReconciler) handleDeletion(ctx context.Context, log logr.Logger, policy *maasv1alpha1.MaaSAuthPolicy) (ctrl.Result, error) {
	// Clean up all AuthPolicies for each model
	for _, modelName := range policy.Spec.ModelRefs {
		// Try to find the HTTPRoute to determine the correct namespace
		// If we can't find it, try the policy namespace as fallback
		_, httpRouteNS, err := r.findHTTPRouteForModel(ctx, log, policy.Namespace, modelName)
		if err != nil {
			log.Info("failed to find HTTPRoute for model during deletion, trying policy namespace", "model", modelName, "error", err)
			httpRouteNS = policy.Namespace
		}

		authPolicyName := fmt.Sprintf("maas-auth-%s-model-%s", policy.Name, modelName)
		authPolicy := &unstructured.Unstructured{}
		authPolicy.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "kuadrant.io",
			Version: "v1",
			Kind:    "AuthPolicy",
		})
		authPolicy.SetName(authPolicyName)
		authPolicy.SetNamespace(httpRouteNS)

		if err := r.Delete(ctx, authPolicy); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "failed to delete AuthPolicy", "name", authPolicyName, "namespace", httpRouteNS)
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *MaaSAuthPolicyReconciler) updateStatus(ctx context.Context, policy *maasv1alpha1.MaaSAuthPolicy, phase, message string) {
	policy.Status.Phase = phase
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
	for i, c := range policy.Status.Conditions {
		if c.Type == condition.Type {
			policy.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		policy.Status.Conditions = append(policy.Status.Conditions, condition)
	}

	r.Status().Update(ctx, policy)
}

// SetupWithManager sets up the controller with the Manager.
func (r *MaaSAuthPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&maasv1alpha1.MaaSAuthPolicy{}).
		Complete(r)
}
