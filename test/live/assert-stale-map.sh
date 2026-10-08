#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 ]]; then
    echo "usage: sudo $0 WWID CACHED_BYTES DIRECT_BYTES" >&2
    exit 2
fi

wwid=$1
cached_bytes=$2
direct_bytes=$3
helper=${HELPER:-}
expected_paths=${EXPECTED_PATHS:-8}

[[ $wwid =~ ^[[:xdigit:]]+$ ]] || {
    echo "ERROR: WWID must contain only hexadecimal characters" >&2
    exit 2
}
[[ $cached_bytes =~ ^[1-9][0-9]*$ && $direct_bytes =~ ^[1-9][0-9]*$ ]] || {
    echo "ERROR: capacities must be positive byte counts" >&2
    exit 2
}
((direct_bytes > cached_bytes)) || {
    echo "ERROR: DIRECT_BYTES must be greater than CACHED_BYTES" >&2
    exit 2
}
[[ -n $helper && -x $helper ]] || {
    echo "ERROR: set HELPER to an executable stale-multipath recovery helper" >&2
    exit 1
}

helper_hash=$(sha256sum "$helper" | awk '{print $1}')
if ! output=$("$helper" --wwid "$wwid" 2>&1); then
    printf '%s\n' "$output"
    echo "ERROR: helper audit failed" >&2
    exit 1
fi
printf 'helper-sha256: %s\n%s\n' "$helper_hash" "$output"

grep -Fq "cached map capacity:" <<<"$output"
grep -Fq "(${cached_bytes} bytes)" <<<"$(grep -F 'cached map capacity:' <<<"$output")" || {
    echo "ERROR: map does not report expected cached capacity $cached_bytes" >&2
    exit 1
}
grep -Fq "  use state: unused" <<<"$output" || {
    echo "ERROR: selected map is not unused" >&2
    exit 1
}
grep -Fq "  result: GROWTH_NEEDED" <<<"$output" || {
    echo "ERROR: helper did not classify the map as GROWTH_NEEDED" >&2
    exit 1
}

mapfile -t path_lines < <(grep '^  path /dev/' <<<"$output")
if [[ ${#path_lines[@]} -ne $expected_paths ]]; then
    echo "ERROR: found ${#path_lines[@]} paths, expected $expected_paths" >&2
    exit 1
fi
for line in "${path_lines[@]}"; do
    [[ $line == *"cached="*"(${cached_bytes} bytes)"* ]] || {
        echo "ERROR: path cached capacity mismatch: $line" >&2
        exit 1
    }
    [[ $line == *"target="*"(${direct_bytes} bytes)"* ]] || {
        echo "ERROR: path direct capacity mismatch: $line" >&2
        exit 1
    }
    [[ $line == *"WWID=${wwid}" ]] || {
        echo "ERROR: path identity mismatch: $line" >&2
        exit 1
    }
done

printf 'PASS: %d paths report WWID %s, cached=%s, direct=%s, map unused\n' \
    "$expected_paths" "$wwid" "$cached_bytes" "$direct_bytes"
