# Infinispan Operator

This is a Kubernetes operator to manage Infinispan clusters.

## System Requirements

* [go 1.17](https://github.com/golang/go) or higher.
* Docker | Podman
* [Operator SDK 1.3.2](https://github.com/operator-framework/operator-sdk/releases/download/v1.3.2/operator-sdk_linux_amd64)
* A running Kubernetes cluster

# Usage

For details on how to use the operator, please read the [official operator documentation](https://infinispan.org/docs/infinispan-operator/main/operator.html).

# Getting Started
You’ll need a Kubernetes cluster to run against. You can use [KIND](https://sigs.k8s.io/kind) to get a local cluster for testing, or run against a remote cluster.
**Note:** Your controller will automatically use the current context in your kubeconfig file (i.e. whatever cluster `kubectl cluster-info` shows).

We utilise [Skaffold](https://skaffold.dev/) to drive CI/CD, so you will need to download the latest binary in order to
follow the steps below:

## Kind Cluster

Create a local kind cluster backed by a local docker repository, with [OLM](https://olm.operatorframework.io/) and
[cert-manager](https://cert-manager.io) installed:

```sh
./hack/kind.sh`
```

## Development

Build the Operator image and deploy to a cluster:

```sh
skaffold dev
```

Changes to the local `**/*.go` files will result in the image being rebuilt and the Operator deployment updated.

## Debugging
Build the Operator image with [dlv](https://github.com/go-delve/delve) so that a remote debugger can be attached
to the Operator deployment from your IDE.

```sh
skaffold debug
```

## Deploying
Build the Operator image and deploy to a cluster:

```sh
skaffold run
```

## Remote Repositories
The `skaffold dev|debug|run` commands can all be used on a remote k8s instance, as long as the built images are accessible
on the cluster. To build and push the operator images to a remote repository, add the `--default-repo` option, for example:

```sh
skaffold run --default-repo <remote_repo>
```

# OLM Bundle
The OLM bundle manifests are created by executing `make bundle VERSION=<latest-version>`.

This will create a `bundle/` dir in your local repository containing the bundle metadata and manifests, as well as a
`bundle.Dockerfile` for generating the image.

The bundle image can be created and pushed to a repository with:

```
make bundle-build bundle-push VERSION=<latest-version> IMG=<operator-image> BUNDLE_IMG=<bundle-image>
```

# Release
To create an Operator release perform the following:

1. Update the `RELATED_IMAGE_OPENJDK` field in `config/manager/manager.yaml` to point to the latest .Final tag of Infinispan Server image. Do not use the floating tags for a stream, e.g. `13.0`.
2. Commit changes with appropriate commit message, e.g "Releasing Operator <x.y.z>.Final"
3. Tag the release `git tag <x.y.z>`
4. Create and push the image `make operator-build operator-push VERSION=<x.y.z>.Final IMG=quay.io/infinispan/operator:<x.y.z>.Final`
5. Remove the old bundle from local `rm -rf bundle`
6. Create OLM bundle `make bundle VERSION=<x.y.z> CHANNELS=2.2.x DEFAULT_CHANNEL=2.2.x IMG=quay.io/remerson/operator:<x.y.z>.Final`
7. Copy contents of `bundle/` and issue PRs to:
    - https://github.com/k8s-operatorhub/community-operators
    - https://github.com/redhat-openshift-ecosystem/community-operators-prod
8. Once PR in 5 has been merged and Operator has been released to OperatorHub, update the "replaces" field in `config/manifests/bases/infinispan-operator.clusterserviceversion.yaml`
to `replaces: infinispan-operator.v<x.y.z>`
9. Update the `RELATED_IMAGE_OPENJDK` field in `config/manager/manager.yaml` to use the required floating tag, e.g. `13.0`
10. Update `scripts/ci/install-catalog-source.sh` `VERSION` field to the next release version
11. Update `scripts/create-olm-catalog.sh` to include the just released version in `BUNDLE_IMGS` and the next release version in the update graph 
12. Commit changes with appropriate commit message, e.g "Next Version <x.y.z>"

# Testing

## Unit Tests

`make test`

## Go Integration Tests

The different categories of integration tests can be executed with the following commands:

- `make infinispan-test`
- `make cache-test`
- `make batch-test`
- `make multinamespace-test`
- `make backuprestore-test`

The target cluster should be specified by exporting or explicitly providing `KUBECONFIG`, e.g. `make infinispan-test KUBECONFIG=/path/to/admin.kubeconfig`.

### Env Variables
The following variables can be exported or provided as part of the `make *test` call.

| Variable              | Purpose                                                                              |
|-----------------------|--------------------------------------------------------------------------------------|
| `TEST_NAME`           | Specify a single test to run                                                         |
| `TESTING_NAMESPACE`   | Specify the namespace/project for running test                                       |
| `RUN_LOCAL_OPERATOR`  | Specify whether run operator locally or use the predefined installation              |
| `EXPOSE_SERVICE_TYPE` | Specify expose service type. `NodePort \| LoadBalancer \| Route`.                    |
| `PARALLEL_COUNT`      | Specify parallel test running count. Default is one, i.e. no parallel tests enabled. |

### Xsite
Cross-Site tests require you to create two k8s Kind clusters or utilize already prepared OKD clusters:
```
$ source scripts/ci/configure-xsite.sh $KIND_VERSION $METALLB_VERSION
```

Actual `$KIND_VERSION` and `$METALLB_VERSION` values can be explored inside the `Jenkinsfile` file 

To test locally in running Kind clusters, run:
```
$ go test -v ./test/e2e/xsite/ -timeout 30m
```

## Java Integration Tests
TODO
