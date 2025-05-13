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
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	edgecdnxv1alpha1 "edgecdnx.com/prefixlist-controller/api/v1alpha1"
	"edgecdnx.com/prefixlist-controller/internal/consolidation"
	argoprojv1alpha1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
)

// PrefixListReconciler reconciles a PrefixList object
type PrefixListReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const PrefixListFinalizer = "prefixlist.edgecdnx.com/finalizer"

const ConsoliadtionStatusRequested = "Requested"
const ConsoliadtionStatusConsolidating = "Consolidating"
const ConsoliadtionStatusConsolidated = "Consolidated"

const HealthStatusHealthy = "Healthy"
const HealthStatusProgressing = "Progressing"

const SourceController = "Controller"

func (r *PrefixListReconciler) reconcileArgocdApplicationSet(prefixList *edgecdnxv1alpha1.PrefixList, ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	applicationsetName := fmt.Sprintf("%s-%s", prefixList.Spec.Destination, "routing-applicationset")
	appsetFound := &argoprojv1alpha1.ApplicationSet{}

	appSet := &argoprojv1alpha1.ApplicationSet{
		ObjectMeta: ctrl.ObjectMeta{
			Name:      applicationsetName,
			Namespace: prefixList.Namespace,
		},
		Spec: argoprojv1alpha1.ApplicationSetSpec{
			Generators: []argoprojv1alpha1.ApplicationSetGenerator{
				{
					Clusters: &argoprojv1alpha1.ClusterGenerator{
						Selector: metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "edgecdnx.com/routing",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"yes", "true"},
								},
							},
						},
						Template: argoprojv1alpha1.ApplicationSetTemplate{},
						Values: map[string]string{
							"chart":        "myroutingchart",
							"chartVersion": "1.0.0",
						},
					},
				},
			},
			Template: argoprojv1alpha1.ApplicationSetTemplate{
				ApplicationSetTemplateMeta: argoprojv1alpha1.ApplicationSetTemplateMeta{
					Name: fmt.Sprintf("%s-%s", "{{ name }}-routing", prefixList.Spec.Destination),
				},
				Spec: argoprojv1alpha1.ApplicationSpec{
					Project: "edgecdnx",
					Destination: argoprojv1alpha1.ApplicationDestination{
						Server:    "{{ server }}",
						Namespace: "edgecdnx",
					},
					Sources: []argoprojv1alpha1.ApplicationSource{},
				},
			},
		},
	}
	controllerutil.SetControllerReference(prefixList, appSet, r.Scheme)

	err := r.Get(ctx, types.NamespacedName{Namespace: prefixList.Namespace, Name: applicationsetName}, appsetFound)
	if err != nil && apierrors.IsNotFound(err) {
		log.Info("ApplicationSet not found for destination. Creating one")

		err := r.Create(ctx, appSet)
		if err != nil {
			log.Error(err, "Failed to create ApplicationSet")
			return ctrl.Result{}, err
		}
		log.Info("ApplicationSet created for destination")
		return ctrl.Result{}, nil
	}

	err = r.Update(ctx, appSet)
	if err != nil {
		log.Error(err, "Failed to update ApplicationSet")
		return ctrl.Result{}, err
	}
	log.Info("ApplicationSet updated for destination")
	return ctrl.Result{}, nil
}

func (r *PrefixListReconciler) handleControllerPrefixList(prefixList *edgecdnxv1alpha1.PrefixList, ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !prefixList.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(prefixList, PrefixListFinalizer) {
			log.Info("Deleting Controller managed PrefixList. This is not really a common operation. We will request a recalculation of the prefix list")
			controllerutil.RemoveFinalizer(prefixList, PrefixListFinalizer)

			err := r.Update(ctx, prefixList)
			if err != nil {
				log.Error(err, "Failed to remove finalizer")
				return ctrl.Result{}, err
			}

			return ctrl.Result{Requeue: true}, nil
		}
	}

	if prefixList.Status != (edgecdnxv1alpha1.PrefixListStatus{}) {
		if prefixList.Status.Status == HealthStatusProgressing && prefixList.Status.ConsoliadtionStatus == ConsoliadtionStatusRequested {
			log.Info("Prefix Recalculation requested for Controller managed PrefixList")

			prefixList.Status = edgecdnxv1alpha1.PrefixListStatus{
				Status:              HealthStatusProgressing,
				ConsoliadtionStatus: ConsoliadtionStatusConsolidating,
			}

			err := r.Status().Update(context.Background(), prefixList)
			if err != nil {
				log.Error(err, "Failed to update PrefixList status")
				return ctrl.Result{}, err
			}

			prefixListList := &edgecdnxv1alpha1.PrefixListList{}
			err = r.List(ctx, prefixListList, client.InNamespace(req.Namespace))

			if err != nil {
				log.Error(err, "Failed to list PrefixList")
				return ctrl.Result{}, err
			}

			v4Prefixes := make([]edgecdnxv1alpha1.V4Prefix, 0)
			// v6Prefixes := make([]edgecdnxv1alpha1.V6Prefix, 0)
			for _, prefix := range prefixListList.Items {
				if prefix.Spec.Source != SourceController && prefix.Spec.Destination == prefixList.Spec.Destination {
					v4Prefixes = append(v4Prefixes, prefix.Spec.Prefix.V4...)
					// v6Prefixes = append(v6Prefixes, prefix.Spec.Prefix.V6...)
				}
			}

			newPrefixes, err := consolidation.ConsolidateV4(ctx, v4Prefixes)
			if err != nil {
				log.Error(err, "Failed to consolidate prefixes")
				return ctrl.Result{}, err
			}
			prefixList.Spec.Prefix.V4 = newPrefixes

			err = r.Update(ctx, prefixList)
			if err != nil {
				log.Error(err, "Failed to update PrefixList")
				return ctrl.Result{}, err
			}

			prefixList.Status = edgecdnxv1alpha1.PrefixListStatus{
				Status:              HealthStatusProgressing,
				ConsoliadtionStatus: ConsoliadtionStatusConsolidated,
			}

			err = r.Status().Update(context.Background(), prefixList)
			if err != nil {
				log.Error(err, "Failed to update PrefixList status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, nil
		}
		if prefixList.Status.Status == HealthStatusProgressing && prefixList.Status.ConsoliadtionStatus == ConsoliadtionStatusConsolidating {
			log.Info("Prefix Recalculation in progress Skipping reconciliation")
			return ctrl.Result{}, nil
		}
		if prefixList.Status.Status == HealthStatusProgressing && prefixList.Status.ConsoliadtionStatus == ConsoliadtionStatusConsolidated {
			log.Info("Prefix Recalculation completed for Controller managed PrefixList")
			prefixList.Status = edgecdnxv1alpha1.PrefixListStatus{
				Status:              HealthStatusHealthy,
				ConsoliadtionStatus: ConsoliadtionStatusConsolidated,
			}

			err := r.Status().Update(context.Background(), prefixList)
			if err != nil {
				log.Error(err, "Failed to update PrefixList status")
				return ctrl.Result{}, err
			}
		}
		if prefixList.Status.Status == HealthStatusHealthy && prefixList.Status.ConsoliadtionStatus == ConsoliadtionStatusConsolidated {
			log.Info("PrefixList is healthy, Rolling it out via Argocd")

			return r.reconcileArgocdApplicationSet(prefixList, ctx, req)
		}
	} else {
		prefixList.Status = edgecdnxv1alpha1.PrefixListStatus{
			Status:              HealthStatusProgressing,
			ConsoliadtionStatus: ConsoliadtionStatusRequested,
		}

		err := r.Status().Update(context.Background(), prefixList)
		if err != nil {
			log.Error(err, "Failed to update PrefixList status")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *PrefixListReconciler) handleUserPrefixList(prefixList *edgecdnxv1alpha1.PrefixList, ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Static and BGP Chain
	generatedName := fmt.Sprintf("%s-%s", prefixList.Spec.Destination, "generated")

	if !prefixList.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(prefixList, PrefixListFinalizer) {
			log.Info("Deleting PrefixList")
			controllerutil.RemoveFinalizer(prefixList, PrefixListFinalizer)
			err := r.Update(ctx, prefixList)
			if err != nil {
				log.Error(err, "Failed to remove finalizer")
				return ctrl.Result{}, err
			}
		}

		// Make sure we call the regeneration
		generatedPrefixList := &edgecdnxv1alpha1.PrefixList{}
		err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: generatedName}, generatedPrefixList)
		if err != nil {
			return ctrl.Result{}, err
		}

		generatedPrefixList.Status.Status = HealthStatusProgressing
		generatedPrefixList.Status.ConsoliadtionStatus = ConsoliadtionStatusRequested
		err = r.Status().Update(context.Background(), generatedPrefixList)
		if err != nil {
			log.Error(err, "Failed to update status of generated PrefixList")
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	if prefixList.Status != (edgecdnxv1alpha1.PrefixListStatus{}) {
		if prefixList.Status.Status == HealthStatusHealthy {
			generatedPrefixList := &edgecdnxv1alpha1.PrefixList{}
			// Find object by destination. We are consolidating the prefix list here
			err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: generatedName}, generatedPrefixList)
			if err != nil && apierrors.IsNotFound(err) {
				log.Info("PrefixList does not exist for destination, creating one")
				nPrefixList := &edgecdnxv1alpha1.PrefixList{
					ObjectMeta: ctrl.ObjectMeta{
						Name:      generatedName,
						Namespace: req.Namespace,
					},
					Spec: edgecdnxv1alpha1.PrefixListSpec{
						Source: SourceController,
						Prefix: edgecdnxv1alpha1.Prefix{
							V4: make([]edgecdnxv1alpha1.V4Prefix, 0),
							V6: make([]edgecdnxv1alpha1.V6Prefix, 0),
						},
						Destination: prefixList.Spec.Destination,
					},
				}

				if !controllerutil.ContainsFinalizer(nPrefixList, PrefixListFinalizer) {
					controllerutil.AddFinalizer(nPrefixList, PrefixListFinalizer)
				}

				err := r.Create(ctx, nPrefixList)

				if err != nil {
					log.Error(err, "Failed to create PrefixList")
					return ctrl.Result{}, err
				}

				log.Info("PrefixList created for destination")
				return ctrl.Result{}, nil
			} else {
				log.Info("PrefixList already exists for destination, Triggering Reconciliation")
				// Update the consolidated prefix list with the new prefixes
				generatedPrefixList.Status.Status = HealthStatusProgressing
				generatedPrefixList.Status.ConsoliadtionStatus = ConsoliadtionStatusRequested

				err := r.Status().Update(context.Background(), generatedPrefixList)
				if err != nil {
					return ctrl.Result{}, err
				}

				return ctrl.Result{}, nil
			}
		}
	}

	log.Info("PrefixList status is nil, resource just created. Setting status to Healthy")
	prefixList.Status = edgecdnxv1alpha1.PrefixListStatus{
		Status: HealthStatusHealthy,
	}

	err := r.Status().Update(context.Background(), prefixList)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !controllerutil.ContainsFinalizer(prefixList, PrefixListFinalizer) {
		controllerutil.AddFinalizer(prefixList, PrefixListFinalizer)
		err = r.Update(ctx, prefixList)
		if err != nil {
			log.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil

}

func (r *PrefixListReconciler) forceSync(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	log.Info("Running triggers, see if we need to trigger a regeneration")
	prefixListList := &edgecdnxv1alpha1.PrefixListList{}
	err := r.List(ctx, prefixListList, client.InNamespace(req.Namespace))

	if err != nil {
		log.Error(err, "Failed to list PrefixList")
		return ctrl.Result{}, err
	}

	notFoundList := make([]string, 0)

	for _, prefixList := range prefixListList.Items {
		if prefixList.Spec.Source != SourceController {

			if !slices.ContainsFunc(prefixListList.Items, func(item edgecdnxv1alpha1.PrefixList) bool {
				return item.Spec.Source == SourceController && prefixList.Spec.Destination == item.Spec.Destination && !slices.Contains(notFoundList, prefixList.Spec.Destination)
			}) {
				notFoundList = append(notFoundList, prefixList.Spec.Destination)
				prefixList.Status.Status = HealthStatusProgressing
				err = r.Status().Update(context.Background(), &prefixList)
				if err != nil {
					return ctrl.Result{}, err
				}
			}
		}
	}

	return ctrl.Result{}, nil
}

// +kubebuilder:rbac:groups=edgecdnx.edgecdnx.com,resources=prefixlists,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=edgecdnx.edgecdnx.com,resources=prefixlists/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=edgecdnx.edgecdnx.com,resources=prefixlists/finalizers,verbs=update
// +kubebuilder:rbac:groups=argoproj.io,resources=applicationsets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the PrefixList object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *PrefixListReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the PrefixList instance
	prefixList := &edgecdnxv1alpha1.PrefixList{}
	err := r.Get(ctx, req.NamespacedName, prefixList)

	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("PrefixList resource not found. Ignoring since object must be deleted.")
			return r.forceSync(ctx, req)
		}
		log.Error(err, "Failed to get PrefixList")
		return ctrl.Result{}, err
	}

	if prefixList.Spec.Source == "Static" || prefixList.Spec.Source == "Bgp" {
		return r.handleUserPrefixList(prefixList, ctx, req)
	}

	if prefixList.Spec.Source == SourceController {
		return r.handleControllerPrefixList(prefixList, ctx, req)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PrefixListReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&edgecdnxv1alpha1.PrefixList{}).
		Owns(&argoprojv1alpha1.ApplicationSet{}).
		Complete(r)
}
