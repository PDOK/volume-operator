package controller

import (
	"context"
	"fmt"

	"github.com/PDOK/volume-operator/internal/config"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func deleteObjects(ctx context.Context, c client.Client, objects []client.Object) (err error) {
	for _, obj := range objects {
		fullName := getObjectFullName(c, obj)
		err = client.IgnoreNotFound(c.Delete(ctx, obj))
		if err != nil {
			return fmt.Errorf("unable to delete resource %s: %w", fullName, err)
		}
	}
	return
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

func StringPtr(s string) *string {
	return &s
}
