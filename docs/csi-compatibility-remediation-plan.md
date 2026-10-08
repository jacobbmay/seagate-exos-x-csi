# CSI compatibility and recovery remediation plan

## Disposition against the tested iSCSI release scope (2026-10-07)

This is a status review of the **numbered plan below**, not a claim that every
proposed remediation was implemented. The intended first release is a fresh
installation using external PowerVault/Exos **iSCSI** through Seagate CSI;
old-to-new rolling driver upgrades, FC/SAS hardware, new CRDs, and host-token
architecture are outside that release scope. `Deferred` means the problem or
acceptance gate remains open, even when a narrower mitigation passed in the
lab. `Disregarded` means not applicable to this scoped deployment, **not**
fixed in the general driver. The plan's original proposed order and blanket
release gate below are retained as review history, not as a record of work
completed or a substitute for explicit risk acceptance.

The valid iSCSI WWID/capacity, LUN-reuse, reboot, initiator-registration, and
cross-array serialization fixes have substantial unit and live evidence from
the three-node PowerVault qualification lab.
Five forced single-node reboot cycles, fresh-cluster rolling and simultaneous
reboots, disposable attach/detach, a supervised lost-node/OSD replacement,
and a three-node OS upgrade with unchanged PostgreSQL data all passed within
their documented boundaries. That evidence does **not** cover the acceptance
gates marked deferred below. In particular, the live controller used a
one-off image from `964662e` while the ISO-pinned node image was `49b57cf`;
the subsequently built unified prerelease image/ISO has not been live-qualified.

| Plan item | Disposition for this release |
| --- | --- |
| 1. Dead-node controller detach | Deferred; supervised recovery done |
| 2. Destructive node RPC | Deferred |
| 3. Clone/restore | Deferred |
| 4. Fallback SCSI scan | Deferred |
| 5. Host utility prerequisites | Deferred; current host image tested |
| 6. Mixed-version driver rollout | Disregarded; fresh install only |
| 7. Array TLS | Deferred as a whole; current insecure mode tested |
| 8. Controller queueing | Deferred as a whole; shared-client serialization done |
| 9. Same-node multi-pod provenance | Deferred |
| 10. Partial iSCSI attach failure | Deferred |
| Separate FC/SAS race | Disregarded for iSCSI-only deployment |
| Full release gate below | Deferred; unified image/ISO live qualification pending |

This plan covers the findings from the review of the current fix branch against
`v1.10.0`, including commit `964662e` (controller serialization). The chart's
temporary image repository and tag are intentionally outside this plan because
the consuming project supplies its own image values.

The goal is to retain the valid safety fixes: exact WWID and capacity checks,
protection for mounted or open multipath maps, safe same-node multi-pod
publishing, initiator registration, capacity-range validation, configurable TLS
verification, and serialization of the shared array client. The work below
changes how the driver recovers, authenticates, and rolls those fixes out.

## Order of work

1. Design durable detach identity and node cleanup intent, then fix controller
   detach semantics and remove the unsafe node acknowledgment barrier (items
   1 and 2). These are release blockers; do not enable automatic orphan cleanup
   based only on an absent mount or connector file.
2. Stage the compatibility and authentication transitions separately (item 6).
   Test every version pair before changing the controller's detach behavior.
3. Fix clone/restore recovery, constrain the fallback SCSI scan, and close
   partial iSCSI attach failures (items 3, 4, and 10).
4. Verify host prerequisites and preserve the current deployment's explicit
   TLS compatibility setting (items 5 and 7). CA migration is optional.
5. Validate same-node multi-pod mount and detach behavior (item 9), improve
   controller queueing (item 8), and synchronize the inherited FC/SAS map.

Each numbered item below has a change and an acceptance gate. The live gates
use disposable volumes on a designated test array; never use a production
volume to simulate a failed node or stale LUN.

## 1. Controller unpublish and dead-node recovery

**Disposition: DEFERRED.** The named-node dead-node path still contacts the
lost node for initiators and requires its cleanup callback; empty `node_id`,
array-authoritative automatic detach, reusable-IP binding, and the proposed
durable records were not implemented or tested. The CRD/host-token design
below is **disregarded for this scoped release**, by project decision, not
quietly treated as complete. A fenced, supervised array-unmap and exact
VolumeAttachment recovery was tested first on a disposable PVC and then on
retained Rook OSDs on a newly imaged node; the runbook now supports
array-first IQN identification and an all-array retired-IQN closure audit.
That is the qualified fallback, not automatic dead-node recovery. See the
supervised permanent-node-loss procedure maintained by the deployment team.

**Problem.** `pkg/controller/publisher.go` always queries `node_id` as a live
node address. CSI requires an empty `node_id` to unpublish the volume from all
nodes. A named node that is unavailable can leave a VolumeAttachment stuck;
the new mandatory `NotifyUnmap` acknowledgment can also block success after
the array has already unmapped the volume, including for FC and SAS.

**Implementation.**

- Parse and validate the augmented volume ID, including protocol and WWID;
  match its WWID to the array volume before changing mappings. A reused volume
  name must not authorize unmapping a different volume.
  For an empty `node_id`, use the array's volume-wide unmap operation
  (`UnmapVolume(volumeName, "")`) and verify from array state that no mappings
  remain. Treat an already-unmapped volume as success only after confirming
  that state; do not equate every generic array unmap error with absence.
- For a named node, use live initiators only after matching the responding
  node's stable host token and Kubernetes Node UID to the original binding;
  `node_id` is currently a reusable IP address. First determine whether the
  array can authoritatively recover the initiators and mapping for that exact
  array/WWID/node. If not, add a durable publish journal keyed by
  array identity, WWID, stable node identity, and initiators. Write the binding
  *before* the array mapping; after a crash, reconcile the journal against
  actual array state before retrying or clearing it. Do not rely on a record
  written only after `PublishVolume`, and migrate or reconstruct mappings made
  by older driver versions. Node IDs are currently IP addresses and may be
  reused, so verify a saved binding against current array host/map data before
  using it. Scope the unmap to the identified node. If ownership remains
  uncertain, return a retryable error rather than unmapping another node.
- Confirm the volume is no longer mapped to the requested node before
  returning success. Controller success must not wait for a live node RPC, but
  the local cleanup in item 2 still needs positive detach intent; do not infer
  that intent merely from a missing target. Do not require a callback for FC/SAS
  controller unpublish; preserve their cleanup by the mechanism in item 2.
- For a node that may be offline, persist a trusted cleanup record through the
  Kubernetes API after confirmed array unmap and before returning success.
  Use a namespaced, versioned `CSIExosVolumeNodeState` resource in the driver's
  namespace, one object per array UUID/WWID/host identity. Store the exact
  array UUID, WWID, node UID, reboot-stable host token, initiators, monotonically
  increasing publish generation, and phase (`Publishing`, `Published`,
  `Detaching`, `Detached`, `Cleaning`, `Cleaned`, `Uncertain`). Install its CRD
  and controller-write/node-read-and-claim RBAC before any code consumes the
  records. Give each node a random host token persisted in the host-mounted
  plugin directory; return it through a versioned node initiator response.
  An IP address alone is not a valid record key. A re-created Node object
  with a new UID must fail closed
  unless ownership is independently re-established.
- Use Kubernetes `resourceVersion` compare-and-swap for record transitions,
  but do not treat a renewable Lease as fencing for an already-issued array
  RPC. For the first release, run exactly one active controller (`replicas: 1`,
  `Recreate`) and prevent overlapping old/new controller processes during
  rollout. Drain active requests on shutdown and set a termination grace
  period longer than the bounded API call deadline; a lost/uncertain call
  remains frozen until backend completion is proven. Before each array
  mutation, persist an `InFlight` operation with
  request identity and desired result. A successor must not start a conflicting
  publish/unpublish while an old call may still complete. It may resume only
  after authoritative backend operation-completion evidence and array-state
  reconciliation; if the API cannot prove completion, mark the record
  `Uncertain`, freeze that array/WWID, alert, and require operator recovery.
  Support multiple active controller replicas only after the array API offers
  fencing/conditional operations or an equivalent provable no-overlap design;
  Lease expiry by itself is not sufficient.
- Allocate a generation only for a genuinely new publication epoch. Persist
  `Publishing` before mapping, then verify and commit `Published`. On a retry
  of an already mapped compatible publication, reuse its generation and
  *identical* CSI `PublishContext`; do not advance it on each RPC. Put only
  immutable array identity and existing publish properties in
  `PublishContext`, not the mutable generation. The node obtains the current
  generation from the trusted record via item 2's node-side bridge at
  `NodePublishVolume`, verifies array UUID/WWID and phase, and durably saves
  the binding with its connector. A legacy volume ID has only
  name/protocol/WWID; if the connector/binding is missing, do not guess an
  array UUID or generation from WWID alone. Persist `Detaching` before
  unmapping and `Detached` only after verifying array state. A node ignores a
  detach record for an older generation. A successful array unmap followed by
  failed `Detached` persistence is a retryable partial result: the next call
  confirms completion and the already-unmapped state before completing the
  record. A Kubernetes API outage must not turn an uncertain record into
  cleanup permission or a false success.
- A `Detached` record must survive VolumeAttachment deletion and node reboot.
  It is superseded only by a higher-generation verified publish; do not expire
  it on a timer while stale sessions can rediscover a map. Define an explicit
  operator-assisted cleanup after verified volume deletion. When `node_id` is
  empty, create records for each identifiable former host. Never target a
  different host because an old IP was reused; unidentifiable legacy hosts
  require operator recovery rather than guessed cleanup.
- Preserve idempotency across retries after partial or completed array unmap.
  Keep the existing valid failure behavior when the array cannot confirm a
  safe result.

**Acceptance.** Unit tests cover empty `node_id`, named/live node, named/dead
node with recoverable mapping identity, absent mapping, partial multi-initiator
unmap, uncertain array state, mismatched WWID, reused name/IP, a crash between
mapping and recording success, and pre-upgrade mappings without a journal.
Exercise a delayed array unmap completing after controller takeover, an
`Uncertain` operation that freezes conflicting work, idempotent duplicate
publish with identical `PublishContext`, and republish with a new generation.
Also test Kubernetes API failure between array unmap and `Detached`
persistence, VolumeAttachment deletion, Node UID replacement, reusable IP,
and record RBAC denial. A
live CSI test verifies that an empty node ID clears all mappings, and a
simulated deleted node does not leave a VolumeAttachment after its confirmed
array unmap. Repeat for iSCSI, FC, and SAS where test hardware is available.
Do not claim FC/SAS qualification based on iSCSI tests alone.

## 2. Destructive node RPC and local cleanup

**Disposition: DEFERRED.** No node-side tombstone/reconciler, authenticated
bridge, or replacement of the unauthenticated `NotifyUnmap` listener was
implemented. Its exact-WWID and mounted/open guards and the reboot cleanup
path were exercised on disposable iSCSI volumes, including late discovery,
but those tests do not authenticate the caller or prove safe cleanup for all
future rediscovery. The current callback risk remains an explicit release
decision; FC/SAS-specific work in this item is disregarded for the tested
iSCSI-only deployment, not fixed upstream. The guarded host recovery helper
remains the supervised fallback for a proven unused stale map.

**Problem.** `pkg/node_service/node_service_server.go` accepts an unauthenticated
`NotifyUnmap` request on a host-network listener. It can now disconnect an
unused exact-WWID iSCSI map. Mounted and open-count checks protect active
maps, but they do not prove that the caller is the controller. The supplied
volume name is also used as the gatekeeper key and can differ from the WWN.

**Implementation.**

- Preferred design: move iSCSI orphan cleanup to the node plugin. Persist
  connector identity *and a separate cleanup-intent/tombstone* in a
  reboot-stable host-mounted kubelet plugin directory, for example
  `<kubeletPath>/plugins/csi-exos-x.seagate.com/state` (not the current
  `/var/run/csi-exos-x.seagate.com`). Add/verify the Helm hostPath/mount, use
  restrictive file permissions and atomic write-plus-file/directory sync,
  and migrate active connector bindings after validating their live WWID;
  missing legacy state fails closed. Key both by array identity, canonical
  WWID, and a publish generation, not just volume name.
  After the last target is safely unmounted, atomically write and sync the
  intent before final disconnect and before returning `NodeUnpublishVolume`
  success; preserve it when the current connector file is deleted. Also read
  the trusted controller-detach records from item 1, accepting one only when
  its host identity matches this node and no newer publish supersedes it. A
  retained local tombstone triggers periodic post-unmap cleanup only after
  the matching record reaches `Detached`; if no trustworthy record exists,
  report the legacy state for safe retry/operator recovery. A valid,
  still-published volume must never acquire an intent merely because
  its target is temporarily absent at startup. A new publish cancels or
  supersedes an older intent under the same per-WWID lock; define retention
  and garbage collection so delayed rediscovery cannot outlive the tombstone.
  If an old installation lacks trustworthy host identity or cleanup intent,
  leave the map in place and report a manual recovery action; never guess that
  it is an orphan.
- Reconcile on startup, after last `NodeUnpublishVolume`, and periodically for
  outstanding intents. Before every disconnect, recheck the intent/generation,
  exact live WWID, remaining kubelet targets, mounts, and open state under the
  publish/cleanup lock. Before any disconnect, atomically claim the current
  generation in a separate cleanup-claim field through the Kubernetes record
  bridge; the claim can coexist with `Published` during CSI's synchronous
  `NodeUnpublishVolume` (which precedes controller unpublish) or with
  `Detached` during later reconciliation. Controller publish must wait while a
  cleanup claim is active. After disconnect, verify exact-map/path absence,
  clear the claim, and retain the tombstone; only post-controller-unmap cleanup
  moves `Detached` to `Cleaned`. A claim is not time-expired into a new publish
  while its cleanup may still be running: a crash requires proof the old node
  operation stopped, then reconciliation or manual recovery. Recheck the
  generation after cleanup. A stale cache or
  unreachable Kubernetes API is not proof of current detach intent. Do not
  disconnect on missing target or connector state alone. Keep unknown or
  conflicting state attached and report it for retry or manual inspection. A
  returning node must clean a late rediscovery without a controller-to-node
  RPC when positive intent exists.
- The node process chroots to `/host`, so it cannot assume its pod-mounted
  Kubernetes service-account token is visible. Add a small same-pod record
  bridge outside the chroot, with an authenticated, permission-restricted Unix
  socket in the stable plugin mount; check Unix peer credentials and allow
  only the record operations the storage process needs. The bridge uses a
  dedicated, always-set
  node ServiceAccount to get/list/watch records and perform only the
  compare-and-swap cleanup-claim/status updates it needs in the driver
  namespace. Grant read access to this node's Kubernetes Node object so the
  bridge can obtain its UID; filter records by that UID and stable host token.
  Do not grant every node unrestricted patch of every volume record: enforce
  node-UID/host-token ownership on claim updates with a validating admission
  policy or equivalent per-node authorization. If this cannot be deployed,
  disable automatic cleanup rather than trust the bridge's client-side filter.
  Records contain no credentials. Document the bridge's token/certificate
  rotation and failure behavior. The chrooted storage process never receives
  the Kubernetes token. Verify the rendered DaemonSet, CRD installation,
  ServiceAccount, RBAC, socket path, and host mount before enabling cleanup.
- Remove remote iSCSI disconnect from `NotifyUnmap` once local reconciliation
  is proven. For FC/SAS, add reboot-stable removed-device intents keyed by
  array UUID/WWID/host token/generation and a periodic exact-identity
  reconciler before retiring their callback. Their current
  `SASandFCRemovedDevicesMap` is in-memory and is not reboot recovery. During
  rediscovery, enumerate sysfs/SCSI transport paths and validate exact VPD or
  multipath identity rather than relying only on `/dev/disk/by-id/wwn-*`
  symlinks. Patch or wrap the FC/SAS detach libraries so sysfs delete-write
  errors are returned; verify each expected path actually disappears and
  retry/report failures without broad deletion. During transition keep the
  FC/SAS callback secured and best-effort, with no controller success
  dependency; remove it only after protocol-specific live
  reboot and rediscovery tests pass. If any node RPC remains, authenticate
  and authorize both `GetInitiators` and cleanup calls
  *after* item 6's compatible credential rollout, restrict the listener to the
  required network, and use canonical WWID rather than a caller-provided name
  for locking. A NetworkPolicy alone is insufficient protection for the
  current `hostNetwork` listener.
- If the callback must remain during transition, it must not be required for
  controller detach success. Do not allow old or unauthenticated peers to
  invoke the new disconnect behavior.

**Acceptance.** A reboot with a still-assigned pod does not disconnect its
volume while kubelet rebuilds targets. A separate reboot test recreates an
unused map after successful last-target unpublish; the persisted intent causes
eventual exact-map removal while preserving another WWID and mounted/open maps.
Test a crash before and after the intent is written, delayed rediscovery,
concurrent republish between record read and disconnect, a crashed `Cleaning`
claim, old intent versus new generation, Kubernetes API unavailability,
chrooted bridge/socket failure, and record garbage collection. A stale record,
including one left by rollback, cannot disconnect a currently published
generation.
An unauthorized network request cannot cause a disconnect. A forged name
paired with a real WWN cannot bypass the publish/cleanup lock. A dead-node
controller detach succeeds after confirmed array unmap and durable cleanup
recording without node contact; on node return, cleanup occurs only with
trusted intent or is explicitly reported for safe manual recovery. FC/SAS
rediscovery still gets cleaned after a node reboot, including when the udev
symlink is missing; a failed sysfs delete is observable and retried.

## 3. Clone and snapshot-restore capacity recovery

**Disposition: DEFERRED.** The copy-before-capacity-check and same-name retry
paths remain in `pkg/controller/provisioner.go`. No clone/restore remediation,
operation journal, rollback proof, or live clone/restore matrix from this item
was completed. Ordinary create/attach/delete and capacity tests do not
qualify clone or snapshot restore.

**Problem.** `pkg/controller/provisioner.go` calls `CopyVolume` before checking
the resulting capacity. A source outside the requested range can leave a
wrong-sized destination after `OutOfRange`; the next CreateVolume call finds
that destination and returns `AlreadyExists`.

**Implementation.**

- Query the source volume or snapshot capacity before copying. Use volume
  blocks times block size for a volume source; verify the snapshot capacity
  field's units and source-ID format against the API before using them as
  bytes. Reject a source above `limit_bytes` before creating a destination. If
  the source is below `required_bytes`, either establish that the array can
  expand the copy to the normalized required size or reject before copying.
- When expansion is supported, copy, expand, and read back the final size
  before returning a response. Handle array allocation granularity and report
  the actual size. Keep normal empty-volume capacity validation.
- Replace the current name/exact-size `CheckVolumeExists` shortcut for clone
  requests with a range-aware lookup that considers source and parameters.
  Since the array's volume response does not expose clone provenance, define a
  durable per-destination operation record (array identity, request/source
  fingerprint, destination WWID when known, creation phase) and coordinate
  same-name *ordinary create, clone/restore, and delete* requests together.
  A clone-only lock cannot prevent another operation from creating or deleting
  the same destination during retry. Do not treat a name match or
  the current request's `ContentSource` as evidence of actual provenance.
  If array metadata or the operation record cannot prove an existing volume's
  origin, do not adopt or delete it automatically; report the conflict and
  provide a documented recovery procedure.
- On a later failure, delete only a destination whose identity is proven new
  and still owned by this operation; verify the deletion. If creation outcome
  or ownership is uncertain, inspect array state and return a retryable or
  explicit manual-recovery error without destructive cleanup. Define how
  failed rollback is surfaced and tracked; retries must not silently return
  an incompatible volume. Validate actual capacity against the requested
  range, not merely against the normalized target size.
- Guard both response-status pointers before dereferencing them: the existing
  `CopyVolume` error log can panic on a nil status, and the new ordinary
  `CreateVolume` status check can do the same even when its Go error is nil.
  Return a retryable/unknown array-result error rather than crashing the
  controller; do not claim creation succeeded until `ShowVolumes` confirms it.

**Acceptance.** Tests cover source smaller than required, larger than limit,
exact fit, snapshot size-unit conversion, expansion failure, nil status with
and without a Go error from both array calls, uncertain copy outcome,
controller crash, failed rollback, and same-name/concurrent retries
with compatible and incompatible sources, plus ordinary create/delete racing
a clone retry. A known-new destination is removed
and its removal verified after a failed clone when safe; uncertain ownership
or failed rollback is reported and recoverable without deleting pre-existing
data. Successful responses report the array's actual capacity and true source.

## 4. Scope the fallback SCSI path scan

**Disposition: DEFERRED.** `findSCSIPathsByWWID` still scans all `sd*` block
devices and treats unreadable identities as an error. Exact-WWID missing-state
recovery and reboot tests passed on the tested hosts, but a mixed host with
an unrelated unreadable non-iSCSI disk was not tested. The current scan can
therefore still block recovery on such a host; it does not justify unsafe
cleanup or a success response.

**Problem.** `findSCSIPathsByWWID` in `pkg/storage/iscsiValidation.go` scans all
`/sys/class/block/sd*` devices and fails if any device's VPD identity is
unreadable. An unrelated local, SAS, or virtual disk can prevent iSCSI
unpublish recovery after connector state is lost.

**Implementation.**

- Enumerate candidate disks from the SCSI device's iSCSI transport ancestry
  in sysfs, including stale block paths whose session or udev by-path link has
  disappeared. `/dev/disk/by-path` may help resolve candidates but must not be
  the sole source unless tests prove equivalent degraded-session coverage.
  Resolve and deduplicate block-device paths, then read VPD identity only for
  those candidates. If transport ancestry is ambiguous, fail closed rather
  than declare the requested WWID absent.
- Keep the exact-WWID match requirement. An unreadable candidate that could
  be this iSCSI volume remains an error; an unrelated non-iSCSI disk does not.
  Handle disappearing symlinks and device names as retryable discovery races.
- Make the enumeration and identity reader injectable so tests can model a
  mixed host without depending on actual `/sys` contents.

**Acceptance.** Tests include an unrelated unreadable disk, a matching path,
an unreadable relevant iSCSI path, a stale iSCSI block path with no by-path
link, ambiguous transport ancestry, and a path disappearing during the scan.
The real-node test uses a host with non-iSCSI SCSI disks and verifies that
missing-connector `NodeUnpublishVolume` completes when the target volume is
truly absent.

## 5. Host utility prerequisites

**Disposition: DEFERRED.** The tested host image had the utilities needed
for its iSCSI attach/expand paths, as shown by live provisioning and growth
tests. The driver's `requiredBinaries` list still comments out `blockdev` and
`scsi_id`, omits `sg_readcap`/`udevadm` from an enforced readiness contract,
and logs missing binaries while `Probe` can remain ready. Other host images
and the missing-tool acceptance matrix have not been qualified.

**Problem.** New iSCSI publish checks invoke `blockdev`, `scsi_id`, and
`sg_readcap`; expansion also invokes `udevadm`. The README and the
`requiredBinaries` list in `pkg/node/node.go` do not cover all of them.

**Implementation.**

- Document which commands are required for attach versus expansion, plus the
  owning packages on each supported node distribution. Remember that the node
  process chroots to the host; utilities present only in the CSI image do not
  satisfy this requirement.
- Make the policy explicit: validate common tools at node startup and report
  a failed `Probe`/readiness state if they are absent; validate iSCSI-only
  tools at the relevant iSCSI publish/expand operation and return a specific
  `FailedPrecondition` naming the missing tool and expected host path. If a
  deployment declares iSCSI-only support, its readiness check may include
  those tools. Do not make FC/SAS-only nodes fail startup for missing iSCSI
  utilities. Wire the chosen readiness result to an actual deployment probe;
  logging a warning while `Probe` always returns ready is insufficient.
- Retain multipath as a documented requirement. Check binary versions/output
  on supported distros, especially `sg_readcap --long --brief`.

**Acceptance.** A clean host install following the docs can publish and expand
an iSCSI test volume. A host missing each required tool produces an immediate,
specific diagnostic rather than a late generic attach failure; tests verify
the actual `Probe`/deployment readiness behavior chosen above. FC/SAS-only
installations are not made to depend on iSCSI-only tools.

## 6. Mixed-version controller/node rollout

**Disposition: DISREGARDED for this fresh-install release.** The user has
ruled out upgrading an installed old CSI driver to this version. We did not
implement versioned authentication, a compatibility endpoint, or the old/new
matrix below, and the successful Kairos OS upgrade did not change CSI images.
The lab's one-off `964662e` controller and `49b57cf` nodes differ only by the
later controller serialization commit; that is **not** a mixed-version
upgrade qualification. Reopen this item before any future in-place CSI
upgrade or rollback. A final release should still pin one unified image for
both controller and nodes.

**Problem.** The existing `UnmappedVolume.VolumeName` field now carries an
augmented ID. Old nodes acknowledge it without performing the new iSCSI
cleanup; new nodes acknowledge an old controller's raw WWN via the legacy
path. The current callback therefore cannot prove reconciliation during a
rolling upgrade. Also, immediately requiring authentication on new nodes
would lock out an old controller that still needs `GetInitiators` for publish
and unpublish.

**Implementation.**

- Separate cleanup deployment from RPC authentication enforcement. First
  deploy durable local reconciliation and disable destructive behavior for
  legacy/unauthenticated `NotifyUnmap` callers. Keep `GetInitiators` compatible
  during this phase, restrict network access as far as host networking allows,
  and explicitly document the residual disclosure risk. A legacy callback
  acknowledgment must never be treated as proof that cleanup ran.
- Provision controller/node credentials and trusted identities before
  enforcement. Use mutually authenticated TLS for controller-to-node traffic:
  the controller verifies a per-node identity bound to the expected Node
  UID/host token, and the node verifies a controller identity authorized for
  the driver. Terminate TLS in the non-chrooted node bridge from item 2 and
  forward only allowed methods over its restricted Unix socket. Specify CA,
  certificate issuance/rotation through a cluster-approved CSR signer or
  equivalent issuer, expiry alerts, and credential mounts for the chart; the
  host token is an identifier, not a secret or substitute for TLS. The rollout
  preflight must verify that an issuer exists and the node and controller
  certificates have the expected identities.
  Deploy a controller able to authenticate to new nodes while still talking
  to old nodes, then upgrade nodes. During transition expose legacy
  `GetInitiators` only on a separately configured compatibility endpoint;
  never permit destructive cleanup there. Choose protocol by an explicit
  per-node capability/rollout gate, not by retrying failed TLS as plaintext.
  Once a node is marked auth-capable, no automatic downgrade is allowed.
  Enforce authentication for `GetInitiators` only after every controller
  replica and eligible node is verified capable. Use versioned responses for
  any retained cleanup RPC; old peers cannot report the new guarantee.
- Document the gates for each phase rather than a single unconditional
  nodes-first rule. A nodes-first deployment can be used for independently
  safe local cleanup; authentication enforcement requires the controller-first
  capability phase above. Treat old/new pairs as temporary compatibility
  states with known limitations, not as proof of reconciliation. Prevent a
  chart rollout from advancing both components past a gate at once.
- Do not roll a controller back to a version that cannot advance/read publish
  generations while generation-based cleanup is enabled. First quiesce new
  publish/unpublish calls, disable automatic cleanup, reconcile or archive
  outstanding records, and only then downgrade. An old controller republish
  must not leave a node acting on a prior `Detached` record. Include CRD/RBAC
  installation and node identity migration in the documented rollout phases.
  Install/upgrade the versioned CRD as an explicit pre-upgrade manifest step
  (Helm's `crds/` install path alone does not upgrade existing CRDs), wait for
  `Established`, then apply controller and dedicated node-bridge ServiceAccounts
  and RBAC plus claim-ownership admission enforcement, and verify access before
  enabling record-dependent detach. Keep
  the CRD and records through rollback; do not delete them with a chart
  uninstall until an operator has reconciled outstanding state. Reverse the
  authentication gate before downgrading to a plaintext-only controller.

**Acceptance.** Test old controller/new node, new controller/old node, and
new/new pairs, including `GetInitiators`, attach, detach, and callback behavior.
Old peers are allowed to lack the new cleanup signal only during documented
transient phases; the deployment gate, not the old wire protocol, must detect
and prevent unsafe advancement. An old controller still attaches/detaches
during the node cleanup phase. Unauthorized destructive calls fail throughout;
after auth enforcement, unauthenticated initiator queries fail. A staged
cluster upgrade and rollback keep existing mounted volumes usable and do not
falsely clear attachments; rollback with outstanding detach records is tested.
Test TLS downgrade attempts, wrong-node certificates, certificate rotation and
expiry, CRD-not-established, RBAC/admission denial, chroot bridge outage, and rendered
Helm resource ordering.

## 7. TLS compatibility and optional CA migration

**Disposition: DEFERRED as a full plan item; current deployment mode DONE.**
The tested deployment explicitly renders
`controller.tls.insecureSkipVerify: true`, and the fresh installation,
primary/backup array provisioning, detach, reboot, and OS
upgrade tests succeeded with that setting. Verification-on remains the chart
default. No CA migration was requested for this release, so that optional
part is disregarded for the current deployment. Malformed-CA precedence,
certificate/SAN tests, both-address preflight tooling, README wording, and
the API-client dependency change below were not completed; do not call the
full item done. The current setting does not verify array server identity.

**Problem.** The new secure default verifies the array's HTTPS certificate.
Existing installations that relied on the API library accepting self-signed
certificates may lose all authenticated controller operations on upgrade unless
their consuming chart explicitly retains the previous behavior.

**Implementation.**

- Keep verification enabled by default for new installations, but preserve
  `controller.tls.insecureSkipVerify: true` as a supported, explicit setting
  for the current consuming deployment. It must continue to work without a CA
  bundle and must not be removed or silently overridden by remediation work.
  Document that it disables storage-array server certificate verification and
  carries a man-in-the-middle risk; that risk is an informed deployment choice,
  not a release blocker for these CSI fixes. Include the exact Helm override
  in the upgrade instructions so the changed default is not accidentally used.
  Update the chart value comment and README so they describe this as an
  explicit supported compatibility choice, while still warning about the
  security risk, rather than implying it works only in tests.
- Define precedence when `insecureSkipVerify: true` is set alongside a CA
  input: ignore the CA input with a clear warning and do not mount or parse
  it in that mode. The current client parses a configured CA even when
  verification is disabled, so a malformed optional CA can otherwise prevent
  startup. Do not silently turn verification back on.
- Offer CA verification as an optional later migration. Document a check for
  CA chain and certificate SAN matching the exact IP/hostname in the CSI
  Secret, with Helm examples using `controller.tls.caBundle`,
  `existingConfigMap`, or `existingSecret`. Do not require this migration
  before consuming the driver fixes.
- Validate chart configuration and, when configured, the mounted CA file at
  controller startup.
  Array addresses and credentials arrive with CSI RPC secrets, so startup
  cannot prove an authenticated handshake. Provide a pre-upgrade authenticated
  connectivity check using the actual Secret, selected TLS mode, and each
  configured management address; surface a clear TLS handshake diagnostic on
  live RPC failure without leaking credentials. Test both addresses when
  failover is configured.
- Follow up in `seagate-exos-x-api-go` so login accepts an injected HTTP
  client. Remove the process-wide `http.DefaultClient` assignment after the
  dependency supports this; preserve both TLS modes through that change.

**Acceptance.** With `controller.tls.insecureSkipVerify: true` and no CA bundle,
the current deployment passes a pre-upgrade connection check and continues to
attach, detach, and provision across the driver upgrade. A separate opt-in CA
test succeeds with a trusted CA and fails clearly for an unknown CA or wrong
SAN. Tests confirm that the chart default remains verification-on, the explicit
override remains effective, and both modes work with every configured
management endpoint. Test `insecureSkipVerify: true` with a malformed CA
setting and verify it still connects with a warning. The consuming deployment
selects and tests its intended mode before rollout; a CA migration is not
required for this release.

## 8. Controller serialization and timeout behavior

**Disposition: DEFERRED as a full plan item; shared-client serialization
DONE.** Commit `964662e` replaced per-method locks with one boundary around
authenticated RPCs. Its focused/race tests passed, and five live concurrent
primary/backup create/attach/detach cycles passed after a pre-fix cross-array
misrouting reproduction. The global lock is still not context-aware; the
array API calls do not have the end-to-end cancellation/uncertain-operation
handling or per-array client isolation required below. Those gates remain
open. The tested `964662e` controller was a one-off image, not the ISO-pinned
release artifact.

**Problem.** `pkg/controller/controller.go` correctly serializes authenticated
RPCs around one mutable array client, but a global `sync.Mutex` makes all
arrays wait for each other and does not stop a timed-out request from waiting
for the lock.

**Implementation.**

- Keep the serialization until the shared client is replaced. Use a
  context-aware admission mechanism and check cancellation after admission
  and before any array mutation. Derive login and API-call contexts from the
  incoming CSI request instead of `context.Background()`. Extend or upgrade
  `seagate-exos-x-api-go` so login, authenticated operations, and management
  failover attempts all accept/propagate a caller deadline; make that
  dependency change a prerequisite for claiming in-flight cancellation. For
  any array call that cannot be interrupted, finish by verifying its outcome
  and return an explicitly retryable uncertain result rather than starting a
  subsequent mutation after the deadline. Keep the full client-configuration
  and RPC
  lifetime inside the critical section.
- Longer term, create independent array clients keyed by a stable array
  identity, with credentials and HTTP transport scoped to that client. Keep
  serialization across every call sharing a mutable client until the API
  library is proven reentrant; only then consider narrower per-volume locks.
  Avoid mutable process-wide authentication state.
- Measure queue wait and operation duration separately; test under node drain
  and bulk PVC deletion with the configured sidecar timeouts.

**Acceptance.** A cancelled queued call performs no later array mutation.
An in-flight login and each API/failover request honor cancellation; tests use
the actual API client path, not only a mocked controller handler. An
interrupted or timed-out array call cannot be followed by an unverified
mutation; retries reconcile any uncertain array result before repeating it.
Concurrent calls to different arrays cannot exchange credentials or endpoints.
Bulk detach stays within documented timeout/retry behavior, or sidecar
timeouts are adjusted based on measured worst-case operations.

## 9. Same-node multi-pod mount provenance and detach safety

**Disposition: DEFERRED.** The earlier same-node publication/open-map guards
are present, but this review's provenance and different-pod-volume-name gaps
remain. `Unmount` still invokes `umount -l`, and no live same-PV/different-name
or busy-file-descriptor matrix from this item was recorded. Do not infer this
item passed from the single-pod disposable attach/detach or reboot tests.
FC/SAS-specific coverage is disregarded for the current iSCSI-only deployment.

**Problem.** The new filesystem path bind-mounts from the first mount reported
for a device, even if that mount was made manually or by another component.
The other-publication check searches pod targets using the current pod's
volume-directory name; two pods can give the same PV different local names.
That can miss a surviving publication during unpublish, especially on FC/SAS
where the storage-specific fallback does not check multipath open count.

**Implementation.**

- Select a bind source only from a verified kubelet CSI publication owned by
  this driver and the requested volume ID. Persist driver-owned per-target
  metadata at successful `NodePublishVolume` and compare it with kubelet CSI
  volume metadata, mountinfo, and underlying block major:minor (plus exact
  WWID where available). Device identity alone does not prove CSI-driver
  ownership. Do not assume the first `findmnt` result is safe. If the device
  is mounted only outside this driver's publication tree, fail with
  `FailedPrecondition` and leave that mount untouched. If multiple candidate
  sources disagree or ownership cannot be proved, fail closed rather than
  bind an arbitrary filesystem. For mounts published by an older driver with
  no per-target record, reconstruct ownership only from trustworthy kubelet
  metadata and live exact device identity; otherwise preserve the existing
  mount and require a drain/manual migration before a second pod binds from it.
- Determine remaining publications by volume/device identity across *all*
  kubelet CSI pod targets, not by matching one pod's volume-directory name.
  Confirm actual mount/bind state using mountinfo; a path that merely exists
  is not proof of an active publication. Recheck under the per-volume lock
  before final detach. For FC/SAS multipath devices, add the same mounted/open
  guard used for iSCSI before calling detach; do not rely on `findmnt` alone.
  For single-path devices, use a validated open-reference check or fail closed.
- Replace the current `umount -l` on CSI unpublish with a normal unmount.
  Propagate `EBUSY` as a retryable refusal to detach, verify the target is no
  longer mounted, then check remaining mounts and device open references.
  Lazy unmount hides a still-open filesystem from mountinfo and is not proof
  that its users have finished. Never force-remove an open multipath map.
- Keep per-target read-only settings and idempotent retries intact. Never
  unmount or remount a different pod's target when handling one pod's request.

**Acceptance.** A manually mounted device is not adopted as a CSI bind source.
Two pods can use the same PV with different pod volume names; unpublishing one
does not disconnect storage or alter the other's mount, for iSCSI, FC, and SAS.
The final unpublish detaches only after all targets and open references are
gone. A busy target with an open file descriptor returns an error and leaves
storage attached. Test read-only and read-write targets, stale target
directories, a pre-upgrade CSI target without metadata, another CSI driver's
target on the same device, and an
ambiguous mount source; use disposable volumes for live protocol tests.

## 10. Recover partial iSCSI connection failures

**Disposition: DEFERRED.** The tested publish path has exact-WWID checks and
some failure rollback after a successful connection, but `AttachStorage`
still returns directly from failed preparation or `iscsilib.Connect`, and
`NodePublishVolume` still returns directly from mount failure. The ownership
snapshot, partial-portal failure handler, durable pending intent, and fault
injection matrix below were not implemented or qualified. The five-cycle
reboot series is not a substitute for these failed-publish edges.

**Problem.** `AttachStorage` returns immediately when `iscsilib.Connect`
fails, although the library may already have logged in or discovered some
paths. `prepareISCSIMultipathMap` can also rescan/create host state before it
returns an error. The existing exact-WWID rollback only runs after a successful
`Connect` and does not prove that the map was created by this attempt; it could
remove a pre-existing map. A subsequent filesystem/raw-block mount failure
also returns from `NodePublishVolume` without reconciling host attach state.

**Implementation.**

- Before preparation/connection, snapshot whether the expected WWID map and
  relevant sessions/paths already exist. Use one ownership-aware failure
  handler for preparation, `Connect`, post-connect identity/capacity checks,
  connector persistence, and filesystem/raw-block mount failures in
  `NodePublishVolume`. At each edge inspect exact WWID, generation, target
  mounts, and open state. If this attempt provably created an unused map,
  remove only that map and verify the result. Never run the current post-Connect
  rollback on a pre-existing map merely because its WWID matches. Preserve
  shared iSCSI sessions; never broadly log out a target to clean one failed
  LUN.
- If identity, ownership, or open state is uncertain, fail closed and surface
  a specific retry/manual-recovery diagnostic. Persist a local pending-cleanup
  intent only when this failed attempt's generation and cleanup ownership are
  known, before returning when partial state may rediscover after the error;
  item 2's reconciler handles it under the cross-node cleanup claim. An
  uncertain or pre-existing map stays attached for safe retry/manual recovery.

**Acceptance.** Inject failure after the first of multiple portals logs in,
after path discovery, during multipath lookup, during preparation, during
post-connect validation/persistence, and during filesystem/raw-block mount.
Verify that known-new unused exact-WWID state is removed, pre-existing or
in-use maps and shared sessions remain untouched, and uncertain state cannot
be silently reported as a successful attach or cleaned by broad logout.

## Separate pre-existing FC/SAS map concurrency fix

**Disposition: DISREGARDED for the tested iSCSI-only release; still open
upstream.** This map race was not fixed or tested. Do not claim FC/SAS support
has been qualified by the PowerVault iSCSI lab work. Reopen before FC/SAS use.

`SASandFCRemovedDevicesMap` is modified in `pkg/storage/fcNode.go` and
`pkg/storage/sasNode.go`, iterated in `pkg/storage/storageService.go`, and
deleted in `pkg/node_service/node_service_server.go` without one shared lock.
Protect all accesses with a mutex or encapsulate the state in a synchronized
type. Do not hold that lock while issuing slow detach commands: take a
snapshot, reconcile, then update entries with a per-entry identity check.
Run a race test with concurrent FC/SAS unpublish and notification, and verify
the node plugin cannot panic from concurrent map iteration and writes.

## Release gate

**Disposition: DEFERRED.** The blanket multi-protocol/mixed-version gate below
is broader than the chosen fresh-install iSCSI release and is not met. The
scoped iSCSI lab gates passed in the external qualification record, but the
unified prerelease image containing `964662e` for both controller and nodes,
pinned into a new ISO, remains unqualified on a live cluster. Clone,
mixed-host SCSI scan, partial-publish failure, same-node mount provenance,
host-tool readiness, and unauthenticated callback risks above remain explicit
deferrals; a release decision must accept or remediate them rather than
silently count them as passing tests. FC/SAS and old-to-new driver rollout
gates are disregarded only for the stated deployment scope.

Before consuming the branch, require passing focused unit/race tests, CSI
sanity checks in a suitable environment, the mixed-version test matrix, and
live disposable-volume tests for attach, detach, node reboot, wrong LUN,
clone/restore, and connectivity under the consuming deployment's chosen TLS
mode (`controller.tls.insecureSkipVerify: true` is supported). Test CA mode
separately where feasible, but do not require an actual CA migration for this
release. Explicitly exercise still-attached reboot, late rediscovery,
dead-node return, FC/SAS rediscovery after reboot or udev-link loss, stale
iSCSI paths without udev links, all failed-publish rollback edges, busy normal
unmount, manual/non-CSI mount rejection, same-PV pods with different volume
names, same-name ordinary-create/delete versus clone retries, and each
authentication/cleanup-record rollout gate. Prove the CRD and node bridge work
in the rendered chrooted DaemonSet; test delayed array operations across
controller replacement and a node cleanup claim racing republish. Record
which protocols and node distributions were actually tested. The existing
`pkg/common` name-length
test failure and the sanity test's `/var/run` permission requirement must be
resolved or run in an appropriate environment before claiming a green
repository-wide suite.
