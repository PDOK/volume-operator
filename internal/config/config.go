package config

import (
	avp "github.com/pdok/azure-volume-populator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
)

const (
	annotationPrefix = "volume-operator.pdok.nl"

	blobPrefixAnnotation       = annotationPrefix + "/blob-prefix"
	volumePathAnnotation       = annotationPrefix + "/volume-path"
	storageCapacityAnnotation  = annotationPrefix + "/storage-capacity"
	storageClassNameAnnotation = annotationPrefix + "/storage-class-name"
	defaultStorageClass        = "managed-premium-zrs"

	RevisionAnnotation       = "deployment.kubernetes.io/revision"
	ResourceSuffixAnnotation = annotationPrefix + "/resource-suffix"
)

type Config struct {
	ResourceName        string
	ResourceNamespace   string
	ReplicaSetRevision  string
	DeploymentRevision  string
	BlobPrefix          string
	VolumePath          string
	StorageCapacity     string
	StorageClassName    string
	BlobDownloadOptions *avp.BlobDownloadOptions
}

func NewConfigFromAnnotations(d *appsv1.Deployment, rs *appsv1.ReplicaSet) Config {
	vpc := Config{
		ResourceName:       d.Annotations[ResourceSuffixAnnotation],
		ResourceNamespace:  rs.GetNamespace(),
		ReplicaSetRevision: rs.Annotations[RevisionAnnotation],
		DeploymentRevision: d.Annotations[RevisionAnnotation],
		BlobPrefix:         d.Annotations[blobPrefixAnnotation],
		VolumePath:         d.Annotations[volumePathAnnotation],
		StorageCapacity:    d.Annotations[storageCapacityAnnotation],
		StorageClassName:   d.Annotations[storageClassNameAnnotation],
	}

	if vpc.StorageClassName == "" {
		vpc.StorageClassName = defaultStorageClass
	}

	if vpc.StorageCapacity == "" {
		vpc.StorageCapacity = "1Gi"
	}

	return vpc
}

func (c Config) RevisionsMatch() bool {
	return c.ReplicaSetRevision != "" && c.DeploymentRevision != "" && c.ReplicaSetRevision == c.DeploymentRevision
}

func (c Config) HasRequiredAnnotations() bool {
	return c.BlobPrefix != "" && c.VolumePath != "" && c.StorageCapacity != ""
}
