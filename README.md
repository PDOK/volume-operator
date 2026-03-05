# volume-operator

_Kubernetes controller/operator to manage Azure-backed volumes for Deployments._

[![Build](https://github.com/PDOK/volume-operator/actions/workflows/build-and-publish-image.yml/badge.svg)](https://github.com/PDOK/volume-operator/actions/workflows/build-and-publish-image.yml)
[![Lint (go)](https://github.com/PDOK/volume-operator/actions/workflows/lint-go.yml/badge.svg)](https://github.com/PDOK/volume-operator/actions/workflows/lint-go.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/PDOK/volume-operator)](https://goreportcard.com/report/github.com/PDOK/volume-operator)
[![GitHub license](https://img.shields.io/github/license/PDOK/volume-operator)](https://github.com/PDOK/volume-operator/blob/master/LICENSE)
[![Docker Pulls](https://img.shields.io/docker/pulls/pdok/volume-operator.svg)](https://hub.docker.com/r/pdok/volume-operator)

## Description

This Kubernetes controller cq operator (an operator could be described as a specialized controller)
automatically provisions Azure-backed persistent volumes for Deployments by watching ReplicaSets
and creating the necessary storage resources based on annotations placed on the owning Deployment.

The operator integrates with [azure-volume-populator](https://github.com/pdok/azure-volume-populator)
to populate volumes from Azure Blob Storage. Instead of a Custom Resource Definition, configuration
is expressed through annotations on the Deployment itself, making adoption straightforward without
changes to existing manifests beyond adding the relevant annotations.

The operator watches ReplicaSets and, for each active ReplicaSet belonging to an annotated Deployment,
creates the following resources in the same namespace:

* An `AzureVolumePopulator` CR, which instructs the azure-volume-populator to populate the volume
  from a blob prefix in Azure Blob Storage
* A `PersistentVolumeClaim` backed by the above populator and configured with the requested
  storage class and capacity

When a Deployment rolls out a new ReplicaSet (e.g. during a rolling update), the operator cleans up
the `AzureVolumePopulator`, `PersistentVolumeClaim`, and old ReplicaSet belonging to the previous revision,
preventing stale storage resources from accumulating in the cluster.

### Annotations

All annotations use the prefix `volume-operator.pdok.nl`. The following annotations are read from
the **Deployment**:

| Annotation | Required | Default | Description |
|---|---|---|---|
| `volume-operator.pdok.nl/resource-suffix` | yes | — | Name used for the created `AzureVolumePopulator` and `PersistentVolumeClaim` |
| `volume-operator.pdok.nl/blob-prefix` | yes | — | Blob prefix in Azure Blob Storage to populate the volume from |
| `volume-operator.pdok.nl/volume-path` | yes | — | Path inside the volume to populate |
| `volume-operator.pdok.nl/storage-capacity` | no | `1Gi` | Size of the requested `PersistentVolumeClaim` |
| `volume-operator.pdok.nl/storage-class-name` | no | `managed-premium-zrs` | Storage class to use for the `PersistentVolumeClaim` |

## Run/usage

```shell
go build github.com/PDOK/volume-operator/cmd -o manager
```

or

```shell
docker build -t pdok/volume-operator .
```

```text
USAGE:
   <volume-controller-manager> [OPTIONS]

OPTIONS:
  -h, --help
        Display these options.
  --enable-http2
        If set, HTTP/2 will be enabled for the metrics and webhook servers
  --health-probe-bind-address string
        The address the probe endpoint binds to. (default ":8081")
  --kubeconfig string
        Paths to a kubeconfig. Only required if out-of-cluster.
        Alternatively use the KUBECONFIG environment variable.
  --leader-elect
        Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.
  --metrics-bind-address string
        The address the metrics endpoint binds to. Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable. (default "0")
  --metrics-cert-key string
        The name of the metrics server key file. (default "tls.key")
  --metrics-cert-name string
        The name of the metrics server certificate file. (default "tls.crt")
  --metrics-cert-path string
        The directory that contains the metrics server certificate.
  --metrics-secure
        If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead. (default true)
  --webhook-cert-key string
        The name of the webhook key file. (default "tls.key")
  --webhook-cert-name string
        The name of the webhook certificate file. (default "tls.crt")
  --webhook-cert-path string
        The directory that contains the webhook certificate.
  --zap-devel
        Development Mode defaults(encoder=consoleEncoder,logLevel=Debug,stackTraceLevel=Warn). Production Mode defaults(encoder=jsonEncoder,logLevel=Info,stackTraceLevel=Error) (default true)
  --zap-encoder value
        Zap log encoding (one of 'json' or 'console')
  --zap-log-level value
        Zap Level to configure the verbosity of logging. Can be one of 'debug', 'info', 'error', or any integer value > 0 which corresponds to custom debug levels of increasing verbosity
  --zap-stacktrace-level value
        Zap Level at and above which stacktraces are captured (one of 'info', 'error', 'panic').
  --zap-time-encoding value
        Zap time encoding (one of 'epoch', 'millis', 'nano', 'iso8601', 'rfc3339' or 'rfc3339nano'). Defaults to 'epoch'.

OPTIONS can also be set via environment variables (with SCREAMING_SNAKE_CASE instead of kebab-case). CLI flags have precedence.
```

### Setup in the cluster

This depends on your own way of maintaining your cluster config.
Inspiration can be found in the [config](./config) dir
which holds "kustomizations" to use with Kustomize.
You could use `make install` for this.
Please refer to the general documentation in the [kubebuilder book](https://kubebuilder.io) for more info.

## Develop

The project is written in Go and scaffolded with [kubebuilder](https://kubebuilder.io).

### Running locally

See [running and deploying the controller](https://kubebuilder.io/cronjob-tutorial/running) for details.

### kubebuilder

This project is scaffolded using Kubebuilder, to update the scaffolding:
- Install the latest version of Kubebuilder on your machine;
- Run: `kubebuilder alpha update --from-branch master`
- This command will probably fail with the message that you should resolve merge conflicts and then run `make fmt vet <some other command>`. This is not a problem. Resolve the conflicts and run the command.

### Linting

Run `make lint` from the root of the project.

## Misc

### How to Contribute

See [CONTRIBUTING.md](https://github.com/pdok/.github/blob/main/CONTRIBUTING.md).

### Contact

Contacting the maintainers can be done through the issue tracker.

