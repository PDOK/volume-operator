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

	"github.com/PDOK/volume-operator/internal/config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	avp "github.com/pdok/azure-volume-populator/api/v1alpha1"
)

type VolumeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *VolumeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	var rs appsv1.ReplicaSet
	err := r.Get(ctx, req.NamespacedName, &rs)

	if err != nil {
		if k8serrors.IsNotFound(err) {
			logger.Info("ReplicaSet not found, skipping reconciliation")
		} else {
			logger.Error(err, "Failed to get ReplicaSet")
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	deployment, err := getOwningDeploymentFromReplicaSet(ctx, r.Client, rs)
	if err != nil {
		logger.Error(err, "Failed to get owning Deployment")
		return ctrl.Result{}, err
	}

	conf := config.NewConfigFromAnnotations(deployment, &rs)
	if conf.ResourceName == "" {
		logger.Info("Missing required resource suffix annotation")
		return ctrl.Result{}, nil
	}

	err = cleanUpOldReplicaSets(ctx, r.Client, &rs, deployment, conf)

	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !conf.RevisionsMatch() {
		logger.Info(
			"Revision mismatch, skipping reconciliation",
			"rsRevision",
			conf.ReplicaSetRevision,
			"deploymentRevision",
			conf.DeploymentRevision,
		)
		return ctrl.Result{}, nil
	}

	if !conf.HasRequiredAnnotations() {
		logger.Info("Missing required volume annotations")
		return ctrl.Result{}, nil
	}

	volumepopulator, err := createAvpIfNotExists(ctx, r.Client, conf)
	if err != nil {
		logger.Error(err, "Failed to create AzureVolumePopulator")
		return ctrl.Result{}, err
	}

	err = createPvcIfNotExists(ctx, r.Client, conf, volumepopulator)
	if err != nil {
		logger.Error(err, "Failed to create PVC")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, err
}

func createPvcIfNotExists(ctx context.Context, obj client.Client, conf config.Config, populator avp.AzureVolumePopulator) error {
	typeMeta := metav1.TypeMeta{
		Kind: "PersistentVolumeClaim",
	}
	pvc := corev1.PersistentVolumeClaim{}
	err := obj.Get(ctx, types.NamespacedName{
		Name:      conf.ResourceName,
		Namespace: conf.ResourceNamespace,
	}, &pvc)

	if k8serrors.IsNotFound(err) {
		pvc = corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      conf.ResourceName,
				Namespace: conf.ResourceNamespace,
			},
			TypeMeta: typeMeta,
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: &conf.StorageClassName,
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteOnce,
				},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse(conf.StorageCapacity),
					},
				},
				DataSourceRef: &corev1.TypedObjectReference{
					APIGroup: StringPtr(avp.GroupVersion.Group),
					Kind:     populator.Kind,
					Name:     populator.Name,
				},
			},
		}

		err = obj.Create(ctx, &pvc)
		pvc.TypeMeta = typeMeta
	}

	return err
}

func createAvpIfNotExists(ctx context.Context, obj client.Client, conf config.Config) (avp.AzureVolumePopulator, error) {
	typeMeta := metav1.TypeMeta{
		Kind:       "AzureVolumePopulator",
		APIVersion: avp.GroupVersion.Group,
	}

	populator := avp.AzureVolumePopulator{}
	err := obj.Get(ctx, types.NamespacedName{
		Name:      conf.ResourceName,
		Namespace: conf.ResourceNamespace,
	}, &populator)
	populator.TypeMeta = typeMeta

	if k8serrors.IsNotFound(err) {
		populator = avp.AzureVolumePopulator{
			ObjectMeta: metav1.ObjectMeta{
				Name:      conf.ResourceName,
				Namespace: conf.ResourceNamespace,
			},
			Spec: avp.AzureVolumePopulatorSpec{
				BlobPrefix:          conf.BlobPrefix,
				VolumePath:          conf.VolumePath,
				BlobDownloadOptions: &avp.BlobDownloadOptions{},
			},
		}

		err = obj.Create(ctx, &populator)
		populator.TypeMeta = typeMeta
		return populator, err
	}

	return populator, err
}

func getOwningDeploymentFromReplicaSet(ctx context.Context, c client.Client, rs appsv1.ReplicaSet) (*appsv1.Deployment, error) {
	deployment := &appsv1.Deployment{}

	for _, oref := range rs.OwnerReferences {
		if oref.Kind == "Deployment" {
			err := c.Get(
				ctx, types.NamespacedName{
					Name:      oref.Name,
					Namespace: rs.Namespace,
				},
				deployment)

			if err != nil {
				return nil, err
			}
			break
		}
	}

	return deployment, nil
}

func cleanUpOldReplicaSets(ctx context.Context, c client.Client, obj client.Object, deployment *appsv1.Deployment, conf config.Config) error {
	var rsList appsv1.ReplicaSetList
	selector, _ := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	err := c.List(ctx, &rsList, client.InNamespace(obj.GetNamespace()), client.MatchingLabelsSelector{
		Selector: selector,
	})

	if err != nil {
		return err
	}

	for _, rs := range rsList.Items {
		rsRevision := rs.Annotations[config.RevisionAnnotation]
		if rsRevision != conf.DeploymentRevision {
			if !resourceIsUsedByOtherReplicaSet(rsList, rs) {
				return deleteAllForReplicaSet(ctx, c, &rs)
			}
		}
	}

	return nil
}

func deleteAllForReplicaSet(ctx context.Context, c client.Client, rs *appsv1.ReplicaSet) error {
	resourceName := rs.Annotations[config.ResourceSuffixAnnotation]

	objectMeta := metav1.ObjectMeta{
		Name:      resourceName,
		Namespace: rs.Namespace,
	}

	populator := &avp.AzureVolumePopulator{
		ObjectMeta: objectMeta,
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: objectMeta,
	}

	objectsToDelete := []client.Object{populator, pvc, rs}

	return deleteObjects(ctx, c, objectsToDelete)
}

// SetupWithManager sets up the controller with the Manager.
func (r *VolumeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.ReplicaSet{}).
		Owns(&avp.AzureVolumePopulator{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.PersistentVolumeClaim{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
