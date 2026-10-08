#!/usr/bin/env bash
set -euo pipefail

namespace=${NAMESPACE:-seagate}
storage_class=${STORAGE_CLASS:-powervault-sc-a}
controller_container=${CONTROLLER_CONTAINER:-seagate-exos-x-csi-controller}
controller_selector=${CONTROLLER_SELECTOR:-app=seagate-exos-x-csi-controller-server}
go_bin=${GO_BIN:-go}
client_binary=${CLIENT_BINARY:-}

required_commands=(kubectl jq)
if [[ -z "$client_binary" ]]; then
    required_commands+=("$go_bin")
fi
for command in "${required_commands[@]}"; do
    command -v "$command" >/dev/null 2>&1 || {
        echo "ERROR: required command not found: $command" >&2
        exit 1
    }
done

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
work_dir=$(mktemp -d)
remote_binary=/tmp/exos-x-csi-capacity-limit
controller_pod=

cleanup() {
    if [[ -n "$controller_pod" ]]; then
        kubectl -n "$namespace" exec "$controller_pod" -c "$controller_container" -- \
            rm -f "$remote_binary" >/dev/null 2>&1 || true
    fi
    rm -rf "$work_dir"
}
trap cleanup EXIT

storage_json=$(kubectl get storageclass "$storage_class" -o json)
pool=$(jq -r '.parameters.pool // empty' <<<"$storage_json")
protocol=$(jq -r '.parameters.storageProtocol // empty' <<<"$storage_json")
volume_prefix=$(jq -r '.parameters.volPrefix // "csi"' <<<"$storage_json")
secret_name=$(jq -r '.parameters["csi.storage.k8s.io/provisioner-secret-name"] // empty' <<<"$storage_json")
secret_namespace=$(jq -r '.parameters["csi.storage.k8s.io/provisioner-secret-namespace"] // empty' <<<"$storage_json")

[[ -n "$pool" && -n "$protocol" && -n "$secret_name" && -n "$secret_namespace" ]] || {
    echo "ERROR: storage class is missing pool, protocol, or provisioner-secret parameters" >&2
    exit 1
}

mapfile -t controller_pods < <(
    kubectl -n "$namespace" get pods -l "$controller_selector" \
        --field-selector=status.phase=Running -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'
)
if [[ ${#controller_pods[@]} -ne 1 ]]; then
    echo "ERROR: expected exactly one running CSI controller pod, found ${#controller_pods[@]}" >&2
    exit 1
fi
controller_pod=${controller_pods[0]}

if [[ -z "$client_binary" ]]; then
    client_binary=$work_dir/exos-x-csi-capacity-limit
    CGO_ENABLED=0 "$go_bin" build -trimpath \
        -o "$client_binary" \
        "$repo_root/test/live/capacity-limit"
fi
[[ -x "$client_binary" ]] || {
    echo "ERROR: client binary is not executable: $client_binary" >&2
    exit 1
}
kubectl -n "$namespace" cp "$client_binary" \
    "$controller_pod:$remote_binary" -c "$controller_container"
kubectl -n "$namespace" exec "$controller_pod" -c "$controller_container" -- \
    chmod 0700 "$remote_binary"

kubectl -n "$secret_namespace" get secret "$secret_name" -o json \
    | jq -c '.data | with_entries(.value |= @base64d)' \
    | kubectl -n "$namespace" exec -i "$controller_pod" -c "$controller_container" -- \
        "$remote_binary" \
        --endpoint /csi/csi.sock \
        --pool "$pool" \
        --protocol "$protocol" \
        --volume-prefix "$volume_prefix"
