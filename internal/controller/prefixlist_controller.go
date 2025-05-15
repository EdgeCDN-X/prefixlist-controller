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
	"crypto/md5"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
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

const ConsoliadtionStatusConsolidated = "Consolidated"

const HealthStatusHealthy = "Healthy"
const HealthStatusProgressing = "Progressing"

const SourceController = "Controller"

func (r *PrefixListReconciler) reconcileArgocdApplicationSet(prefixList *edgecdnxv1alpha1.PrefixList, ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	applicationsetName := fmt.Sprintf("%s-%s", prefixList.Spec.Destination, "routing")
	appsetFound := &argoprojv1alpha1.ApplicationSet{}

	valuesObject, err := json.Marshal(map[string]interface{}{
		"routing": map[string]interface{}{
			"location": prefixList.Spec.Destination,
			"prefix": map[string]interface{}{
				"v4": prefixList.Spec.Prefix.V4,
				"v6": prefixList.Spec.Prefix.V6,
			},
		},
	})

	hash := md5.Sum(valuesObject)

	if err != nil {
		log.Error(err, "Failed to marshal values object")
		return ctrl.Result{}, err
	}

	specObj := argoprojv1alpha1.ApplicationSetSpec{
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
					Values: map[string]string{
						// TODO make these configurable
						"chartRepository": "https://edgecdn-x.github.io/helm-charts",
						"chart":           "routing-config",
						"chartVersion":    "0.1.0",
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
					Namespace: req.Namespace,
				},
				Sources: []argoprojv1alpha1.ApplicationSource{
					{
						Chart:          "{{ values.chart }}",
						RepoURL:        "{{ values.chartRepository }}",
						TargetRevision: "{{ values.chartVersion }}",
						Helm: &argoprojv1alpha1.ApplicationSourceHelm{
							ReleaseName: "{{ name }}",
							ValuesObject: &runtime.RawExtension{
								Raw: valuesObject,
							},
						},
					},
				},
			},
		},
	}

	err = r.Get(ctx, types.NamespacedName{Namespace: prefixList.Namespace, Name: applicationsetName}, appsetFound)
	if err != nil && apierrors.IsNotFound(err) {
		log.Info("ApplicationSet not found for destination. Creating one")

		appSet := &argoprojv1alpha1.ApplicationSet{
			ObjectMeta: ctrl.ObjectMeta{
				Name:      applicationsetName,
				Namespace: prefixList.Namespace,
				Labels: map[string]string{
					"edgecdnx.com/config-md5": fmt.Sprintf("%x", hash),
				},
			},
			Spec: specObj,
		}
		controllerutil.SetControllerReference(prefixList, appSet, r.Scheme)

		err := r.Create(ctx, appSet)
		if err != nil {
			log.Error(err, "Failed to create ApplicationSet")
			return ctrl.Result{}, err
		}
		log.Info("ApplicationSet created for destination")
		return ctrl.Result{}, nil
	}

	if !appsetFound.DeletionTimestamp.IsZero() {
		log.Info("ApplicationSet is being deleted. Skipping reconciliation")
		return ctrl.Result{}, nil
	}

	appsetHash, ok := appsetFound.ObjectMeta.Labels["edgecdnx.com/config-md5"]
	if ok && appsetHash == fmt.Sprintf("%x", hash) {
		log.Info("ApplicationSet already exists for destination with the correct config (md5-hash). Skipping reconciliation")
		return ctrl.Result{}, nil
	}

	appsetFound.ObjectMeta.Labels["edgecdnx.com/config-md5"] = fmt.Sprintf("%x", hash)
	appsetFound.Spec = specObj

	return ctrl.Result{}, r.Update(ctx, appsetFound)
}

func (r *PrefixListReconciler) handleUserPrefixList(prefixList *edgecdnxv1alpha1.PrefixList, ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if prefixList.Status != (edgecdnxv1alpha1.PrefixListStatus{}) {
		// With status
		// If healthy, nothing to do
		// If not healthy, we need to update the status

		if prefixList.Status.Status == HealthStatusHealthy {
			log.Info("PrefixList is healthy.")

			generatedName := fmt.Sprintf("%s-%s", prefixList.Spec.Destination, "generated")

			generatedPrefixList := &edgecdnxv1alpha1.PrefixList{}
			// Find object by destination. We are consolidating the prefix list here
			err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: generatedName}, generatedPrefixList)
			if err != nil && apierrors.IsNotFound(err) {
				log.Info("PrefixList does not exist for destination, creating one")
				nPrefixList := &edgecdnxv1alpha1.PrefixList{
					ObjectMeta: ctrl.ObjectMeta{
						Name:      generatedName,
						Namespace: req.Namespace,
						Labels: map[string]string{
							"edgecdnx.com/config-md5": "",
						},
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

				controllerutil.SetOwnerReference(prefixList, nPrefixList, r.Scheme)
				return ctrl.Result{}, r.Create(ctx, nPrefixList)
			} else {

				if !generatedPrefixList.DeletionTimestamp.IsZero() {
					log.Info("Generated PrefixList is being deleted. Skipping reconciliation")

					return ctrl.Result{}, nil
				}

				containsOwnerReference := false
				for _, ownerRef := range generatedPrefixList.OwnerReferences {
					if ownerRef.UID == prefixList.UID {
						containsOwnerReference = true
						break
					}
				}

				if !containsOwnerReference {
					log.Info("Prefixlist exists for destination. Adding OwnerReference to generated PrefixList")
					controllerutil.SetOwnerReference(prefixList, generatedPrefixList, r.Scheme)
					r.Update(ctx, generatedPrefixList)
				}

				return ctrl.Result{}, nil
			}
		}
		if prefixList.Status.Status == HealthStatusProgressing {
			log.Info("PrefixList is in progress.")

			prefixList.Status = edgecdnxv1alpha1.PrefixListStatus{
				Status: HealthStatusHealthy,
			}
			return ctrl.Result{}, r.Status().Update(ctx, prefixList)
		}
	} else {
		// No Status
		// Adding one
		prefixList.Status = edgecdnxv1alpha1.PrefixListStatus{
			Status: HealthStatusHealthy,
		}
		return ctrl.Result{}, r.Status().Update(ctx, prefixList)
	}

	return ctrl.Result{}, nil
}

func (r *PrefixListReconciler) handleControllerPrefixList(prefixList *edgecdnxv1alpha1.PrefixList, ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	v4Prefixes := make([]edgecdnxv1alpha1.V4Prefix, 0)
	v6Prefixes := make([]edgecdnxv1alpha1.V6Prefix, 0)

	for _, ownerRes := range prefixList.OwnerReferences {
		if ownerRes.Kind == "PrefixList" {
			log.Info(fmt.Sprintf("%v", ownerRes))
			ownerPrefixList := &edgecdnxv1alpha1.PrefixList{}
			err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: ownerRes.Name}, ownerPrefixList)
			if err != nil {
				log.Error(err, "Failed to get Owner PrefixList")
				return ctrl.Result{}, err
			}

			if ownerPrefixList.Status.Status != HealthStatusHealthy {
				log.Info("Owner PrefixList is in progress. Waiting for it to be healthy")
				return ctrl.Result{}, nil
			}

			v4Prefixes = append(v4Prefixes, ownerPrefixList.Spec.Prefix.V4...)
			v6Prefixes = append(v6Prefixes, ownerPrefixList.Spec.Prefix.V6...)
		}
	}

	newPrefixesV4, err := consolidation.ConsolidateV4(ctx, v4Prefixes)
	if err != nil {
		log.Error(err, "Failed to consolidate prefixes")
		return ctrl.Result{}, err
	}
	newPrefixesV6 := v6Prefixes

	newPrefix := edgecdnxv1alpha1.Prefix{
		V4: newPrefixesV4,
		V6: newPrefixesV6,
	}

	prefixByteA, err := json.Marshal(newPrefix)
	if err != nil {
		log.Error(err, "Failed to marshal prefix object")
		return ctrl.Result{}, err
	}
	newmd5Hash := md5.Sum(prefixByteA)
	log.Info(fmt.Sprintf("New md5 hash: %x", newmd5Hash))
	curHash, ok := prefixList.ObjectMeta.Labels["edgecdnx.com/config-md5"]

	if ok {
		log.Info(fmt.Sprintf("Current md5 hash: %s", curHash))
	}

	if ok && curHash == fmt.Sprintf("%x", newmd5Hash) {

		log.Info("PrefixList already exists for destination with the correct config (md5-hash).")

		prefixList.Status = edgecdnxv1alpha1.PrefixListStatus{
			Status:              HealthStatusHealthy,
			ConsoliadtionStatus: ConsoliadtionStatusConsolidated,
		}

		r.Status().Update(context.Background(), prefixList)

		return r.reconcileArgocdApplicationSet(prefixList, ctx, req)
	}

	prefixList.ObjectMeta.Labels["edgecdnx.com/config-md5"] = fmt.Sprintf("%x", newmd5Hash)
	prefixList.Spec.Prefix = newPrefix

	return ctrl.Result{}, r.Update(ctx, prefixList)
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
			return ctrl.Result{}, nil
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
		Owns(&edgecdnxv1alpha1.PrefixList{}, builder.MatchEveryOwner).
		Complete(r)
}
