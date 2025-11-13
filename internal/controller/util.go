package controller

import (
	"github.com/PDOK/volume-operator/internal/config"
	appsv1 "k8s.io/api/apps/v1"
)

func resourceIsUsedByOtherReplicaSet(rsList appsv1.ReplicaSetList, currentRs appsv1.ReplicaSet) bool {
	for _, rs := range rsList.Items {
		if rs.Name == currentRs.Name {
			continue
		}
		currentResource := currentRs.Annotations[config.ResourceSuffixAnnotation]
		resource := rs.Annotations[config.ResourceSuffixAnnotation]
		if currentResource == resource {
			return true
		}
	}

	return false
}
