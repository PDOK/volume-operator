package controller

import (
	"context"
	"fmt"

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

func deploymentHasVolume(deployment *appsv1.Deployment, volumeName string) bool {
	for _, vol := range deployment.Spec.Template.Spec.Volumes {
		if vol.Name == volumeName {
			return true
		}
	}
	return false
}

func StringPtr(s string) *string {
	return &s
}
