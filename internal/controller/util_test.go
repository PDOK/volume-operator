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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/PDOK/volume-operator/internal/config"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func helperReplicaSet(name, revision, suffix string, replicas *int32, available int32) appsv1.ReplicaSet {
	return appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Annotations: map[string]string{
				config.RevisionAnnotation:       revision,
				config.ResourceSuffixAnnotation: suffix,
			},
		},
		Spec:   appsv1.ReplicaSetSpec{Replicas: replicas},
		Status: appsv1.ReplicaSetStatus{AvailableReplicas: available},
	}
}

func int32Ptr(i int32) *int32 { return &i }

var _ = Describe("Controller helpers", func() {
	Describe("hasReplicas", func() {
		It("is false when Replicas is nil", func() {
			Expect(hasReplicas(helperReplicaSet("rs", "1", "s", nil, 0))).To(BeFalse())
		})

		It("is false when Replicas is zero", func() {
			Expect(hasReplicas(helperReplicaSet("rs", "1", "s", int32Ptr(0), 0))).To(BeFalse())
		})

		It("is true when Replicas is greater than zero", func() {
			Expect(hasReplicas(helperReplicaSet("rs", "1", "s", int32Ptr(2), 0))).To(BeTrue())
		})
	})

	Describe("currentReplicaSetIsAvailable", func() {
		It("is false when the deployment revision is empty", func() {
			list := appsv1.ReplicaSetList{Items: []appsv1.ReplicaSet{
				helperReplicaSet("rs", "1", "s", int32Ptr(1), 1),
			}}
			Expect(currentReplicaSetIsAvailable(list, "")).To(BeFalse())
		})

		It("is false when no ReplicaSet carries the deployment revision", func() {
			list := appsv1.ReplicaSetList{Items: []appsv1.ReplicaSet{
				helperReplicaSet("rs", "1", "s", int32Ptr(1), 1),
			}}
			Expect(currentReplicaSetIsAvailable(list, "2")).To(BeFalse())
		})

		It("is false when the matching ReplicaSet has no available replicas", func() {
			list := appsv1.ReplicaSetList{Items: []appsv1.ReplicaSet{
				helperReplicaSet("rs", "2", "s", int32Ptr(1), 0),
			}}
			Expect(currentReplicaSetIsAvailable(list, "2")).To(BeFalse())
		})

		It("is true when the matching ReplicaSet has available replicas", func() {
			list := appsv1.ReplicaSetList{Items: []appsv1.ReplicaSet{
				helperReplicaSet("rs", "2", "s", int32Ptr(1), 3),
			}}
			Expect(currentReplicaSetIsAvailable(list, "2")).To(BeTrue())
		})
	})

	Describe("resourceIsUsedByOtherReplicaSet", func() {
		current := helperReplicaSet("current", "1", "shared", int32Ptr(0), 0)

		It("ignores the ReplicaSet with the same name", func() {
			list := appsv1.ReplicaSetList{Items: []appsv1.ReplicaSet{
				helperReplicaSet("current", "1", "shared", int32Ptr(5), 5),
			}}
			Expect(resourceIsUsedByOtherReplicaSet(list, current)).To(BeFalse())
		})

		It("is false when the shared suffix is empty", func() {
			empty := helperReplicaSet("current", "1", "", int32Ptr(0), 0)
			list := appsv1.ReplicaSetList{Items: []appsv1.ReplicaSet{
				helperReplicaSet("other", "1", "", int32Ptr(5), 5),
			}}
			Expect(resourceIsUsedByOtherReplicaSet(list, empty)).To(BeFalse())
		})

		It("is false when the other ReplicaSet with the same suffix has no replicas", func() {
			list := appsv1.ReplicaSetList{Items: []appsv1.ReplicaSet{
				helperReplicaSet("other", "2", "shared", int32Ptr(0), 0),
			}}
			Expect(resourceIsUsedByOtherReplicaSet(list, current)).To(BeFalse())
		})

		It("is true when another ReplicaSet with the same suffix still has replicas", func() {
			list := appsv1.ReplicaSetList{Items: []appsv1.ReplicaSet{
				helperReplicaSet("other", "2", "shared", int32Ptr(1), 1),
			}}
			Expect(resourceIsUsedByOtherReplicaSet(list, current)).To(BeTrue())
		})
	})
})
