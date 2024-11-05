#!/usr/bin/env bash
set -euo pipefail

K8S_VERSION="${K8S_VERSION:="1.28.x"}"
KUBEBUILDER_ASSETS="/usr/local/kubebuilder/bin"

main() {
    tools
    kubebuilder
}

tools() {
    go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
    go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
    go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
    go install github.com/onsi/ginkgo/v2/ginkgo@latest

    if ! echo "$PATH" | grep -q "${GOPATH:-undefined}/bin\|$HOME/go/bin"; then
        echo "Go workspace's \"bin\" directory is not in PATH. Run 'export PATH=\"\$PATH:\${GOPATH:-\$HOME/go}/bin\"'."
    fi
}

kubebuilder() {
    echo "Setting up Kubebuilder assets..."
    
    sudo mkdir -p ${KUBEBUILDER_ASSETS}
    sudo chown "${USER}" ${KUBEBUILDER_ASSETS}
    arch=$(go env GOARCH)
    ln -sf $(setup-envtest use -p path "${K8S_VERSION}" --arch="${arch}" --bin-dir="${KUBEBUILDER_ASSETS}")/* ${KUBEBUILDER_ASSETS}
    find $KUBEBUILDER_ASSETS
}

main "$@"