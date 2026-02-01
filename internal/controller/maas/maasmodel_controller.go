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
	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
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

// MaaSModelReconciler reconciles a MaaSModel object
type MaaSModelReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasmodels,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasmodels/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasmodels/finalizers,verbs=update
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kuadrant.io,resources=authpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=serving.kserve.io,resources=llminferenceservices,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop
func (r *MaaSModelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logr.FromContextOrDiscard(ctx).WithValues("MaaSModel", req.NamespacedName)

	model := &maasv1alpha1.MaaSModel{}
	if err := r.Get(ctx, req.NamespacedName, model); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch MaaSModel")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !model.GetDeletionTimestamp().IsZero() {
		return r.handleDeletion(ctx, log, model)
	}

	// Reconcile HTTPRoute (only for ExternalModel, validate for llmisvc)
	if err := r.reconcileHTTPRoute(ctx, log, model); err != nil {
		log.Error(err, "failed to reconcile HTTPRoute")
		r.updateStatus(ctx, model, "Failed", fmt.Sprintf("Failed to reconcile HTTPRoute: %v", err))
		return ctrl.Result{}, err
	}

	// Reconcile AuthPolicy for the model
	if err := r.reconcileModelAuthPolicy(ctx, log, model); err != nil {
		log.Error(err, "failed to reconcile AuthPolicy")
		r.updateStatus(ctx, model, "Failed", fmt.Sprintf("Failed to reconcile AuthPolicy: %v", err))
		return ctrl.Result{}, err
	}

	// Update status based on referenced model
	if err := r.updateModelStatus(ctx, log, model); err != nil {
		log.Error(err, "failed to update model status")
		// Don't fail reconciliation on status update errors
	}

	// Update status - this will preserve HTTPRouteName and HTTPRouteNamespace set earlier
	r.updateStatus(ctx, model, "Ready", "Successfully reconciled")
	return ctrl.Result{}, nil
}

func (r *MaaSModelReconciler) reconcileHTTPRoute(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) error {
	// For llmisvc kind, only validate that the HTTPRoute exists (don't create/override)
	if model.Spec.ModelRef.Kind == "llmisvc" {
		return r.validateLLMISvcHTTPRoute(ctx, log, model)
	}

	// For ExternalModel, create/update the HTTPRoute
	return r.createOrUpdateHTTPRoute(ctx, log, model)
}

func (r *MaaSModelReconciler) validateLLMISvcHTTPRoute(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) error {
	// Determine namespace for HTTPRoute (same as LLMInferenceService)
	routeNS := model.Namespace
	if model.Spec.ModelRef.Namespace != "" {
		routeNS = model.Spec.ModelRef.Namespace
	}

	// Find HTTPRoute using labels instead of naming convention
	// HTTPRoutes for LLMInferenceService have these labels:
	// - app.kubernetes.io/name: <llmisvc-name>
	// - app.kubernetes.io/component: llminferenceservice-router
	// - app.kubernetes.io/part-of: llminferenceservice
	routeList := &gatewayapiv1.HTTPRouteList{}
	labelSelector := client.MatchingLabels{
		"app.kubernetes.io/name":      model.Spec.ModelRef.Name,
		"app.kubernetes.io/component": "llminferenceservice-router",
		"app.kubernetes.io/part-of":   "llminferenceservice",
	}

	if err := r.List(ctx, routeList, client.InNamespace(routeNS), labelSelector); err != nil {
		return fmt.Errorf("failed to list HTTPRoutes for LLMInferenceService %s: %w", model.Spec.ModelRef.Name, err)
	}

	if len(routeList.Items) == 0 {
		log.Error(nil, "HTTPRoute not found for LLMInferenceService", "llmisvcName", model.Spec.ModelRef.Name, "namespace", routeNS)
		return fmt.Errorf("HTTPRoute not found for LLMInferenceService %s in namespace %s - it should be created by the LLMInferenceService controller", model.Spec.ModelRef.Name, routeNS)
	}

	// Use the first matching HTTPRoute
	route := &routeList.Items[0]
	routeName := route.Name

	log.Info("HTTPRoute validated for LLMInferenceService", "routeName", routeName, "namespace", routeNS, "llmisvcName", model.Spec.ModelRef.Name)

	// Update status with HTTPRoute information
	model.Status.HTTPRouteName = routeName
	model.Status.HTTPRouteNamespace = routeNS

	return nil
}

func (r *MaaSModelReconciler) createOrUpdateHTTPRoute(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) error {
	routeName := fmt.Sprintf("maas-model-%s", model.Name)
	route := &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeName,
			Namespace: model.Namespace,
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, route, func() error {
		// Set owner reference
		if err := controllerutil.SetControllerReference(model, route, r.Scheme); err != nil {
			return err
		}

		// Determine namespace for backend service
		backendNS := model.Namespace
		if model.Spec.ModelRef.Namespace != "" {
			backendNS = model.Spec.ModelRef.Namespace
		}

		// Build HTTPRoute spec
		// This routes requests to the model endpoint
		route.Spec = gatewayapiv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayapiv1.CommonRouteSpec{
				ParentRefs: []gatewayapiv1.ParentReference{
					{
						Name:      gatewayapiv1.ObjectName("maas-default-gateway"),
						Namespace: (*gatewayapiv1.Namespace)(&model.Namespace),
					},
				},
			},
			Hostnames: []gatewayapiv1.Hostname{
				"maas.*", // Match any hostname under maas domain
			},
			Rules: []gatewayapiv1.HTTPRouteRule{
				{
					Matches: []gatewayapiv1.HTTPRouteMatch{
						{
							Path: &gatewayapiv1.HTTPPathMatch{
								Type:  ptr(gatewayapiv1.PathMatchPathPrefix),
								Value: ptr(fmt.Sprintf("/v1/models/%s", model.Name)),
							},
						},
					},
					BackendRefs: []gatewayapiv1.HTTPBackendRef{
						{
							BackendRef: gatewayapiv1.BackendRef{
								BackendObjectReference: gatewayapiv1.BackendObjectReference{
									Group: ptr(gatewayapiv1.Group("")),
									Kind:  ptr(gatewayapiv1.Kind("Service")),
									Name:  gatewayapiv1.ObjectName(model.Spec.ModelRef.Name),
									Namespace: func() *gatewayapiv1.Namespace {
										ns := gatewayapiv1.Namespace(backendNS)
										return &ns
									}(),
									Port: ptr(gatewayapiv1.PortNumber(8080)),
								},
							},
						},
					},
				},
			},
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to create/update HTTPRoute: %w", err)
	}

	if op != controllerutil.OperationResultNone {
		log.Info("HTTPRoute reconciled", "operation", op, "name", routeName)
	}

	// Update status with HTTPRoute information
	model.Status.HTTPRouteName = routeName
	model.Status.HTTPRouteNamespace = model.Namespace

	return nil
}

func (r *MaaSModelReconciler) reconcileModelAuthPolicy(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) error {
	// Create AuthPolicy that validates the token has subscription access to this model
	authPolicyName := fmt.Sprintf("maas-model-auth-%s", model.Name)

	// Use unstructured for AuthPolicy
	authPolicy := &unstructured.Unstructured{}
	authPolicy.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "kuadrant.io",
		Version: "v1",
		Kind:    "AuthPolicy",
	})
	// Determine HTTPRoute name and namespace
	// For llmisvc, find the HTTPRoute using labels
	// For ExternalModel, use the MaaS model route name
	var routeName string
	routeNS := model.Namespace
	if model.Spec.ModelRef.Kind == "llmisvc" {
		// Find HTTPRoute using labels
		if model.Spec.ModelRef.Namespace != "" {
			routeNS = model.Spec.ModelRef.Namespace
		}

		routeList := &gatewayapiv1.HTTPRouteList{}
		labelSelector := client.MatchingLabels{
			"app.kubernetes.io/name":      model.Spec.ModelRef.Name,
			"app.kubernetes.io/component": "llminferenceservice-router",
			"app.kubernetes.io/part-of":   "llminferenceservice",
		}

		if err := r.List(ctx, routeList, client.InNamespace(routeNS), labelSelector); err != nil {
			return fmt.Errorf("failed to list HTTPRoutes for LLMInferenceService %s: %w", model.Spec.ModelRef.Name, err)
		}

		if len(routeList.Items) == 0 {
			return fmt.Errorf("HTTPRoute not found for LLMInferenceService %s in namespace %s", model.Spec.ModelRef.Name, routeNS)
		}

		routeName = routeList.Items[0].Name
	} else {
		routeName = fmt.Sprintf("maas-model-%s", model.Name)
	}

	authPolicy.SetName(authPolicyName)
	// Use the same namespace as the HTTPRoute
	authPolicy.SetNamespace(routeNS)

	// Set owner reference
	if err := controllerutil.SetControllerReference(model, authPolicy, r.Scheme); err != nil {
		return fmt.Errorf("failed to set controller reference: %w", err)
	}

	// Target the HTTPRoute for this model
	// Use kubernetesTokenReview for authentication (same as other AuthPolicies)
	spec := map[string]interface{}{
		"targetRef": map[string]interface{}{
			"group": "gateway.networking.k8s.io",
			"kind":  "HTTPRoute",
			"name":  routeName,
		},
		"rules": map[string]interface{}{
			"authentication": map[string]interface{}{
				"kubernetes-user": map[string]interface{}{
					"kubernetesTokenReview": map[string]interface{}{},
				},
			},
		},
	}

	if err := unstructured.SetNestedMap(authPolicy.Object, spec, "spec"); err != nil {
		return fmt.Errorf("failed to set spec: %w", err)
	}

	// Create or update the policy
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(authPolicy.GroupVersionKind())
	key := client.ObjectKeyFromObject(authPolicy)

	err := r.Get(ctx, key, existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, authPolicy); err != nil {
			return fmt.Errorf("failed to create AuthPolicy: %w", err)
		}
		log.Info("AuthPolicy created", "name", authPolicyName)
	} else if err != nil {
		return fmt.Errorf("failed to get existing AuthPolicy: %w", err)
	} else {
		// Update existing
		if err := unstructured.SetNestedMap(existing.Object, spec, "spec"); err != nil {
			return fmt.Errorf("failed to update spec: %w", err)
		}
		if err := r.Update(ctx, existing); err != nil {
			return fmt.Errorf("failed to update AuthPolicy: %w", err)
		}
		log.Info("AuthPolicy updated", "name", authPolicyName)
	}

	return nil
}

func (r *MaaSModelReconciler) updateModelStatus(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) error {
	// For internal models (llmisvc), check the status of the referenced LLMInferenceService
	if model.Spec.ModelRef.Kind == "llmisvc" {
		llmisvcNS := model.Namespace
		if model.Spec.ModelRef.Namespace != "" {
			llmisvcNS = model.Spec.ModelRef.Namespace
		}

		llmisvc := &kservev1alpha1.LLMInferenceService{}
		key := client.ObjectKey{
			Name:      model.Spec.ModelRef.Name,
			Namespace: llmisvcNS,
		}

		if err := r.Get(ctx, key, llmisvc); err != nil {
			if apierrors.IsNotFound(err) {
				model.Status.Phase = "Failed"
				model.Status.Endpoint = ""
				return nil
			}
			return err
		}

		// Validate HTTPRoute exists (already checked in reconcileHTTPRoute, but double-check here)
		routeList := &gatewayapiv1.HTTPRouteList{}
		labelSelector := client.MatchingLabels{
			"app.kubernetes.io/name":      model.Spec.ModelRef.Name,
			"app.kubernetes.io/component": "llminferenceservice-router",
			"app.kubernetes.io/part-of":   "llminferenceservice",
		}

		if err := r.List(ctx, routeList, client.InNamespace(llmisvcNS), labelSelector); err != nil {
			return err
		}

		if len(routeList.Items) == 0 {
			model.Status.Phase = "Failed"
			model.Status.Endpoint = ""
			return nil
		}

		// Check if LLMInferenceService is ready
		// Check status conditions to determine readiness
		ready := false
		for _, condition := range llmisvc.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true
				break
			}
		}

		if ready {
			model.Status.Endpoint = fmt.Sprintf("https://maas.%s/v1/models/%s", "cluster.local", model.Name)
			model.Status.Phase = "Ready"
		} else {
			model.Status.Phase = "Pending"
			model.Status.Endpoint = ""
		}
	}

	// For external models, we would perform health checks
	if model.Spec.ModelRef.Kind == "ExternalModel" {
		// External model health check logic would go here
		model.Status.Phase = "Ready"
	}

	return nil
}

func (r *MaaSModelReconciler) handleDeletion(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) (ctrl.Result, error) {
	// Only clean up HTTPRoute for ExternalModel (llmisvc HTTPRoutes are managed by LLMInferenceService controller)
	if model.Spec.ModelRef.Kind != "llmisvc" {
		routeName := fmt.Sprintf("maas-model-%s", model.Name)
		route := &gatewayapiv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:      routeName,
				Namespace: model.Namespace,
			},
		}

		if err := r.Delete(ctx, route); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "failed to delete HTTPRoute")
			return ctrl.Result{}, err
		}
	}

	// Clean up AuthPolicy
	authPolicyName := fmt.Sprintf("maas-model-auth-%s", model.Name)
	authPolicy := &unstructured.Unstructured{}
	authPolicy.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "kuadrant.io",
		Version: "v1",
		Kind:    "AuthPolicy",
	})
	authPolicy.SetName(authPolicyName)
	authPolicy.SetNamespace(model.Namespace)

	if err := r.Delete(ctx, authPolicy); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "failed to delete AuthPolicy")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *MaaSModelReconciler) updateStatus(ctx context.Context, model *maasv1alpha1.MaaSModel, phase, message string) {
	model.Status.Phase = phase
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
	for i, c := range model.Status.Conditions {
		if c.Type == condition.Type {
			model.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		model.Status.Conditions = append(model.Status.Conditions, condition)
	}

	r.Status().Update(ctx, model)
}

// Helper function to get pointer to value
func ptr[T any](v T) *T {
	return &v
}

// SetupWithManager sets up the controller with the Manager.
func (r *MaaSModelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&maasv1alpha1.MaaSModel{}).
		Complete(r)
}
