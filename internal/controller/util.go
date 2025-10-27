package controller

import (
	"context"
	"fmt"
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
