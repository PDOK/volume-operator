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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	avp "github.com/pdok/azure-volume-populator/api/v1alpha1"
)

const (
	controllerName      = "volumeoperator"
	defaultStorageClass = "managed-premium-zrs"
)

// var (
//	finalizerName = controllerName + "." + "volume.pdok.nl" + "/finalizer"
// )

// VolumeReconciler reconciles a VolumePopulator object
type VolumeReconciler struct {
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
//
//nolint:cyclop,funlen
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
	if deployment == nil || err != nil {
		logger.Error(err, "Failed to get owning Deployment")
		// err = r.deleteAllForReplicaSet(ctx, &rs, "")
		return ctrl.Result{}, err
	}

	resourceSuffix := deployment.Annotations["volume-operator.pdok.nl/resource-suffix"]
	if resourceSuffix == "" {
		logger.Info("Missing required resource suffix annotation")
		return ctrl.Result{}, nil
	}
	//
	// shouldContinue, err := finalizeIfNecessary(ctx, r.Client, &rs, finalizerName, func() error {
	//	logger.Info("Deleting for replicaset", "name", rs.Name)
	//	return r.deleteAllForReplicaSet(ctx, &rs, resourceSuffix)
	// })
	//
	// if !shouldContinue || err != nil {
	//	err = client.IgnoreNotFound(err)
	//	return ctrl.Result{}, client.IgnoreNotFound(err)
	// }

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

	volumepopulator, err := createAvpIfNotExists(ctx, r.Client, resourceSuffix, rs.Namespace, blobPrefix, volumePath)
	if err != nil {
		logger.Error(err, "Failed to create AzureVolumePopulator")
		return ctrl.Result{}, err
	}

	storageClassName := deployment.Annotations["volume-operator.pdok.nl/storage-class-name"]
	if storageClassName == "" {
		storageClassName = defaultStorageClass
	}

	pvc, err := createPvcIfNotExists(ctx, r.Client, volumepopulator, resourceSuffix, rs.Namespace, storageClassName, storageCapacity)
	if err != nil {
		logger.Error(err, "Failed to create PVC")
		return ctrl.Result{}, err
	}

	// err = createPVCBinderPod(ctx, r.Client, pvc)
	// if err != nil {
	//	logger.Error(err, "Failed to create PVCBinder pod")
	//	return ctrl.Result{}, err
	// }

	ephemeralName := pvc.Name + "-clone"
	// ephemeralName := pvc.Name
	if !deploymentHasVolume(deployment, ephemeralName) {
		err = mountPvcToDeployment(ctx, r.Client, deployment, ephemeralName, &pvc)
	}

	return ctrl.Result{}, err
}

func mountPvcToDeployment(ctx context.Context, c client.Client, deployment *appsv1.Deployment, name string, pvc *corev1.PersistentVolumeClaim) error {
	storageClassName := deployment.Annotations["volume-operator.pdok.nl/storage-class-name"]
	if storageClassName == "" {
		storageClassName = defaultStorageClass
	}
	storageCapacity := deployment.Annotations["volume-operator.pdok.nl/storage-capacity"]

	patch := client.MergeFrom(deployment.DeepCopy())

	deployment.Spec.Template.Spec.Volumes = append(deployment.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			Ephemeral: &corev1.EphemeralVolumeSource{
				VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteOnce,
						},
						StorageClassName: &storageClassName,
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse(storageCapacity),
							},
						},
						DataSource: &corev1.TypedLocalObjectReference{
							APIGroup: StringPtr("v1"),
							Kind:     pvc.Kind,
							Name:     pvc.Name,
						},
					},
				},
			},
		},
	})

	for i := range deployment.Spec.Template.Spec.Containers {
		deployment.Spec.Template.Spec.Containers[i].VolumeMounts = append(deployment.Spec.Template.Spec.Containers[i].VolumeMounts, corev1.VolumeMount{
			Name:      name,
			MountPath: "/mnt/data",
		})
	}

	return c.Patch(ctx, deployment, patch)
}

func createPvcIfNotExists(ctx context.Context, obj client.Client, populator avp.AzureVolumePopulator, suffix string, namespace string, storageClass string, storageCapacity string) (corev1.PersistentVolumeClaim, error) {
	if storageClass == "" {
		storageClass = defaultStorageClass
	}

	volumeName := "volume-" + suffix
	typeMeta := metav1.TypeMeta{
		Kind: "PersistentVolumeClaim",
	}
	pvc := corev1.PersistentVolumeClaim{}
	err := obj.Get(ctx, types.NamespacedName{
		Name:      volumeName,
		Namespace: namespace,
	}, &pvc)

	if k8serrors.IsNotFound(err) {
		pvc = corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      volumeName,
				Namespace: namespace,
			},
			TypeMeta: typeMeta,
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: &storageClass,
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteOnce,
				},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse(storageCapacity),
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
		if err != nil {
			return corev1.PersistentVolumeClaim{}, err
		}
	}

	return pvc, err
}

func createAvpIfNotExists(ctx context.Context, obj client.Client, suffix string, namespace string, prefix string, path string) (avp.AzureVolumePopulator, error) {
	name := "avp-" + suffix

	typeMeta := metav1.TypeMeta{
		Kind:       "AzureVolumePopulator",
		APIVersion: avp.GroupVersion.Group,
	}

	populator := avp.AzureVolumePopulator{}
	err := obj.Get(ctx, types.NamespacedName{
		Name:      name,
		Namespace: namespace,
	}, &populator)
	populator.TypeMeta = typeMeta

	if k8serrors.IsNotFound(err) {
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

// func finalizeIfNecessary(ctx context.Context, c client.Client, obj client.Object, name string, f func() error) (bool, error) {
//	if obj.GetDeletionTimestamp().IsZero() {
//		if !controllerutil.ContainsFinalizer(obj, name) {
//			// controllerutil.AddFinalizer(obj, name)
//			err := c.Update(ctx, obj)
//			return true, err
//		}
//		return true, nil
//	}
//
//	if !controllerutil.ContainsFinalizer(obj, finalizerName) {
//		return false, nil
//	}
//
//	if err := f(); err != nil {
//		return false, err
//	}
//
//	controllerutil.RemoveFinalizer(obj, finalizerName)
//	err := c.Update(ctx, obj)
//	return false, err
// }

// func getObjectFullName(c client.Client, obj client.Object) string {
//	groupVersionKind, err := c.GroupVersionKindFor(obj)
//	if err != nil {
//		return ""
//	}
//	key := client.ObjectKeyFromObject(obj)
//
//	return groupVersionKind.Group + "/" +
//		groupVersionKind.Version + "/" +
//		groupVersionKind.Kind + "/" +
//		key.String()
// }
//
// func (r *VolumeReconciler) deleteAllForReplicaSet(ctx context.Context, rs *appsv1.ReplicaSet, suffix string) error {
//	var objectsToDelete []client.Object
//
//	if suffix != "" {
//		populator := &avp.AzureVolumePopulator{
//			ObjectMeta: metav1.ObjectMeta{
//				Name:      rs.GetName() + suffix,
//				Namespace: rs.Namespace,
//			},
//		}
//
//		pvc := &corev1.PersistentVolumeClaim{
//			ObjectMeta: metav1.ObjectMeta{
//				Name:      rs.GetName() + suffix,
//				Namespace: rs.Namespace,
//			},
//		}
//
//		objectsToDelete = append(objectsToDelete, populator, pvc)
//	}
//
//	objectsToDelete = append(objectsToDelete, rs)
//	return deleteObjects(ctx, r.Client, objectsToDelete)
// }

// SetupWithManager sets up the controller with the Manager.
func (r *VolumeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.ReplicaSet{}).
		Owns(&avp.AzureVolumePopulator{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.PersistentVolumeClaim{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
