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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	testNamespace        = "default"
	blobPrefixAnnotation = "volume-operator.pdok.nl/blob-prefix"
	volumePathAnnotation = "volume-operator.pdok.nl/volume-path"
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
})
