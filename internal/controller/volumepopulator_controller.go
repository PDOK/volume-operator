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
	"crypto/sha256"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	avp "github.com/pdok/azure-volume-populator/api/v1alpha1"
)

const (
	controllerName = "volumeoperator"
)

var (
	finalizerName = controllerName + "." + "volume.pdok.nl" + "/finalizer"
)

// VolumePopulatorReconciler reconciles a VolumePopulator object
type VolumePopulatorReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=pdok.nl,resources=volumepopulators,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pdok.nl,resources=volumepopulators/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pdok.nl,resources=volumepopulators/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// the VolumePopulator object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.1/pkg/reconcile
func (r *VolumePopulatorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	var rs appsv1.ReplicaSet

	err := r.Get(ctx, req.NamespacedName, &rs)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("ReplicaSet not found, skipping reconciliation")
			return ctrl.Result{}, err
		}
		logger.Error(err, "Failed to get ReplicaSet")
		return ctrl.Result{}, err
	}

	deployment, err := getOwningDeploymentFromResultSet(ctx, r.Client, rs)
	if deployment == nil || err != nil {
		logger.Error(err, "Failed to get owning Deployment")
		return ctrl.Result{}, err
	}

	rsRevision := rs.Annotations["deployment.kubernetes.io/revision"]
	deploymentRevision := deployment.Annotations["deployment.kubernetes.io/revision"]

	if rsRevision == "" || deploymentRevision == "" || rsRevision != deploymentRevision {
		logger.Info("Revision mismatch, skipping reconciliation", "rsRevision", rsRevision, "deploymentRevision", deploymentRevision)
		return ctrl.Result{}, nil
	}

	blobPrefix := deployment.Annotations["volume-operator.pdok.nl/blob-prefix"]
	volumePath := deployment.Annotations["volume-operator.pdok.nl/volume-path"]
	storageCapacity := deployment.Annotations["volume-operator.pdok.nl/storage-capacity"]

	if blobPrefix == "" || volumePath == "" || storageCapacity == "" {
		logger.Info("Missing required volume annotations")
		return ctrl.Result{}, nil
	}

	hash := sha256.Sum256([]byte(blobPrefix + volumePath + storageCapacity))
	volumeName := "populator-" + string(hash[:8])

	pvc, err := createPvcIfNotExists(ctx, r.Client, volumeName, rs.Namespace, storageCapacity)
	if err != nil {
		logger.Error(err, "Failed to create PVC")
		return ctrl.Result{}, err
	}

	volumepopulator, err := createAvpIfNotExists(ctx, r.Client, volumeName, rs.Namespace, blobPrefix, volumePath)
	if err != nil {
		logger.Error(err, "Failed to create AzureVolumePopulator")
		return ctrl.Result{}, err
	}
	shouldContinue, err := finalizeIfNecessary(ctx, r.Client, &rs, finalizerName, func() error {
		logger.Info("Deleting for VolumePopulator", "name", volumeName)
		return r.deleteAllForVolumePopulator(ctx, &volumepopulator, &pvc)
	})

	if !shouldContinue || err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	return ctrl.Result{}, err
}

func createPvcIfNotExists(ctx context.Context, obj client.Client, volumeName string, namespace string, storageCapacity string) (corev1.PersistentVolumeClaim, error) {
	logger := logf.FromContext(ctx)
	pvc := corev1.PersistentVolumeClaim{}
	err := obj.Get(ctx, types.NamespacedName{
		Name:      volumeName,
		Namespace: namespace,
	}, &pvc)

	if errors.IsNotFound(err) {
		logger.Info("PVC not found, creating new one")
		pvc = corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      volumeName,
				Namespace: namespace,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteOnce,
				},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse(storageCapacity),
					},
				},
			},
		}

		err = obj.Create(ctx, &pvc)
		if err != nil {
			logger.Error(err, "Failed to create PVC")
			return corev1.PersistentVolumeClaim{}, err
		}
		logger.Info("PVC created", "PVC", pvc)
		return pvc, err
	}

	return pvc, err
}

func createAvpIfNotExists(ctx context.Context, obj client.Client, name string, namespace string, prefix string, path string) (avp.AzureVolumePopulator, error) {
	logger := logf.FromContext(ctx)
	populator := avp.AzureVolumePopulator{}
	err := obj.Get(ctx, types.NamespacedName{
		Name:      name,
		Namespace: namespace,
	}, &populator)

	if errors.IsNotFound(err) {
		logger.Info("AzureVolumePopulator not found, creating new one")
		populator = avp.AzureVolumePopulator{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: avp.AzureVolumePopulatorSpec{
				BlobPrefix:          prefix,
				VolumePath:          path,
				BlobDownloadOptions: &avp.BlobDownloadOptions{},
			},
		}
		err = obj.Create(ctx, &populator)
		if err != nil {
			logger.Error(err, "Failed to create AzureVolumePopulator")
			return populator, err
		}
		logger.Info("AzureVolumePopulator created", "AzureVolumePopulator", populator)
		return populator, err
	}

	return populator, err
}

func getOwningDeploymentFromResultSet(ctx context.Context, c client.Client, rs appsv1.ReplicaSet) (*appsv1.Deployment, error) {
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

func finalizeIfNecessary(ctx context.Context, c client.Client, obj client.Object, name string, f func() error) (bool, error) {
	if obj.GetDeletionTimestamp().IsZero() {
		if !controllerutil.ContainsFinalizer(obj, name) {
			controllerutil.AddFinalizer(obj, name)
			err := c.Update(ctx, obj)
			return true, err
		}
		return true, nil
	}

	if !controllerutil.ContainsFinalizer(obj, finalizerName) {
		return false, nil
	}

	if err := f(); err != nil {
		return false, err
	}

	controllerutil.RemoveFinalizer(obj, finalizerName)
	err := c.Update(ctx, obj)
	return false, err
}

func getObjectFullName(c client.Client, obj client.Object) string {
	groupVersionKind, err := c.GroupVersionKindFor(obj)
	if err != nil {
		return ""
	}
	key := client.ObjectKeyFromObject(obj)

	return groupVersionKind.Group + "/" +
		groupVersionKind.Version + "/" +
		groupVersionKind.Kind + "/" +
		key.String()
}
func (r *VolumePopulatorReconciler) deleteAllForVolumePopulator(ctx context.Context, avp *avp.AzureVolumePopulator, pvc *corev1.PersistentVolumeClaim) error {
	return deleteObjects(ctx, r.Client, []client.Object{
		avp,
		pvc,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *VolumePopulatorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.ReplicaSet{}).
		Owns(&avp.AzureVolumePopulator{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.PersistentVolumeClaim{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
