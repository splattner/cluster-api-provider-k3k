# capi-k3k

A Cluster API provider for [K3k](https://github.com/rancher/k3k).

## Description

This project allows the provisioning and management of K3k clusters using [CAPI](https://cluster-api.sigs.k8s.io).

## Usage

A `K3kControlPlane` creates a K3k cluster on a host cluster (the management cluster, or the cluster referenced by
`spec.hostKubeconfig`). Its spec embeds the K3k [ClusterSpec](https://github.com/rancher/k3k/blob/main/docs/crds/crds.md),
and the `mode` selects how the K3k cluster runs:

- `shared` (default) and `virtual`: the nodes are managed by K3k on the host cluster, use a `K3kCluster` as
  infrastructure. See [templates/cluster-template.yaml](templates/cluster-template.yaml).
- `hcp`: K3k runs only the control plane, the worker nodes are Cluster API Machines of any infrastructure provider,
  joined with the k3s bootstrap provider. See [Hosted Control Plane (HCP) mode](docs/hcp-mode.md).

## Getting Started

### Prerequisites

The following specify the minimum version requirement of each component:

- Go v1.26
- Docker v24
- Access to a Kubernetes cluster v1.25
- Tilt v0.33.11
- Helm v3.12.0

### Run from development branch

Follow the [upstream docs](https://cluster-api.sigs.k8s.io/developer/tilt) to run the project locally. The basic steps:

#### Install [Tilt](https://tilt.dev)

#### Deploy a Kubernetes cluster

Any cluster should work. For quick local tests, Kind and K3d are recommended.

#### Create a local Docker container registry

This is necessary to avoid pushing to a remote repo frequently.

For K3d:

```bash
k3d registry create --port 51111 # Or use a different port.
```

#### Create a cluster using the registry

```bash
k3d cluster create --registry-use $CONTAINER_NAME:$PORT
```


#### Clone the upstream [CAPI](https://github.com/kubernetes-sigs/cluster-api) project

In your local copy of the project, paste the following into `tilt-settings.yaml`:

```yaml
provider_repos:
  - ../cluster-api-provider-k3k
enable_providers:
  - k3k
allowed_contexts:
  - k3d-k3s-default
kustomize_substitutions:
  CLUSTER_TOPOLOGY: "true"
```

The last two lines are only necessary if testing using cluster classes.

Some notes:

- The above assumes that the upstream CAPI project is in the same folder as the cluster-api-provider-k3k project. If
  that isn't true, adjust the path in `provider_repos`.
- `allowed_contexts` controls which kubeconfig contexts Tilt is able to use to deploy resources.
  This needs to point to a context for an admin account on the cluster. You may need to change this value depending on
  your current kubeconfig contents.

#### In the local copy of the CAPI upstream project, run Tilt

```bash
export IMG="$CONTAINER_NAME:$PORT/controller:latest"
tilt up
```

Most code changes will cause Tilt to automatically rebuild the Go binary, rebuild the Docker image
and push it to the local registry.
However, if you are changing values related to RBAC configuration or CRD fields, you may need to do the following:

- Stop Tilt
- In this project's repo, regenerate the CRDs and manifests (`make generate && make manifests`)
- Start Tilt again

## Project Distribution

TODO: This project is in an unreleased, alpha state. As of now, there are no releases, or a supported way to install the
project other than the dev setup documented above.

## Contributing

All changes should have an issue attached to them, and should have tests (where appropriate).

Don't use `''` (empty string) in `+kubebuilder:validation:XValidation` rules: the Go doc comment formatting of
`make lint-fix` turns it into `”`, which makes the CRD invalid. Use e.g. `size(self.field) > 0` instead.

## License

Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
