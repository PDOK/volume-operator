package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/PDOK/volume-operator/internal/config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	avp "github.com/pdok/azure-volume-populator/api/v1alpha1"
	smoothoperator "github.com/pdok/smooth-operator/pkg/util"
)

const cleanupRetryInterval = time.Minute

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

	if conf.RevisionsMatch() {
		if !conf.HasRequiredAnnotations() {
			logger.Info("Missing required volume annotations")
			return ctrl.Result{}, nil
		}

		volumepopulator, err := createAvpIfNotExists(ctx, r.Client, r.Scheme, conf, &rs)
		if err != nil {
			logger.Error(err, "Failed to create AzureVolumePopulator")
			return ctrl.Result{}, err
		}

		if err = createPvcIfNotExists(ctx, r.Client, r.Scheme, conf, volumepopulator, &rs); err != nil {
			logger.Error(err, "Failed to create PVC")
			return ctrl.Result{}, err
		}
	} else {
		logger.Info(
			"Revision mismatch, skipping resource creation",
			"rsRevision",
			conf.ReplicaSetRevision,
			"deploymentRevision",
			conf.DeploymentRevision,
		)
	}

	requeue, err := cleanUpOldReplicaSets(ctx, r.Client, &rs, deployment, conf)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if requeue {
		return ctrl.Result{RequeueAfter: cleanupRetryInterval}, nil
	}

	return ctrl.Result{}, nil
}

func createPvcIfNotExists(ctx context.Context, c client.Client, scheme *runtime.Scheme, conf config.Config, populator avp.AzureVolumePopulator, rs *appsv1.ReplicaSet) error {
	typeMeta := metav1.TypeMeta{
		Kind: "PersistentVolumeClaim",
	}
	pvc := corev1.PersistentVolumeClaim{}
	err := c.Get(ctx, types.NamespacedName{
		Name:      conf.ResourceName,
		Namespace: conf.ResourceNamespace,
	}, &pvc)

	if err == nil {
		return ensureOwnerReference(ctx, c, &pvc, rs, scheme)
	}
	if !k8serrors.IsNotFound(err) {
		return err
	}

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
				APIGroup: new(avp.GroupVersion.Group),
				Kind:     populator.Kind,
				Name:     populator.Name,
			},
		},
	}

	if err := controllerutil.SetOwnerReference(rs, &pvc, scheme); err != nil {
		return err
	}

	if err := c.Create(ctx, &pvc); err != nil && !k8serrors.IsAlreadyExists(err) {
		return err
	}

	return nil
}

func createAvpIfNotExists(ctx context.Context, c client.Client, scheme *runtime.Scheme, conf config.Config, rs *appsv1.ReplicaSet) (avp.AzureVolumePopulator, error) {
	typeMeta := metav1.TypeMeta{
		Kind:       "AzureVolumePopulator",
		APIVersion: avp.GroupVersion.Group,
	}

	populator := avp.AzureVolumePopulator{}
	err := c.Get(ctx, types.NamespacedName{
		Name:      conf.ResourceName,
		Namespace: conf.ResourceNamespace,
	}, &populator)
	populator.TypeMeta = typeMeta

	if err == nil {
		err = ensureOwnerReference(ctx, c, &populator, rs, scheme)
		populator.TypeMeta = typeMeta
		return populator, err
	}
	if !k8serrors.IsNotFound(err) {
		return populator, err
	}

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

	if err := controllerutil.SetOwnerReference(rs, &populator, scheme); err != nil {
		return populator, err
	}

	if err := c.Create(ctx, &populator); err != nil && !k8serrors.IsAlreadyExists(err) {
		return populator, err
	}
	populator.TypeMeta = typeMeta

	return populator, nil
}

func ensureOwnerReference(ctx context.Context, c client.Client, obj client.Object, rs *appsv1.ReplicaSet, scheme *runtime.Scheme) error {
	key := client.ObjectKeyFromObject(obj)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.Get(ctx, key, obj); err != nil {
			return err
		}

		orig, ok := obj.DeepCopyObject().(client.Object)
		if !ok {
			return fmt.Errorf("unable to copy %T", obj)
		}

		if err := controllerutil.SetOwnerReference(rs, obj, scheme); err != nil {
			return err
		}

		if slices.Equal(orig.GetOwnerReferences(), obj.GetOwnerReferences()) {
			return nil
		}

		return c.Patch(ctx, obj, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
	})
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

func cleanUpOldReplicaSets(ctx context.Context, c client.Client, obj client.Object, deployment *appsv1.Deployment, conf config.Config) (requeue bool, err error) {
	var rsList appsv1.ReplicaSetList
	selector, _ := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err = c.List(ctx, &rsList, client.InNamespace(obj.GetNamespace()), client.MatchingLabelsSelector{
		Selector: selector,
	}); err != nil {
		return false, err
	}

	currentIsAvailable := currentReplicaSetIsAvailable(rsList, conf.DeploymentRevision)

	var errs []error
	pending := false
	for _, rs := range rsList.Items {
		if rs.Annotations[config.RevisionAnnotation] == conf.DeploymentRevision {
			continue
		}
		if hasReplicas(rs) || resourceIsUsedByOtherReplicaSet(rsList, rs) {
			continue
		}
		if rs.Annotations[config.ResourceSuffixAnnotation] == conf.ResourceName {
			continue
		}

		if !currentIsAvailable {
			pending = pending || replicaSetHasResources(ctx, c, &rs)
			continue
		}

		if err := deleteResourcesForReplicaSet(ctx, c, &rs); err != nil {
			errs = append(errs, err)
		}
	}

	return pending, errors.Join(errs...)
}

func replicaSetHasResources(ctx context.Context, c client.Client, rs *appsv1.ReplicaSet) bool {
	logger := logf.FromContext(ctx)
	name := types.NamespacedName{
		Name:      rs.Annotations[config.ResourceSuffixAnnotation],
		Namespace: rs.Namespace,
	}
	if name.Name == "" {
		return false
	}

	if err := c.Get(ctx, name, &avp.AzureVolumePopulator{}); err == nil {
		return true
	} else if !k8serrors.IsNotFound(err) {
		logger.Error(err, "Failed to check AzureVolumePopulator existence during cleanup gating", "replicaSet", rs.Name)
		return true
	}

	if err := c.Get(ctx, name, &corev1.PersistentVolumeClaim{}); err == nil {
		return true
	} else if !k8serrors.IsNotFound(err) {
		logger.Error(err, "Failed to check PVC existence during cleanup gating", "replicaSet", rs.Name)
		return true
	}

	return false
}

func deleteResourcesForReplicaSet(ctx context.Context, c client.Client, rs *appsv1.ReplicaSet) error {
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

	return smoothoperator.DeleteObjects(ctx, c, []client.Object{populator, pvc})
}

// SetupWithManager sets up the controller with the Manager.
func (r *VolumeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.ReplicaSet{}).
		Owns(&avp.AzureVolumePopulator{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.PersistentVolumeClaim{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
