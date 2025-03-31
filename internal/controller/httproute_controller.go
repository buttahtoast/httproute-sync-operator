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

package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// HTTPRouteReconciler reconciles HTTPRoute objects to synchronize hostnames with Gateways.
type HTTPRouteReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	// finalizerName is the finalizer added to HTTPRoutes to handle cleanup on delete.
	finalizerName = "httproute-sync-operator.buttah.cloud/finalizer"
)

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/finalizers,verbs=update
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;update;patch

// Reconcile ensures the HTTPRoute's hostnames are synchronized with its referenced Gateway and handles deletes.
func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the HTTPRoute instance
	httpRoute := &gatewayv1.HTTPRoute{}
	err := r.Get(ctx, req.NamespacedName, httpRoute)
	if err != nil {
		if errors.IsNotFound(err) {
			// HTTPRoute was deleted; nothing more to do since cleanup happens via finalizer
			logger.Info("HTTPRoute not found, likely deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to fetch HTTPRoute")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if httpRoute.DeletionTimestamp != nil {
		if containsFinalizer(httpRoute, finalizerName) {
			if err := r.cleanupGateway(ctx, httpRoute); err != nil {
				logger.Error(err, "Failed to clean up Gateway during HTTPRoute deletion")
				return ctrl.Result{}, err
			}
			// Remove finalizer after cleanup
			httpRoute.ObjectMeta.Finalizers = removeFinalizer(httpRoute.ObjectMeta.Finalizers, finalizerName)
			if err := r.Update(ctx, httpRoute); err != nil {
				logger.Error(err, "Failed to remove finalizer from HTTPRoute")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present
	if !containsFinalizer(httpRoute, finalizerName) {
		httpRoute.ObjectMeta.Finalizers = append(httpRoute.ObjectMeta.Finalizers, finalizerName)
		if err := r.Update(ctx, httpRoute); err != nil {
			logger.Error(err, "Failed to add finalizer to HTTPRoute")
			return ctrl.Result{}, err
		}
	}

	// Retrieve the Gateway from the HTTPRoute's ParentRefs
	gateway, err := r.getGatewayFromParentRefs(ctx, httpRoute)
	if err != nil {
		logger.Error(err, "Failed to retrieve Gateway from ParentRefs")
		return ctrl.Result{}, err
	}
	if gateway == nil {
		logger.Info("No Gateway found in ParentRefs")
		return ctrl.Result{}, nil
	}

	// Synchronize hostnames from HTTPRoute to Gateway
	if err := r.syncHostnames(httpRoute, gateway); err != nil {
		logger.Error(err, "Failed to synchronize hostnames with Gateway")
		return ctrl.Result{}, err
	}

	// Update the Gateway with the new configuration
	if err := r.Update(ctx, gateway); err != nil {
		logger.Error(err, "Failed to update Gateway")
		return ctrl.Result{}, err
	}

	logger.Info("Successfully synchronized HTTPRoute hostnames with Gateway", "gateway", gateway.Name)
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.HTTPRoute{}).
		Named("httproute").
		Complete(r)
}

// getGatewayFromParentRefs retrieves the Gateway referenced in the HTTPRoute's ParentRefs.
func (r *HTTPRouteReconciler) getGatewayFromParentRefs(ctx context.Context, httpRoute *gatewayv1.HTTPRoute) (*gatewayv1.Gateway, error) {
	logger := log.FromContext(ctx)

	for _, parentRef := range httpRoute.Spec.ParentRefs {
		if parentRef.Kind != nil && *parentRef.Kind == "Gateway" {
			namespace := httpRoute.Namespace // Default to HTTPRoute's namespace
			if parentRef.Namespace != nil {
				namespace = string(*parentRef.Namespace)
			}

			gateway := &gatewayv1.Gateway{}
			err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: string(parentRef.Name)}, gateway)
			if err != nil {
				logger.Error(err, "Failed to fetch Gateway", "namespace", namespace, "name", parentRef.Name)
				return nil, fmt.Errorf("failed to fetch Gateway %s/%s: %w", namespace, parentRef.Name, err)
			}
			return gateway, nil
		}
	}
	return nil, nil // No Gateway found in ParentRefs
}

// syncHostnames adds HTTPRoute hostnames to the Gateway if they don't already exist.
func (r *HTTPRouteReconciler) syncHostnames(httpRoute *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) error {
	for _, hostname := range httpRoute.Spec.Hostnames {
		if !isHostnameInGateway(hostname, gateway) {
			newListener := r.createListenerForHostname(hostname, gateway)
			if newListener == nil {
				return fmt.Errorf("no reference listener found in Gateway %s for hostname %s", gateway.Name, hostname)
			}
			gateway.Spec.Listeners = append(gateway.Spec.Listeners, *newListener)
		}
	}
	return nil
}

// cleanupGateway removes listeners from the Gateway that were added by this HTTPRoute.
func (r *HTTPRouteReconciler) cleanupGateway(ctx context.Context, httpRoute *gatewayv1.HTTPRoute) error {
	logger := log.FromContext(ctx)

	gateway, err := r.getGatewayFromParentRefs(ctx, httpRoute)
	if err != nil {
		return err
	}
	if gateway == nil {
		logger.Info("No Gateway found to clean up")
		return nil
	}

	// Filter out listeners that match the HTTPRoute's hostnames
	updatedListeners := []gatewayv1.Listener{}
	for _, listener := range gateway.Spec.Listeners {
		keep := true
		for _, hostname := range httpRoute.Spec.Hostnames {
			if listener.Hostname != nil && *listener.Hostname == hostname {
				keep = false
				break
			}
		}
		if keep {
			updatedListeners = append(updatedListeners, listener)
		}
	}

	// Update Gateway only if listeners changed
	if len(updatedListeners) != len(gateway.Spec.Listeners) {
		gateway.Spec.Listeners = updatedListeners
		if err := r.Update(ctx, gateway); err != nil {
			logger.Error(err, "Failed to update Gateway during cleanup")
			return fmt.Errorf("failed to clean up Gateway %s: %w", gateway.Name, err)
		}
		logger.Info("Cleaned up Gateway listeners", "gateway", gateway.Name)
	}

	return nil
}

// isHostnameInGateway checks if a hostname already exists in the Gateway's listeners.
func isHostnameInGateway(hostname gatewayv1.Hostname, gateway *gatewayv1.Gateway) bool {
	for _, listener := range gateway.Spec.Listeners {
		if listener.Hostname != nil && *listener.Hostname == hostname {
			return true
		}
	}
	return false
}

// createListenerForHostname creates a new listener based on a reference listener in the Gateway.
func (r *HTTPRouteReconciler) createListenerForHostname(hostname gatewayv1.Hostname, gateway *gatewayv1.Gateway) *gatewayv1.Listener {
	refListener := r.getReferenceListener(gateway)
	if refListener == nil {
		return nil
	}

	// Create a new listener by copying the reference listener
	newListener := &gatewayv1.Listener{
		Name:     gatewayv1.SectionName(hostname),
		Protocol: refListener.Protocol,
		Port:     refListener.Port,
		Hostname: &hostname,
		AllowedRoutes: &gatewayv1.AllowedRoutes{
			Namespaces: &gatewayv1.RouteNamespaces{
				From: refListener.AllowedRoutes.Namespaces.From,
			},
		},
		TLS: refListener.TLS,
	}
	return newListener
}

// getReferenceListener finds a suitable listener (e.g., port 443) to use as a template.
func (r *HTTPRouteReconciler) getReferenceListener(gateway *gatewayv1.Gateway) *gatewayv1.Listener {
	for _, listener := range gateway.Spec.Listeners {
		if listener.Port == 443 {
			return &listener
		}
	}
	return nil
}

// containsFinalizer checks if the finalizer exists in the list.
func containsFinalizer(httpRoute *gatewayv1.HTTPRoute, finalizer string) bool {
	for _, f := range httpRoute.ObjectMeta.Finalizers {
		if f == finalizer {
			return true
		}
	}
	return false
}

// removeFinalizer removes the specified finalizer from the list.
func removeFinalizer(finalizers []string, finalizer string) []string {
	updated := []string{}
	for _, f := range finalizers {
		if f != finalizer {
			updated = append(updated, f)
		}
	}
	return updated
}
