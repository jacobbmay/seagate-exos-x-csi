# Live CSI qualification

These tests call a deployed CSI controller and therefore create real,
disposable PowerVault volumes. Run them only against a designated test array.
They do not print credentials or complete CSI volume IDs.

## Capacity matrix and `limit_bytes`

`run-capacity-limit.sh` derives the pool, protocol, volume prefix, and
provisioner Secret from a Kubernetes StorageClass. It builds a temporary client,
copies it into the running controller pod, and passes decoded credentials over
stdin. The client verifies these backend-backed results:

| Required | Limit | Expected result |
| ---: | ---: | --- |
| 1 MiB | none | 4 MiB |
| 4 MiB | none | 4 MiB |
| 10 MiB | none | 12 MiB |
| 12 MiB | none | 12 MiB |
| 1 GiB | none | 1 GiB |
| 10 MiB | 10 MiB | `OutOfRange`, before creation |
| 10 MiB | 12 MiB | 12 MiB |

Every successfully created volume is deleted before the client advances. The
runner also removes its temporary binary from the controller pod.

```bash
GO_BIN=/path/to/go \
NAMESPACE=seagate \
STORAGE_CLASS=powervault-sc-a \
./test/live/run-capacity-limit.sh
```

On an administration host without Go, build the client elsewhere and provide
its path with `CLIENT_BINARY=/path/to/exos-x-csi-capacity-limit`.

The controller Deployment and Secret may use a different namespace. The
StorageClass's provisioner-secret namespace is honored automatically; use
`NAMESPACE` only for the controller pod.

## Stale-capacity evidence assertions

Fault injection remains deliberately manual because it changes array mappings,
connector state, and host multipath state. Once an unused stale map has been
created, run this read-only assertion on the affected node before attempting
candidate attach or helper repair:

```bash
sudo HELPER=/path/to/recover-stale-multipath \
  ./test/live/assert-stale-map.sh \
  3600c0ff000000000000000000000000 \
  1073741824 \
  2147483648
```

Set `HELPER` to the installed recovery helper on the test host. The assertion
requires `GROWTH_NEEDED`, an unused map, eight paths by default, one exact
WWID, and exact cached/direct byte counts. Set `EXPECTED_PATHS` only when the
test topology intentionally has a different path count. The output includes
the helper SHA-256 but never reads connector JSON.

After candidate attach has failed, assert the Kubernetes evidence from an
administration host:

```bash
./test/live/assert-failed-precondition.sh \
  csi-repro negative-attach-cycle2 \
  3600c0ff000000000000000000000000 \
  1073741824 \
  2147483648
```

The script requires a pod event with top-level `FailedPrecondition`, the
expected WWID, and the exact cached/direct mismatch. It remains useful after
repair because Kubernetes retains the failure event for a limited period.

## Direct wrong-LUN assertion

`node-publish-check` is a small direct CSI client for controlled negative node
tests. It accepts an augmented volume ID plus explicit IQN, portals, and LUN,
then requires the configured gRPC status. Its initial qualification use points
a disposable volume ID at a LUN occupied by a different test volume and
requires `FailedPrecondition` before any connector or mount is created.
For an isolated baseline-driver probe expected to attach successfully, pass
`--expected-code OK --unpublish-after-success`; the client then requires a
successful cleanup NodeUnpublish before reporting success.
Use `--timeout` for a baseline expected to enter the upstream per-portal wait;
the default is 90 seconds. That default also leaves enough time for the
candidate missing-connector stable-absence window during `--unpublish-only`.

To exercise detach reconciliation against a preserved, independently verified
unused map without issuing another publish, pass `--unpublish-only` and
`--expected-code OK`. In that mode only `--volume-id` and `--target-path` are
required. The target path should be a nonexistent disposable path unless the
test intentionally owns an existing publication. Audit the exact WWID map and
prove that it is unused before invoking this mode.
