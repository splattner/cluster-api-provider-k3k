# Hosted Control Plane (HCP) mode

In `hcp` mode, [K3k](https://github.com/rancher/k3k) runs an agentless K3s control plane inside the host cluster, and
the worker nodes join it from outside. With this provider, the worker nodes are regular Cluster API Machines:

- the **K3kControlPlane** provisions the K3k cluster in `hcp` mode on the host cluster,
- **any infrastructure provider** (vSphere, OpenStack, Metal3, Docker, ...) provisions the worker Machines,
- the **k3s bootstrap provider** ([cluster-api-k3s](https://github.com/k3s-io/cluster-api-k3s) `KThreesConfig`) joins
  them to the K3k control plane.

```
 host cluster                                   worker infrastructure
┌──────────────────────────────────┐           ┌──────────────────────────────┐
│ K3kControlPlane ──► K3k Cluster  │  K3S_URL  │ MachineDeployment            │
│                     (hcp mode)   │◄──────────┤   InfraMachine (any provider)│
│                     K3s server ──┼─ exposed ─┤   KThreesConfig (k3s agent)  │
└──────────────────────────────────┘ endpoint  └──────────────────────────────┘
```

> [!NOTE]
> The `hcp` mode is experimental in K3k (v1.2.0+).

## Requirements

- K3k v1.2.0 or newer on the host cluster. The provider installs the K3k chart `1.2.0` when K3k is not installed yet,
  and upgrades the `k3k` Helm release in `k3k-system` if it installed it. A K3k installed otherwise is not upgraded:
  the `K3kControllerUpToDate` condition of the K3kControlPlane reports when it's outdated.
- The k3s bootstrap provider. It is not in the clusterctl built-in provider list, add it to the clusterctl
  configuration (`~/.config/cluster-api/clusterctl.yaml`):

  ```yaml
  providers:
  - name: k3s
    type: BootstrapProvider
    url: https://github.com/k3s-io/cluster-api-k3s/releases/download/v0.4.0/bootstrap-components.yaml
  ```

  and install it with `clusterctl init --bootstrap k3s`.
- An infrastructure provider for the worker Machines, e.g. `clusterctl init --infrastructure vsphere`.
- The worker Machines must be able to reach the K3k server through the control plane endpoint (see below).

## Configuration

A HCP cluster has:

- a `Cluster` with the **control plane endpoint set upfront**, the `K3kControlPlane` as `controlPlaneRef`, and the
  infrastructure cluster object of the worker provider (e.g. `VSphereCluster`) as `infrastructureRef`,
- a `K3kControlPlane` with `mode: hcp`,
- `MachineDeployments` (or `MachinePools`) using a `KThreesConfigTemplate` and the infrastructure provider's machine
  template.

### Control plane endpoint

Set `Cluster.spec.controlPlaneEndpoint` to the address where the worker Machines reach the K3k server:

```yaml
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
spec:
  controlPlaneEndpoint:
    host: k3k.example.com
    port: 443
```

Many infrastructure providers create their own control plane endpoint, for example a load balancer, and publish it on
their infrastructure cluster object. Cluster API copies the first endpoint it finds to the Cluster, and the
infrastructure is usually ready long before the K3k server: without an endpoint set upfront, the workers would try to
join that (empty) load balancer.

The K3kControlPlane uses the endpoint as follows:

- its host is added as the first TLS SAN of the K3s server. K3k uses it to point the in-cluster `kubernetes` Service to
  the external address, so Pods on the worker nodes can reach the API server;
- the kubeconfig of the cluster (`<cluster>-kubeconfig`) points to it;
- if K3k is not exposed at the endpoint, the control plane is not initialized: the `Available` condition of the
  K3kControlPlane reports `EndpointMismatch` with the address K3k is exposed on, and no worker joins.

If the endpoint is not set, the provider derives it from the K3k exposure and the first non-loopback TLS SAN. This only
works with infrastructure providers that don't set an endpoint themselves.

### K3kControlPlane

```yaml
apiVersion: controlplane.cluster.x-k8s.io/v1beta1
kind: K3kControlPlane
spec:
  mode: hcp
  servers: 1
  version: v1.35.0-k3s1
  expose:
    nodePort:
      serverPort: 30443
```

- `version` is required: Cluster API needs the control plane version to create worker Machines.
- `expose` is required and must make the K3s server reachable by the workers, at the control plane endpoint:

  | expose         | control plane endpoint                                                                 |
  |----------------|----------------------------------------------------------------------------------------|
  | `nodePort`     | a host cluster node address, and `serverPort` as port (it must be in the NodePort range) |
  | `loadBalancer` | the load balancer address (or a name resolving to it), and `serverPort` (default 443) as port |
  | `ingress`      | a DNS name, and port 443. The Ingress controller must pass TLS through to the K3s server |

  Only `nodePort` is covered by the e2e tests.
- The K3s embedded cloud controller manager is disabled (`--disable-cloud-controller` is added to `serverArgs`), see
  [Node providerIDs](#node-providerids). To keep it, set `--disable-cloud-controller=false` in `serverArgs`.

### Worker Machines

The worker Machines use the K3s version format with a `+`, while the K3kControlPlane uses the K3k (image tag) format
with a `-`:

```yaml
apiVersion: cluster.x-k8s.io/v1beta2
kind: MachineDeployment
spec:
  template:
    spec:
      version: v1.35.0+k3s1
      bootstrap:
        configRef:
          apiGroup: bootstrap.cluster.x-k8s.io
          kind: KThreesConfigTemplate
          name: my-cluster-md-0
      infrastructureRef:
        apiGroup: infrastructure.cluster.x-k8s.io
        kind: VSphereMachineTemplate
        name: my-cluster-md-0
---
apiVersion: bootstrap.cluster.x-k8s.io/v1beta2
kind: KThreesConfigTemplate
spec:
  template:
    spec: {}
```

The K3k join token is copied to the `<cluster>-token` Secret, where `KThreesConfig` reads it. The server URL is the
Cluster control plane endpoint.

### Node providerIDs

Cluster API matches Machines to Nodes with `spec.providerID`. The K3s embedded cloud controller manager sets
`k3s://<node>` providerIDs, which don't match any infrastructure provider, so it is disabled by default.

By default, `KThreesConfig` runs the kubelet with `--cloud-provider=external` (`serverConfig.cloudProviderName`
defaults to `external`, also for agents). The kubelet then registers the node with the
`node.cloudprovider.kubernetes.io/uninitialized` taint, which only a cloud controller manager removes. The k3s agent
waits for the taint to be removed before it reports ready: until then, `systemctl start k3s-agent` doesn't return,
the bootstrap (cloud-init) doesn't finish, and infrastructure providers that check the bootstrap result (e.g.
[CAPK](https://github.com/kubernetes-sigs/cluster-api-provider-kubevirt)) never mark the Machine as bootstrapped.

Pick one of these setups:

- **The cloud controller manager of the infrastructure** (e.g. the vSphere CPI or the OpenStack CCM), deployed in the
  K3k cluster, for example with a [ClusterResourceSet](https://cluster-api.sigs.k8s.io/tasks/cluster-resource-set) or
  the [Cluster API Add-on Provider for Helm](https://github.com/kubernetes-sigs/cluster-api-addon-provider-helm). Keep
  the default `cloudProviderName`: the cloud controller manager sets the providerID and removes the taint.
- **No cloud controller manager**: disable the external cloud provider with an empty `cloudProviderName`, and let the
  kubelet set the providerID with its `provider-id` argument, if the infrastructure provider can render it. For
  example with KubeVirt workers ([CAPK](https://github.com/kubernetes-sigs/cluster-api-provider-kubevirt)), where the
  VM name is the hostname from the cloud-init metadata (`KThreesConfig` renders cloud-init as Jinja):

  ```yaml
  apiVersion: bootstrap.cluster.x-k8s.io/v1beta2
  kind: KThreesConfigTemplate
  spec:
    template:
      spec:
        serverConfig:
          cloudProviderName: ""
        agentConfig:
          kubeletArgs:
            - provider-id=kubevirt://{{ ds.meta_data.local_hostname }}
  ```

- **The infrastructure provider sets the providerID and removes the taint itself.** CAPD does, so the
  [`hcp-docker`](#example-docker-workers) template keeps the defaults.

Otherwise the nodes join but keep the taint, no Pods are scheduled on them, and the Machines stay `Provisioning`.

## Example: Docker workers

The [`hcp-docker`](../templates/cluster-template-hcp-docker.yaml) template creates a HCP cluster with [CAPD](https://github.com/kubernetes-sigs/cluster-api/tree/main/test/infrastructure/docker)
worker Machines, exposed with a NodePort on a node of the host (management) cluster. It is meant for local tests, for
example with a kind management cluster:

With Cluster API and the K3k provider installed (see the [README](../README.md)), and the k3s bootstrap provider in
the clusterctl configuration (see [Requirements](#requirements)):

```bash
clusterctl init --infrastructure docker --bootstrap k3s

# a node address of the management cluster, reachable by the Docker Machines
export HCP_ENDPOINT_HOST=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
export KUBERNETES_VERSION=v1.35.0

clusterctl generate cluster my-hcp --from templates/cluster-template-hcp-docker.yaml --worker-machine-count 1 | kubectl apply -f -
```

With kind, the management cluster must be created with the Docker socket mounted (see the
[CAPD quick start](https://cluster-api.sigs.k8s.io/user/quick-start)), and Docker Machines may need higher inotify
limits on the host:

```bash
sudo sysctl -w fs.inotify.max_user_instances=512 fs.inotify.max_user_watches=524288
```

## Troubleshooting

| Symptom | Cause |
|---------|-------|
| `version is required in hcp mode` / `expose must be set ...` when creating the K3kControlPlane | `spec.version` and `spec.expose` are required in `hcp` mode. |
| `spec.mode: Unsupported value: "hcp"` when the K3k cluster is created, `K3kControllerUpToDate` is `False` | K3k on the host cluster is older than v1.2.0, and was not installed by the provider: upgrade it. |
| K3kControlPlane `Available` is `False` with reason `EndpointMismatch` | K3k is exposed at a different address than the Cluster control plane endpoint, e.g. a different `expose.nodePort.serverPort`. The message shows both addresses. |
| Machines wait for the bootstrap data | The `<cluster>-token` Secret is missing: check the K3kControlPlane events and the K3k token Secret in the host namespace. |
| Machines stay `Provisioning`, Nodes exist without providerID | No cloud controller manager sets the Node providerIDs, see [Node providerIDs](#node-providerids). |
| Machines stay `Provisioning`, Nodes are tainted `node.cloudprovider.kubernetes.io/uninitialized`, the k3s agent logs `waiting for removal of node.cloudprovider.kubernetes.io/uninitialized taint` | The kubelet runs with `--cloud-provider=external` and no cloud controller manager removes the taint, so the agent and the bootstrap never finish. Deploy one, or set `serverConfig.cloudProviderName: ""`, see [Node providerIDs](#node-providerids). |
| Nodes join, but Pods can't reach the API server | The control plane endpoint is not reachable from the Pods on the worker nodes, or it is not the first TLS SAN. |

## Limitations

- ClusterClass: the K3kControlPlane and the worker Machines use different version formats (`v1.35.0-k3s1` and
  `v1.35.0+k3s1`), so a single `topology.version` doesn't work yet.
- The k3s bootstrap provider (cluster-api-k3s v0.4) implements the v1beta1 Cluster API contract, supported by Cluster
  API until v1.16.
- Only one K3k cluster per K3kControlPlane; the control plane is scaled with `servers`, not with Machines.
