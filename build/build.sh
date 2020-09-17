#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

GOOS=${1-linux}
BIN_DIR=${2-$(pwd)/build/_output/bin}
BIN_FILE_NAME="infinispan-operator"
BUILD_PATH="./cmd/manager"
VERSION=${RELEASE_NAME:-$(git describe --tags --always --dirty)}
GO_LDFLAGS="-X github.com/infinispan/infinispan-operator/version.Version=${VERSION}"
echo "building ${BIN_FILE_NAME}... version ${VERSION}"
GOOS=${GOOS} GOARCH=amd64 CGO_ENABLED=0 go build -o "${BIN_DIR}"/"${BIN_FILE_NAME}" -ldflags "${GO_LDFLAGS}" "${BUILD_PATH}"
