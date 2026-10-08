#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 5 ]]; then
    echo "usage: $0 NAMESPACE POD WWID CACHED_BYTES DIRECT_BYTES" >&2
    exit 2
fi

namespace=$1
pod=$2
wwid=$3
cached_bytes=$4
direct_bytes=$5

for command in kubectl jq; do
    command -v "$command" >/dev/null 2>&1 || {
        echo "ERROR: required command not found: $command" >&2
        exit 1
    }
done
[[ $wwid =~ ^[[:xdigit:]]+$ ]] || {
    echo "ERROR: WWID must contain only hexadecimal characters" >&2
    exit 2
}
[[ $cached_bytes =~ ^[1-9][0-9]*$ && $direct_bytes =~ ^[1-9][0-9]*$ ]] || {
    echo "ERROR: capacities must be positive byte counts" >&2
    exit 2
}

# Multipath WWIDs commonly include the leading NAA type nibble that the CSI
# volume handle omits. Accept either representation in the kubelet message.
csi_wwn=$wwid
if [[ $wwid == 3* ]]; then
    csi_wwn=${wwid#3}
fi

events=$(kubectl -n "$namespace" get events \
    --field-selector "involvedObject.kind=Pod,involvedObject.name=$pod" -o json)
message=$(jq -r --arg cached "$cached_bytes" --arg direct "$direct_bytes" \
    '[.items[].message
      | select(contains("rpc error: code = FailedPrecondition"))
      | select(contains("cached capacity " + $cached))
      | select(contains("target capacity " + $direct))][-1] // empty' \
    <<<"$events")

[[ -n $message ]] || {
    echo "ERROR: no matching FailedPrecondition event found for pod $namespace/$pod" >&2
    exit 1
}
[[ $message == *"WWN ${csi_wwn}"* || $message == *"WWN ${wwid}"* ]] || {
    echo "ERROR: FailedPrecondition event does not name expected WWID" >&2
    exit 1
}

printf '%s\n' "$message"
printf 'PASS: %s/%s failed closed with cached=%s and direct=%s\n' \
    "$namespace" "$pod" "$cached_bytes" "$direct_bytes"
