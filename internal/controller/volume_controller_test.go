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
	"maps"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/PDOK/volume-operator/internal/config"
	avp "github.com/pdok/azure-volume-populator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	testNamespace             = "default"
	blobPrefixAnnotation      = "volume-operator.pdok.nl/blob-prefix"
	volumePathAnnotation      = "volume-operator.pdok.nl/volume-path"
	storageClassAnnotation    = "volume-operator.pdok.nl/storage-class"
	storageCapacityAnnotation = "volume-operator.pdok.nl/storage-capacity"
)

func newDeployment(name, revision string, annotations map[string]string) *appsv1.Deployment {
	allAnnotations := map[string]string{config.RevisionAnnotation: revision}
	maps.Copy(allAnnotations, annotations)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   testNamespace,
			Annotations: allAnnotations,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
			},
		},
	}
}

func newReplicaSet(name string, owner *appsv1.Deployment, revision string, replicas, availableReplicas int32, resourceSuffix string) *appsv1.ReplicaSet {
	labels := map[string]string{}
	var ownerRefs []metav1.OwnerReference
	if owner != nil {
		maps.Copy(labels, owner.Spec.Selector.MatchLabels)
		ownerRefs = []metav1.OwnerReference{
			{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       owner.Name,
				UID:        owner.UID,
			},
		}
	}

	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       testNamespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
			Annotations: map[string]string{
				config.RevisionAnnotation:       revision,
				config.ResourceSuffixAnnotation: resourceSuffix,
			},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
		},
		Status: appsv1.ReplicaSetStatus{
			AvailableReplicas: availableReplicas,
		},
	}
}

func newAVP(name string) *avp.AzureVolumePopulator {
	return &avp.AzureVolumePopulator{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
	}
}

func newPVC(name string, phase corev1.PersistentVolumeClaimPhase) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
	}
	if phase != "" {
		pvc.Status.Phase = phase
	}
	return pvc
}

var _ = Describe("Volume Controller", func() {
	var (
		ctx                  context.Context
		k8sClient            client.Client
		controllerReconciler *VolumeReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()

		scheme := runtime.NewScheme()
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(avp.AddToScheme(scheme)).To(Succeed())

		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
		controllerReconciler = &VolumeReconciler{Client: k8sClient, Scheme: scheme}
	})

	reconcileRS := func(name string) (reconcile.Result, error) {
		return controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: testNamespace},
		})
	}

	It("does nothing when the ReplicaSet is not found", func() {
		_, err := reconcileRS("missing-rs")
		Expect(err).NotTo(HaveOccurred())
	})

	It("skips reconciliation when the ReplicaSet has no owning Deployment", func() {
		rs := newReplicaSet("rs-no-owner", nil, "1", 1, 1, "suffix-no-owner")
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())

		_, err := reconcileRS(rs.Name)
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-no-owner", Namespace: testNamespace}, &avp.AzureVolumePopulator{})
		Expect(err).To(HaveOccurred())
	})

	It("skips reconciliation when the Deployment is missing the resource-suffix annotation", func() {
		deployment := newDeployment("dep-no-suffix", "1", nil)
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

		rs := newReplicaSet("rs-no-suffix", deployment, "1", 1, 1, "")
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())

		_, err := reconcileRS(rs.Name)
		Expect(err).NotTo(HaveOccurred())
	})

	It("skips creating resources when the ReplicaSet and Deployment revisions differ", func() {
		deployment := newDeployment("dep-revision-mismatch", "2", map[string]string{
			config.ResourceSuffixAnnotation: "suffix-revision-mismatch",
			blobPrefixAnnotation:            "blob/prefix",
			volumePathAnnotation:            "/data",
		})
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

		rs := newReplicaSet("rs-revision-mismatch", deployment, "1", 1, 1, "suffix-revision-mismatch")
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())

		_, err := reconcileRS(rs.Name)
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-revision-mismatch", Namespace: testNamespace}, &avp.AzureVolumePopulator{})
		Expect(err).To(HaveOccurred())
	})

	It("skips creating resources when required volume annotations are missing", func() {
		deployment := newDeployment("dep-missing-vol-annotations", "1", map[string]string{
			config.ResourceSuffixAnnotation: "suffix-missing-vol",
		})
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

		rs := newReplicaSet("rs-missing-vol", deployment, "1", 1, 1, "suffix-missing-vol")
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())

		_, err := reconcileRS(rs.Name)
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-missing-vol", Namespace: testNamespace}, &avp.AzureVolumePopulator{})
		Expect(err).To(HaveOccurred())
	})

	It("creates the AVP and PVC when all required annotations are present and revisions match", func() {
		deployment := newDeployment("dep-happy", "1", map[string]string{
			config.ResourceSuffixAnnotation: "suffix-happy",
			blobPrefixAnnotation:            "blob/prefix",
			volumePathAnnotation:            "/data",
		})
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

		rs := newReplicaSet("rs-happy", deployment, "1", 1, 1, "suffix-happy")
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())

		_, err := reconcileRS(rs.Name)
		Expect(err).NotTo(HaveOccurred())

		createdAvp := &avp.AzureVolumePopulator{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-happy", Namespace: testNamespace}, createdAvp)).To(Succeed())
		Expect(createdAvp.Spec.BlobPrefix).To(Equal("blob/prefix"))
		Expect(createdAvp.Spec.VolumePath).To(Equal("/data"))

		createdPvc := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-happy", Namespace: testNamespace}, createdPvc)).To(Succeed())
		Expect(createdPvc.Spec.DataSourceRef).NotTo(BeNil())
		Expect(createdPvc.Spec.DataSourceRef.Name).To(Equal(createdAvp.Name))
	})

	It("does not error or duplicate resources when the AVP and PVC already exist", func() {
		deployment := newDeployment("dep-idempotent", "1", map[string]string{
			config.ResourceSuffixAnnotation: "suffix-idempotent",
			blobPrefixAnnotation:            "blob/prefix",
			volumePathAnnotation:            "/data",
		})
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

		rs := newReplicaSet("rs-idempotent", deployment, "1", 1, 1, "suffix-idempotent")
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())

		_, err := reconcileRS(rs.Name)
		Expect(err).NotTo(HaveOccurred())

		_, err = reconcileRS(rs.Name)
		Expect(err).NotTo(HaveOccurred())

		avpList := &avp.AzureVolumePopulatorList{}
		Expect(k8sClient.List(ctx, avpList, client.InNamespace(testNamespace))).To(Succeed())
		Expect(avpList.Items).To(HaveLen(1))

		pvcList := &corev1.PersistentVolumeClaimList{}
		Expect(k8sClient.List(ctx, pvcList, client.InNamespace(testNamespace))).To(Succeed())
		Expect(pvcList.Items).To(HaveLen(1))
	})

	It("cleans up an old ReplicaSet's AVP and PVC once the new ReplicaSet is available", func() {
		deployment := newDeployment("dep-cleanup", "2", map[string]string{
			config.ResourceSuffixAnnotation: "suffix-new",
			blobPrefixAnnotation:            "blob/prefix",
			volumePathAnnotation:            "/data",
		})
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

		oldRs := newReplicaSet("rs-old", deployment, "1", 0, 0, "suffix-old")
		Expect(k8sClient.Create(ctx, oldRs)).To(Succeed())

		newRs := newReplicaSet("rs-new", deployment, "2", 1, 1, "suffix-new")
		Expect(k8sClient.Create(ctx, newRs)).To(Succeed())

		oldAvp := &avp.AzureVolumePopulator{ObjectMeta: metav1.ObjectMeta{Name: "suffix-old", Namespace: testNamespace}}
		Expect(k8sClient.Create(ctx, oldAvp)).To(Succeed())
		oldPvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "suffix-old", Namespace: testNamespace}}
		Expect(k8sClient.Create(ctx, oldPvc)).To(Succeed())

		_, err := reconcileRS(newRs.Name)
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-old", Namespace: testNamespace}, &avp.AzureVolumePopulator{})
		Expect(err).To(HaveOccurred())
		err = k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-old", Namespace: testNamespace}, &corev1.PersistentVolumeClaim{})
		Expect(err).To(HaveOccurred())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-new", Namespace: testNamespace}, &avp.AzureVolumePopulator{})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-new", Namespace: testNamespace}, &corev1.PersistentVolumeClaim{})).To(Succeed())
	})

	It("does not delete the old ReplicaSet's resources when the new ReplicaSet is not yet available", func() {
		deployment := newDeployment("dep-deferred", "2", map[string]string{
			config.ResourceSuffixAnnotation: "suffix-new-deferred",
			blobPrefixAnnotation:            "blob/prefix",
			volumePathAnnotation:            "/data",
		})
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

		oldRs := newReplicaSet("rs-old-deferred", deployment, "1", 0, 0, "suffix-old-deferred")
		Expect(k8sClient.Create(ctx, oldRs)).To(Succeed())

		// New ReplicaSet matches the current Deployment revision but isn't up yet.
		newRs := newReplicaSet("rs-new-deferred", deployment, "2", 1, 0, "suffix-new-deferred")
		Expect(k8sClient.Create(ctx, newRs)).To(Succeed())

		oldAvp := &avp.AzureVolumePopulator{ObjectMeta: metav1.ObjectMeta{Name: "suffix-old-deferred", Namespace: testNamespace}}
		Expect(k8sClient.Create(ctx, oldAvp)).To(Succeed())
		oldPvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "suffix-old-deferred", Namespace: testNamespace}}
		Expect(k8sClient.Create(ctx, oldPvc)).To(Succeed())

		_, err := reconcileRS(newRs.Name)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-old-deferred", Namespace: testNamespace}, &avp.AzureVolumePopulator{})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-old-deferred", Namespace: testNamespace}, &corev1.PersistentVolumeClaim{})).To(Succeed())
	})

	Context("creating resources", func() {
		It("uses the default storage class and capacity when the storage annotations are omitted", func() {
			deployment := newDeployment("dep-defaults", "1", map[string]string{
				config.ResourceSuffixAnnotation: "suffix-defaults",
				blobPrefixAnnotation:            "blob/prefix",
				volumePathAnnotation:            "/data",
			})
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			rs := newReplicaSet("rs-defaults", deployment, "1", 1, 1, "suffix-defaults")
			Expect(k8sClient.Create(ctx, rs)).To(Succeed())

			_, err := reconcileRS(rs.Name)
			Expect(err).NotTo(HaveOccurred())

			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-defaults", Namespace: testNamespace}, pvc)).To(Succeed())
			Expect(pvc.Spec.StorageClassName).NotTo(BeNil())
			Expect(*pvc.Spec.StorageClassName).To(Equal("managed-premium-zrs"))
			request := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
			Expect(request.Cmp(resource.MustParse("1Gi"))).To(Equal(0))
		})

		It("honours custom storage-class and storage-capacity annotations", func() {
			deployment := newDeployment("dep-custom-storage", "1", map[string]string{
				config.ResourceSuffixAnnotation: "suffix-custom-storage",
				blobPrefixAnnotation:            "blob/prefix",
				volumePathAnnotation:            "/data",
				storageClassAnnotation:          "fast-ssd",
				storageCapacityAnnotation:       "5Gi",
			})
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			rs := newReplicaSet("rs-custom-storage", deployment, "1", 1, 1, "suffix-custom-storage")
			Expect(k8sClient.Create(ctx, rs)).To(Succeed())

			_, err := reconcileRS(rs.Name)
			Expect(err).NotTo(HaveOccurred())

			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-custom-storage", Namespace: testNamespace}, pvc)).To(Succeed())
			Expect(*pvc.Spec.StorageClassName).To(Equal("fast-ssd"))
			request := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
			Expect(request.Cmp(resource.MustParse("5Gi"))).To(Equal(0))
		})

		It("sets the PVC access mode and data source reference", func() {
			deployment := newDeployment("dep-pvc-fields", "1", map[string]string{
				config.ResourceSuffixAnnotation: "suffix-pvc-fields",
				blobPrefixAnnotation:            "blob/prefix",
				volumePathAnnotation:            "/data",
			})
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			rs := newReplicaSet("rs-pvc-fields", deployment, "1", 1, 1, "suffix-pvc-fields")
			Expect(k8sClient.Create(ctx, rs)).To(Succeed())

			_, err := reconcileRS(rs.Name)
			Expect(err).NotTo(HaveOccurred())

			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-pvc-fields", Namespace: testNamespace}, pvc)).To(Succeed())
			Expect(pvc.Spec.AccessModes).To(Equal([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}))
			Expect(pvc.Spec.DataSourceRef).NotTo(BeNil())
			Expect(pvc.Spec.DataSourceRef.APIGroup).NotTo(BeNil())
			Expect(*pvc.Spec.DataSourceRef.APIGroup).To(Equal("volume.pdok.nl"))
			Expect(pvc.Spec.DataSourceRef.Kind).To(Equal("AzureVolumePopulator"))
			Expect(pvc.Spec.DataSourceRef.Name).To(Equal("suffix-pvc-fields"))
		})

		It("sets the AVP spec from the Deployment annotations", func() {
			deployment := newDeployment("dep-avp-fields", "1", map[string]string{
				config.ResourceSuffixAnnotation: "suffix-avp-fields",
				blobPrefixAnnotation:            "some/blob/prefix",
				volumePathAnnotation:            "/mnt/data",
			})
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			rs := newReplicaSet("rs-avp-fields", deployment, "1", 1, 1, "suffix-avp-fields")
			Expect(k8sClient.Create(ctx, rs)).To(Succeed())

			_, err := reconcileRS(rs.Name)
			Expect(err).NotTo(HaveOccurred())

			createdAvp := &avp.AzureVolumePopulator{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-avp-fields", Namespace: testNamespace}, createdAvp)).To(Succeed())
			Expect(createdAvp.Spec.BlobPrefix).To(Equal("some/blob/prefix"))
			Expect(createdAvp.Spec.VolumePath).To(Equal("/mnt/data"))
			Expect(createdAvp.Spec.BlobDownloadOptions).NotTo(BeNil())
		})

		It("creates the PVC referencing an already-existing AVP", func() {
			deployment := newDeployment("dep-partial", "1", map[string]string{
				config.ResourceSuffixAnnotation: "suffix-partial",
				blobPrefixAnnotation:            "blob/prefix",
				volumePathAnnotation:            "/data",
			})
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			rs := newReplicaSet("rs-partial", deployment, "1", 1, 1, "suffix-partial")
			Expect(k8sClient.Create(ctx, rs)).To(Succeed())
			Expect(k8sClient.Create(ctx, newAVP("suffix-partial"))).To(Succeed())

			_, err := reconcileRS(rs.Name)
			Expect(err).NotTo(HaveOccurred())

			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-partial", Namespace: testNamespace}, pvc)).To(Succeed())
			Expect(pvc.Spec.DataSourceRef).NotTo(BeNil())
			Expect(pvc.Spec.DataSourceRef.Name).To(Equal("suffix-partial"))

			avpList := &avp.AzureVolumePopulatorList{}
			Expect(k8sClient.List(ctx, avpList, client.InNamespace(testNamespace))).To(Succeed())
			Expect(avpList.Items).To(HaveLen(1))
		})

		It("returns an empty result without requeue on the happy path", func() {
			deployment := newDeployment("dep-result", "1", map[string]string{
				config.ResourceSuffixAnnotation: "suffix-result",
				blobPrefixAnnotation:            "blob/prefix",
				volumePathAnnotation:            "/data",
			})
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			rs := newReplicaSet("rs-result", deployment, "1", 1, 1, "suffix-result")
			Expect(k8sClient.Create(ctx, rs)).To(Succeed())

			result, err := reconcileRS(rs.Name)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})

		It("returns an error when the owning Deployment does not exist", func() {
			// Deployment is built to wire the owner reference but is never created.
			deployment := newDeployment("dep-missing", "1", map[string]string{
				config.ResourceSuffixAnnotation: "suffix-missing-dep",
				blobPrefixAnnotation:            "blob/prefix",
				volumePathAnnotation:            "/data",
			})

			rs := newReplicaSet("rs-dangling-owner", deployment, "1", 1, 1, "suffix-missing-dep")
			Expect(k8sClient.Create(ctx, rs)).To(Succeed())

			_, err := reconcileRS(rs.Name)
			Expect(err).To(HaveOccurred())
		})
	})

	Context("updating resources", func() {
		It("does not mutate an existing AVP when the Deployment's blob-prefix changes", func() {
			deployment := newDeployment("dep-update-avp", "1", map[string]string{
				config.ResourceSuffixAnnotation: "suffix-update-avp",
				blobPrefixAnnotation:            "blob/v1",
				volumePathAnnotation:            "/data",
			})
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			rs := newReplicaSet("rs-update-avp", deployment, "1", 1, 1, "suffix-update-avp")
			Expect(k8sClient.Create(ctx, rs)).To(Succeed())

			_, err := reconcileRS(rs.Name)
			Expect(err).NotTo(HaveOccurred())

			// Change the annotation on the Deployment (suffix and revision unchanged).
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: testNamespace}, deployment)).To(Succeed())
			deployment.Annotations[blobPrefixAnnotation] = "blob/v2"
			Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

			_, err = reconcileRS(rs.Name)
			Expect(err).NotTo(HaveOccurred())

			createdAvp := &avp.AzureVolumePopulator{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-update-avp", Namespace: testNamespace}, createdAvp)).To(Succeed())
			Expect(createdAvp.Spec.BlobPrefix).To(Equal("blob/v1"))

			avpList := &avp.AzureVolumePopulatorList{}
			Expect(k8sClient.List(ctx, avpList, client.InNamespace(testNamespace))).To(Succeed())
			Expect(avpList.Items).To(HaveLen(1))
		})

		It("does not mutate an existing PVC when the Deployment's storage annotations change", func() {
			deployment := newDeployment("dep-update-pvc", "1", map[string]string{
				config.ResourceSuffixAnnotation: "suffix-update-pvc",
				blobPrefixAnnotation:            "blob/prefix",
				volumePathAnnotation:            "/data",
			})
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			rs := newReplicaSet("rs-update-pvc", deployment, "1", 1, 1, "suffix-update-pvc")
			Expect(k8sClient.Create(ctx, rs)).To(Succeed())

			_, err := reconcileRS(rs.Name)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: testNamespace}, deployment)).To(Succeed())
			deployment.Annotations[storageCapacityAnnotation] = "10Gi"
			deployment.Annotations[storageClassAnnotation] = "fast-ssd"
			Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

			_, err = reconcileRS(rs.Name)
			Expect(err).NotTo(HaveOccurred())

			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-update-pvc", Namespace: testNamespace}, pvc)).To(Succeed())
			request := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
			Expect(request.Cmp(resource.MustParse("1Gi"))).To(Equal(0))
			Expect(*pvc.Spec.StorageClassName).To(Equal("managed-premium-zrs"))
		})

		It("provisions new resources and removes the old ones when the resource-suffix changes", func() {
			deployment := newDeployment("dep-rollout", "2", map[string]string{
				config.ResourceSuffixAnnotation: "suffix-rollout-new",
				blobPrefixAnnotation:            "blob/v2",
				volumePathAnnotation:            "/data",
			})
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			oldRs := newReplicaSet("rs-rollout-old", deployment, "1", 0, 0, "suffix-rollout-old")
			Expect(k8sClient.Create(ctx, oldRs)).To(Succeed())
			newRs := newReplicaSet("rs-rollout-new", deployment, "2", 1, 1, "suffix-rollout-new")
			Expect(k8sClient.Create(ctx, newRs)).To(Succeed())

			Expect(k8sClient.Create(ctx, newAVP("suffix-rollout-old"))).To(Succeed())
			Expect(k8sClient.Create(ctx, newPVC("suffix-rollout-old", ""))).To(Succeed())

			_, err := reconcileRS(newRs.Name)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-rollout-new", Namespace: testNamespace}, &avp.AzureVolumePopulator{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-rollout-new", Namespace: testNamespace}, &corev1.PersistentVolumeClaim{})).To(Succeed())

			err = k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-rollout-old", Namespace: testNamespace}, &avp.AzureVolumePopulator{})
			Expect(err).To(HaveOccurred())
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-rollout-old", Namespace: testNamespace}, &corev1.PersistentVolumeClaim{})
			Expect(err).To(HaveOccurred())
		})
	})

	Context("deleting resources", func() {
		It("keeps an old ReplicaSet's resources while another ReplicaSet still uses the same suffix", func() {
			deployment := newDeployment("dep-shared", "2", map[string]string{
				config.ResourceSuffixAnnotation: "suffix-current",
				blobPrefixAnnotation:            "blob/prefix",
				volumePathAnnotation:            "/data",
			})
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			scaledDownRs := newReplicaSet("rs-shared-down", deployment, "1", 0, 0, "suffix-shared")
			Expect(k8sClient.Create(ctx, scaledDownRs)).To(Succeed())
			scaledUpRs := newReplicaSet("rs-shared-up", deployment, "1", 2, 2, "suffix-shared")
			Expect(k8sClient.Create(ctx, scaledUpRs)).To(Succeed())
			currentRs := newReplicaSet("rs-shared-current", deployment, "2", 1, 1, "suffix-current")
			Expect(k8sClient.Create(ctx, currentRs)).To(Succeed())

			Expect(k8sClient.Create(ctx, newAVP("suffix-shared"))).To(Succeed())
			Expect(k8sClient.Create(ctx, newPVC("suffix-shared", ""))).To(Succeed())

			_, err := reconcileRS(currentRs.Name)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-shared", Namespace: testNamespace}, &avp.AzureVolumePopulator{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-shared", Namespace: testNamespace}, &corev1.PersistentVolumeClaim{})).To(Succeed())
		})

		It("keeps old resources while the current PVC is still being populated", func() {
			deployment := newDeployment("dep-populating", "2", map[string]string{
				config.ResourceSuffixAnnotation: "suffix-populating-new",
				blobPrefixAnnotation:            "blob/prefix",
				volumePathAnnotation:            "/data",
			})
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			oldRs := newReplicaSet("rs-populating-old", deployment, "1", 0, 0, "suffix-populating-old")
			Expect(k8sClient.Create(ctx, oldRs)).To(Succeed())
			newRs := newReplicaSet("rs-populating-new", deployment, "2", 1, 1, "suffix-populating-new")
			Expect(k8sClient.Create(ctx, newRs)).To(Succeed())

			Expect(k8sClient.Create(ctx, newAVP("suffix-populating-old"))).To(Succeed())
			Expect(k8sClient.Create(ctx, newPVC("suffix-populating-old", ""))).To(Succeed())
			// The current PVC already exists and is still pending (being populated).
			Expect(k8sClient.Create(ctx, newPVC("suffix-populating-new", corev1.ClaimPending))).To(Succeed())

			_, err := reconcileRS(newRs.Name)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-populating-old", Namespace: testNamespace}, &avp.AzureVolumePopulator{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-populating-old", Namespace: testNamespace}, &corev1.PersistentVolumeClaim{})).To(Succeed())
		})

		It("keeps an old ReplicaSet's resources while it still has replicas", func() {
			deployment := newDeployment("dep-oldscaled", "2", map[string]string{
				config.ResourceSuffixAnnotation: "suffix-oldscaled-new",
				blobPrefixAnnotation:            "blob/prefix",
				volumePathAnnotation:            "/data",
			})
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			oldRs := newReplicaSet("rs-oldscaled-old", deployment, "1", 1, 1, "suffix-oldscaled-old")
			Expect(k8sClient.Create(ctx, oldRs)).To(Succeed())
			newRs := newReplicaSet("rs-oldscaled-new", deployment, "2", 1, 1, "suffix-oldscaled-new")
			Expect(k8sClient.Create(ctx, newRs)).To(Succeed())

			Expect(k8sClient.Create(ctx, newAVP("suffix-oldscaled-old"))).To(Succeed())
			Expect(k8sClient.Create(ctx, newPVC("suffix-oldscaled-old", ""))).To(Succeed())

			_, err := reconcileRS(newRs.Name)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-oldscaled-old", Namespace: testNamespace}, &avp.AzureVolumePopulator{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-oldscaled-old", Namespace: testNamespace}, &corev1.PersistentVolumeClaim{})).To(Succeed())
		})

		It("deletes the resources of every scaled-down old ReplicaSet in one reconcile", func() {
			deployment := newDeployment("dep-multi", "3", map[string]string{
				config.ResourceSuffixAnnotation: "suffix-multi-new",
				blobPrefixAnnotation:            "blob/prefix",
				volumePathAnnotation:            "/data",
			})
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			oldRs1 := newReplicaSet("rs-multi-old1", deployment, "1", 0, 0, "suffix-multi-old1")
			Expect(k8sClient.Create(ctx, oldRs1)).To(Succeed())
			oldRs2 := newReplicaSet("rs-multi-old2", deployment, "2", 0, 0, "suffix-multi-old2")
			Expect(k8sClient.Create(ctx, oldRs2)).To(Succeed())
			newRs := newReplicaSet("rs-multi-new", deployment, "3", 1, 1, "suffix-multi-new")
			Expect(k8sClient.Create(ctx, newRs)).To(Succeed())

			for _, name := range []string{"suffix-multi-old1", "suffix-multi-old2"} {
				Expect(k8sClient.Create(ctx, newAVP(name))).To(Succeed())
				Expect(k8sClient.Create(ctx, newPVC(name, ""))).To(Succeed())
			}

			_, err := reconcileRS(newRs.Name)
			Expect(err).NotTo(HaveOccurred())

			for _, name := range []string{"suffix-multi-old1", "suffix-multi-old2"} {
				err = k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, &avp.AzureVolumePopulator{})
				Expect(err).To(HaveOccurred())
				err = k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, &corev1.PersistentVolumeClaim{})
				Expect(err).To(HaveOccurred())
			}
		})

		It("does not error when a scaled-down old ReplicaSet has no resources to delete", func() {
			deployment := newDeployment("dep-noresources", "2", map[string]string{
				config.ResourceSuffixAnnotation: "suffix-noresources-new",
				blobPrefixAnnotation:            "blob/prefix",
				volumePathAnnotation:            "/data",
			})
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			oldRs := newReplicaSet("rs-noresources-old", deployment, "1", 0, 0, "suffix-noresources-old")
			Expect(k8sClient.Create(ctx, oldRs)).To(Succeed())
			newRs := newReplicaSet("rs-noresources-new", deployment, "2", 1, 1, "suffix-noresources-new")
			Expect(k8sClient.Create(ctx, newRs)).To(Succeed())

			_, err := reconcileRS(newRs.Name)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-noresources-old", Namespace: testNamespace}, &avp.AzureVolumePopulator{})
			Expect(err).To(HaveOccurred())
		})

		It("keeps the shared resources when a new ReplicaSet reuses the old resource-suffix", func() {
			deployment := newDeployment("dep-samehash", "2", map[string]string{
				config.ResourceSuffixAnnotation: "suffix-samehash",
				blobPrefixAnnotation:            "blob/prefix",
				volumePathAnnotation:            "/data",
			})
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			oldRs := newReplicaSet("rs-samehash-old", deployment, "1", 0, 0, "suffix-samehash")
			Expect(k8sClient.Create(ctx, oldRs)).To(Succeed())
			newRs := newReplicaSet("rs-samehash-new", deployment, "2", 1, 1, "suffix-samehash")
			Expect(k8sClient.Create(ctx, newRs)).To(Succeed())

			_, err := reconcileRS(newRs.Name)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-samehash", Namespace: testNamespace}, &avp.AzureVolumePopulator{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "suffix-samehash", Namespace: testNamespace}, &corev1.PersistentVolumeClaim{})).To(Succeed())
		})
	})
})
