//
// Copyright (c) 2022 Seagate Technology LLC and/or its Affiliates
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// For any questions about this software or licensing,
// please email opensource@seagate.com or cortx-questions@seagate.com.

package storage

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	iscsilib "github.com/Seagate/csi-lib-iscsi/iscsi"
	"github.com/Seagate/seagate-exos-x-csi/pkg/common"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

// Configuration constants
const (
	BlkidTimeout                          = 10
	maxDmnameAttempts                     = 18
	dmnameDelay                           = 10
	missingConnectorReconcileAttempts     = 46
	missingConnectorReconcilePollInterval = time.Second
	controllerUnmapReconcileAttempts      = 17
	controllerUnmapReconcilePollInterval  = 500 * time.Millisecond
)

// NodeStageVolume mounts the volume to a staging path on the node. This is
// called by the CO before NodePublishVolume and is used to temporary mount the
// volume to a staging path. Once mounted, NodePublishVolume will make sure to
// mount it to the appropriate path
// Will not be called as the plugin does not have the STAGE_UNSTAGE_VOLUME capability
func (iscsi *iscsiStorage) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeStageVolume is not implemented")
}

// NodeUnstageVolume unstages the volume from the staging path
// Will not be called as the plugin does not have the STAGE_UNSTAGE_VOLUME capability
func (iscsi *iscsiStorage) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeUnstageVolume is not implemented")
}

func (iscsi *iscsiStorage) AttachStorage(ctx context.Context, req *csi.NodePublishVolumeRequest) (string, error) {
	wwn, err := common.VolumeIdGetWwn(req.GetVolumeId())
	if err != nil {
		return "", status.Error(codes.InvalidArgument, err.Error())
	}
	iqn := req.GetVolumeContext()["iqn"]
	if strings.TrimSpace(iqn) == "" {
		return "", status.Error(codes.InvalidArgument, "iSCSI target IQN is empty")
	}
	portals := strings.Split(req.GetVolumeContext()["portals"], ",")
	klog.InfoS("iSCSI connection info:", "iqn", iqn, "portals", portals)

	lun, err := strconv.ParseInt(req.GetPublishContext()["lun"], 10, 32)
	if err != nil || lun < 0 {
		return "", status.Errorf(codes.InvalidArgument, "invalid iSCSI LUN %q", req.GetPublishContext()["lun"])
	}
	klog.InfoS("LUN:", "lun", lun)

	klog.InfoS("initiating ISCSI connection...")
	targets := make([]iscsilib.TargetInfo, 0)
	for _, portal := range portals {
		if portal != "" {
			klog.V(1).InfoS("-- add iqn and portal targets", "iqn", iqn, "portal", portal)
			targets = append(targets, iscsilib.TargetInfo{
				Iqn:    iqn,
				Portal: portal,
			})
			// test and produce a warning if path already exists before iscsi login
			devicePath := fmt.Sprintf("/dev/disk/by-path/ip-%s:3260-iscsi-%s-lun-%d", portal, iqn, lun)
			_, err := os.Stat(devicePath)
			klog.V(4).InfoS("[TEST] os stat device:", "exist", !os.IsNotExist(err), "device", devicePath)
			if !os.IsNotExist(err) {
				_, err := os.Stat(devicePath)
				klog.V(4).InfoS("WARNING: device exists before iscsi login:", "devicePath", devicePath, "os.Stat error", err)
			}
		}
	}
	if len(targets) == 0 {
		return "", status.Error(codes.InvalidArgument, "iSCSI portal list is empty")
	}

	// If CHAP secrets have been specified, include them in the iscsilib Connector
	doCHAPAuth := false
	authType := ""
	var iscsiSecrets iscsilib.Secrets
	if reqSecrets := req.GetSecrets(); reqSecrets != nil {
		CHAPusername := reqSecrets[common.CHAPUsernameKey]
		CHAPpassword := reqSecrets[common.CHAPSecretKey]
		CHAPusernameIn := reqSecrets[common.CHAPUsernameInKey]
		CHAPpasswordIn := reqSecrets[common.CHAPPasswordInKey]
		if CHAPusername != "" && CHAPpassword != "" {
			doCHAPAuth = true
			authType = "chap"
			iscsiSecrets = iscsilib.Secrets{
				SecretsType: "chap",
				UserName:    CHAPusername,
				Password:    CHAPpassword,
				UserNameIn:  CHAPusernameIn,
				PasswordIn:  CHAPpasswordIn,
			}
		}
	}

	klog.V(4).InfoS("iscsi connector setup", "AuthType", authType, "Targets", targets, "Lun", lun)
	connector := &iscsilib.Connector{
		AuthType:         authType,
		Targets:          targets,
		Lun:              int32(lun),
		DoDiscovery:      true,
		DoCHAPDiscovery:  doCHAPAuth,
		DiscoverySecrets: iscsiSecrets,
		SessionSecrets:   iscsiSecrets,
		RetryCount:       20,
	}
	if err := prepareISCSIMultipathMap(iqn, int(lun), wwn); err != nil {
		return "", status.Errorf(codes.FailedPrecondition,
			"iSCSI pre-attach validation failed for WWN %s at LUN %d: %v", wwn, lun, err)
	}

	path, err := iscsilib.Connect(connector)
	if err != nil {
		return "", err
	}
	klog.InfoS("attached device:", "path", path)

	exists := true
	out, err := exec.Command("ls", "-l", fmt.Sprintf("/dev/disk/by-id/dm-name-3%s", wwn)).CombinedOutput()
	klog.V(1).InfoS("ls command output", "command", fmt.Sprintf("ls -l /dev/disk/by-id/dm-name-3%s", wwn), "err", err, "out", out)
	if err != nil {
		exists = false
	}

	// wait here until the dm-name exists, for debugging
	if !exists {
		attempts := 1
		for attempts < (maxDmnameAttempts + 1) {
			// Force a reload of all existing multipath maps
			output, err := exec.Command("multipath", "-r").CombinedOutput()
			klog.V(4).InfoS("## (publish) multipath -r output", "err", err, "output", output)

			out, err := exec.Command("ls", "-l", fmt.Sprintf("/dev/disk/by-id/dm-name-3%s", wwn)).CombinedOutput()
			klog.V(1).InfoS("check for dm-name exists", "attempt", attempts, "command", fmt.Sprintf("ls -l /dev/disk/by-id/dm-name-3%s", wwn), "err", err, "out", out)
			if err == nil {
				exists = true
				break
			}
			time.Sleep(dmnameDelay * time.Second)
			attempts++
		}
	}

	inspection, err := inspectISCSIMultipathMap(wwn)
	if err != nil {
		rollbackErr := rollbackFailedISCSIAttach(
			wwn,
			inspectISCSIMultipathDetachIdentity,
			IsVolumeInUse,
			IsMultipathDeviceOpen,
			iscsilib.DisconnectVolume,
		)
		if rollbackErr != nil {
			return "", status.Errorf(codes.FailedPrecondition,
				"iSCSI device validation failed for WWN %s: %v; rollback failed closed: %v",
				wwn, err, rollbackErr)
		}
		return "", status.Errorf(codes.FailedPrecondition,
			"iSCSI device validation failed for WWN %s: %v; unused exact-WWID host state rolled back",
			wwn, err)
	}
	// Connect can discover a map by target and numeric LUN. Always replace its
	// result with the map selected by the expected volume WWN after validating
	// every live path and every capacity layer.
	path = inspection.Device
	connector.DevicePath = inspection.Device
	connector.Multipath = true
	klog.InfoS("validated iSCSI multipath device",
		"device", inspection.Device,
		"wwid", inspection.WWID,
		"paths", len(inspection.SlaveNames),
		"capacityBytes", inspection.Capacity)

	if _, err := os.Stat(iscsi.connectorInfoPath); err == nil {
		klog.InfoS("iscsi connection file already exists", "connectorInfoPath", iscsi.connectorInfoPath)
	}

	klog.InfoS("saving ISCSI connection info", "connectorInfoPath", iscsi.connectorInfoPath)
	if _, err := os.Stat(iscsi.connectorInfoPath); err == nil {
		klog.InfoS("iscsi connection file already exists", "connectorInfoPath", iscsi.connectorInfoPath)
	}
	err = iscsilib.PersistConnector(connector, iscsi.connectorInfoPath)
	if err != nil {
		rollbackErr := rollbackFailedISCSIAttach(
			wwn,
			inspectISCSIMultipathDetachIdentity,
			IsVolumeInUse,
			IsMultipathDeviceOpen,
			iscsilib.DisconnectVolume,
		)
		if rollbackErr != nil {
			return "", status.Errorf(codes.Internal,
				"persist iSCSI connector for WWN %s: %v; rollback failed closed: %v",
				wwn, err, rollbackErr)
		}
		return "", err
	}

	return path, nil
}

func (iscsi *iscsiStorage) DetachStorage(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) error {
	wwn, err := common.VolumeIdGetWwn(req.GetVolumeId())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	expectedWWID, err := canonicalMultipathWWID(wwn)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	klog.Infof("loading ISCSI connection info from %s", iscsi.connectorInfoPath)
	connector, err := iscsilib.GetConnectorFromFile(iscsi.connectorInfoPath)
	if err != nil {
		if os.IsNotExist(err) {
			mapDevice, inaccessiblePaths, alreadyClean, inspectErr := waitForMissingISCSIConnectorState(
				ctx,
				wwn,
				expectedWWID,
				missingConnectorReconcileAttempts,
				missingConnectorReconcilePollInterval,
				inspectISCSIMultipathDetachIdentity,
				findSCSIPathsByWWID,
			)
			if inspectErr != nil {
				return status.Errorf(codes.Aborted, "cannot safely reconcile missing iSCSI connector state: %v", inspectErr)
			}
			if alreadyClean {
				klog.InfoS("missing iSCSI connector reconciled after stable-absence window: no exact map or paths remain",
					"wwid", expectedWWID,
					"window", time.Duration(missingConnectorReconcileAttempts-1)*missingConnectorReconcilePollInterval)
				return nil
			}
			connector = &iscsilib.Connector{DevicePath: mapDevice, Multipath: true}
			klog.InfoS("reconstructed missing iSCSI connector from live exact-WWID map",
				"device", mapDevice, "wwid", expectedWWID, "inaccessiblePaths", inaccessiblePaths)
		} else {
			return status.Error(codes.Internal, err.Error())
		}
	}

	if connector.Multipath {
		mapDevice, inaccessiblePaths, inspectErr := inspectISCSIMultipathDetachIdentity(wwn)
		if inspectErr != nil {
			if os.IsNotExist(inspectErr) {
				paths, pathsErr := findSCSIPathsByWWID(expectedWWID)
				if pathsErr != nil {
					return status.Errorf(codes.Aborted, "cannot validate persisted iSCSI connector state: %v", pathsErr)
				}
				if len(paths) == 0 {
					if removeErr := os.Remove(iscsi.connectorInfoPath); removeErr != nil && !os.IsNotExist(removeErr) {
						return status.Errorf(codes.Internal, "remove stale iSCSI connector: %v", removeErr)
					}
					klog.InfoS("persisted iSCSI connector reconciled: no exact map or paths remain", "wwid", expectedWWID)
					return nil
				}
			}
			return status.Errorf(codes.Aborted, "persisted iSCSI connector does not match safe live state: %v", inspectErr)
		}
		connector.DevicePath = mapDevice
		if inaccessiblePaths > 0 {
			klog.InfoS("validated exact-WWID multipath map for detach with unavailable paths",
				"device", mapDevice, "wwid", expectedWWID, "inaccessiblePaths", inaccessiblePaths)
		}
	} else {
		alreadyGone, inspectErr := validatePersistedSinglePath(connector.DevicePath, expectedWWID)
		if inspectErr != nil {
			return status.Errorf(codes.Aborted, "cannot validate persisted iSCSI connector state: %v", inspectErr)
		}
		if alreadyGone {
			if removeErr := os.Remove(iscsi.connectorInfoPath); removeErr != nil && !os.IsNotExist(removeErr) {
				return status.Errorf(codes.Internal, "remove stale iSCSI connector: %v", removeErr)
			}
			klog.InfoS("persisted single-path iSCSI connector reconciled: device no longer exists",
				"device", connector.DevicePath, "wwid", expectedWWID)
			return nil
		}
	}
	klog.InfoS("connector.DevicePath", "connector.DevicePath", connector.DevicePath)

	if IsVolumeInUse(connector.DevicePath) {
		return status.Errorf(codes.Aborted, "volume %s is still mounted on the node; keeping it attached", connector.DevicePath)
	}

	_, err = os.Stat(connector.DevicePath)
	if err != nil && os.IsNotExist(err) {
		klog.InfoS("connector.devicePath does not exist, assuming that volume is already disconnected")
		return nil
	}

	if connector.Multipath {
		deviceOpen, err := IsMultipathDeviceOpen(connector.DevicePath)
		if err != nil {
			// Failing closed is safer than allowing DisconnectVolume to force
			// remove a map whose open state could not be established.
			return status.Error(codes.Aborted, err.Error())
		}
		if deviceOpen {
			return status.Errorf(codes.Aborted, "multipath device %s is still open; keeping it attached", connector.DevicePath)
		}
	}

	out, err := exec.Command("ls", "-l", fmt.Sprintf("/dev/disk/by-id/dm-name-3%s", wwn)).CombinedOutput()
	klog.Infof("check for dm-name: ls -l %s, err = %v, out = \n%s", fmt.Sprintf("/dev/disk/by-id/dm-name-3%s", wwn), err, string(out))

	klog.Info("DisconnectVolume, detaching ISCSI device")
	err = iscsilib.DisconnectVolume(*connector)
	if err != nil {
		return err
	}

	klog.Infof("deleting ISCSI connection info file %s", iscsi.connectorInfoPath)
	os.Remove(iscsi.connectorInfoPath)
	return nil
}

type inspectDetachMapFunc func(string) (string, int, error)
type findSCSIPathsFunc func(string) ([]string, error)
type volumeInUseFunc func(string) bool
type multipathOpenFunc func(string) (bool, error)
type disconnectISCSIFunc func(iscsilib.Connector) error

// rollbackFailedISCSIAttach removes only an unused exact-WWID map. Attach can
// create host SCSI and multipath state before final identity/capacity
// validation or connector persistence fails. Kubelet does not reliably issue
// NodeUnpublishVolume after a failed NodePublishVolume, so leaving that state
// behind recreates the stale-LUN prerequisite. Any ambiguity, mount, or open
// reference fails closed and is surfaced with the original publish error.
func rollbackFailedISCSIAttach(
	volumeWWN string,
	inspectMap inspectDetachMapFunc,
	volumeInUse volumeInUseFunc,
	multipathOpen multipathOpenFunc,
	disconnect disconnectISCSIFunc,
) error {
	expectedWWID, err := canonicalMultipathWWID(volumeWWN)
	if err != nil {
		return err
	}
	mapDevice, inaccessiblePaths, err := inspectMap(volumeWWN)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if volumeInUse(mapDevice) {
		return fmt.Errorf("exact-WWID map %s for %s is mounted", mapDevice, expectedWWID)
	}
	open, err := multipathOpen(mapDevice)
	if err != nil {
		return fmt.Errorf("establish open state for exact-WWID map %s: %w", mapDevice, err)
	}
	if open {
		return fmt.Errorf("exact-WWID map %s for %s is open", mapDevice, expectedWWID)
	}
	if err := disconnect(iscsilib.Connector{DevicePath: mapDevice, Multipath: true}); err != nil {
		return fmt.Errorf("disconnect exact-WWID map %s: %w", mapDevice, err)
	}
	klog.InfoS("rolled back unused exact-WWID iSCSI map after attach failure",
		"device", mapDevice, "wwid", expectedWWID, "inaccessiblePaths", inaccessiblePaths)
	return nil
}

// ReconcileControllerUnmappedISCSI is the node-side completion barrier for an
// array unmap. In particular, it catches an exact-WWID map reconstructed by
// late iSCSI discovery after kubelet's NodeUnpublishVolume already returned.
// A clean node must remain clean across the full observation window. If a map
// is removed late in that window, the call fails so the controller retry runs
// another complete clean window before the VolumeAttachment can disappear.
func ReconcileControllerUnmappedISCSI(ctx context.Context, volumeWWN string) error {
	return reconcileControllerUnmappedISCSI(
		ctx,
		volumeWWN,
		controllerUnmapReconcileAttempts,
		controllerUnmapReconcilePollInterval,
		inspectISCSIMultipathDetachIdentity,
		IsVolumeInUse,
		IsMultipathDeviceOpen,
		iscsilib.DisconnectVolume,
	)
}

func reconcileControllerUnmappedISCSI(
	ctx context.Context,
	volumeWWN string,
	attempts int,
	pollInterval time.Duration,
	inspectMap inspectDetachMapFunc,
	volumeInUse volumeInUseFunc,
	multipathOpen multipathOpenFunc,
	disconnect disconnectISCSIFunc,
) error {
	if attempts < 2 {
		return status.Error(codes.Internal, "post-unmap iSCSI reconciliation requires at least two inspections")
	}

	expectedWWID, err := canonicalMultipathWWID(volumeWWN)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	cleanInspections := 0

	for attempt := 1; attempt <= attempts; attempt++ {
		mapDevice, inaccessiblePaths, inspectErr := inspectMap(volumeWWN)
		switch {
		case inspectErr == nil:
			cleanInspections = 0
			if volumeInUse(mapDevice) {
				return status.Errorf(codes.Aborted,
					"post-unmap iSCSI map %s for WWID %s is still mounted", mapDevice, expectedWWID)
			}
			open, openErr := multipathOpen(mapDevice)
			if openErr != nil {
				return status.Errorf(codes.Aborted,
					"cannot establish open state for post-unmap iSCSI map %s: %v", mapDevice, openErr)
			}
			if open {
				return status.Errorf(codes.Aborted,
					"post-unmap iSCSI map %s for WWID %s is still open", mapDevice, expectedWWID)
			}
			if disconnectErr := disconnect(iscsilib.Connector{DevicePath: mapDevice, Multipath: true}); disconnectErr != nil {
				return status.Errorf(codes.Aborted,
					"remove post-unmap iSCSI map %s for WWID %s: %v", mapDevice, expectedWWID, disconnectErr)
			}
			klog.InfoS("removed exact-WWID iSCSI map discovered after controller unmap",
				"device", mapDevice, "wwid", expectedWWID, "inaccessiblePaths", inaccessiblePaths)
		case os.IsNotExist(inspectErr):
			cleanInspections++
		default:
			return status.Errorf(codes.Aborted,
				"cannot safely inspect post-unmap iSCSI state for WWID %s: %v", expectedWWID, inspectErr)
		}

		if cleanInspections == attempts {
			klog.InfoS("post-controller-unmap iSCSI state remained clean",
				"wwid", expectedWWID,
				"window", time.Duration(attempts-1)*pollInterval)
			return nil
		}
		if attempt == attempts {
			return status.Errorf(codes.Aborted,
				"post-unmap iSCSI map for WWID %s did not remain absent for the complete %s window",
				expectedWWID, time.Duration(attempts-1)*pollInterval)
		}

		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return status.Error(codes.Aborted, ctx.Err().Error())
		case <-timer.C:
		}
	}

	return status.Error(codes.Internal, "post-unmap iSCSI reconciliation ended without a result")
}

// waitForMissingISCSIConnectorState closes the reboot race between kubelet's
// first NodeUnpublishVolume calls and late iSCSI/multipath discovery. During a
// node boot the old map may not exist when kubelet asks for unpublish, then be
// reconstructed seconds later when sessions return. Do not report idempotent
// success until the exact map has remained absent for the complete window and
// a final live path scan is also empty.
func waitForMissingISCSIConnectorState(
	ctx context.Context,
	volumeWWN string,
	expectedWWID string,
	attempts int,
	pollInterval time.Duration,
	inspectMap inspectDetachMapFunc,
	findPaths findSCSIPathsFunc,
) (mapDevice string, inaccessiblePaths int, alreadyClean bool, err error) {
	if attempts < 1 {
		return "", 0, false, fmt.Errorf("missing-connector reconciliation requires at least one inspection")
	}

	for attempt := 1; attempt <= attempts; attempt++ {
		mapDevice, inaccessiblePaths, inspectErr := inspectMap(volumeWWN)
		if inspectErr == nil {
			return mapDevice, inaccessiblePaths, false, nil
		}
		if !os.IsNotExist(inspectErr) {
			return "", 0, false, inspectErr
		}

		if attempt == attempts {
			paths, pathsErr := findPaths(expectedWWID)
			if pathsErr != nil {
				return "", 0, false, pathsErr
			}
			if len(paths) > 0 {
				return "", 0, false, fmt.Errorf(
					"no exact multipath map for WWID %s, but %d matching SCSI paths remain",
					expectedWWID, len(paths))
			}
			return "", 0, true, nil
		}

		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return "", 0, false, ctx.Err()
		case <-timer.C:
		}
	}

	return "", 0, false, fmt.Errorf("missing-connector reconciliation ended without a result")
}

func (iscsi *iscsiStorage) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "iSCSI specific NodePublishVolume not implemented")
}

// NodeUnpublishVolume unmounts the volume from the target path
func (iscsi *iscsiStorage) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "iSCSI specific NodeUnpublishVolume not implemented")
}

// NodeGetVolumeStats return info about a given volume
// Will not be called as the plugin does not have the GET_VOLUME_STATS capability
func (iscsi *iscsiStorage) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeGetVolumeStats is not implemented")
}

// NodeExpandVolume finalizes volume expansion on the node
func (iscsi *iscsiStorage) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {

	volumeName, _ := common.VolumeIdGetName(req.GetVolumeId())
	wwn, wwnErr := common.VolumeIdGetWwn(req.GetVolumeId())
	volumepath := req.GetVolumePath()
	klog.V(2).Infof("NodeExpandVolume: VolumeId=%v,  VolumePath=%v", volumeName, volumepath)

	if len(volumeName) == 0 {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("node expand volume requires volume id"))
	}

	if len(volumepath) == 0 {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("node expand volume requires volume path"))
	}
	if wwnErr != nil {
		return nil, status.Error(codes.InvalidArgument, wwnErr.Error())
	}

	connector, err := iscsilib.GetConnectorFromFile(iscsi.connectorInfoPath)
	klog.V(3).Infof("GetConnectorFromFile(%s) connector: %v, err: %v", volumeName, connector, err)

	if err != nil {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("node expand volume path not found for volume id (%s)", volumeName))
	}

	if connector.Multipath {
		klog.V(2).Info("device is using multipath")
		inspection, resizeErr := rescanAndResizeISCSIMultipath(wwn)
		if resizeErr != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "iSCSI expansion validation failed: %v", resizeErr)
		}
		connector.DevicePath = inspection.Device
		klog.InfoS("rescanned and validated expanded iSCSI multipath device",
			"device", inspection.Device,
			"wwid", inspection.WWID,
			"paths", len(inspection.SlaveNames),
			"capacityBytes", inspection.Capacity)
	} else {
		klog.V(2).Info("device is NOT using multipath")
	}

	if req.GetVolumeCapability().GetMount() != nil {
		klog.Infof("expanding filesystem using resize2fs on device %s", connector.DevicePath)
		output, err := exec.Command("resize2fs", connector.DevicePath).CombinedOutput()
		if err != nil {
			klog.V(2).InfoS("could not resize filesystem", "resize2fs output", output)
			return nil, fmt.Errorf("could not resize filesystem: %v", output)
		}
	}

	return &csi.NodeExpandVolumeResponse{}, nil
}

// NodeGetCapabilities returns the supported capabilities of the node server
func (iscsi *iscsiStorage) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeGetCapabilities is not implemented")
}

// NodeGetInfo returns info about the node
func (iscsi *iscsiStorage) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeGetInfo is not implemented")
}

func GetISCSIInitiators() ([]string, error) {
	initiatorNameFilePath := "/etc/iscsi/initiatorname.iscsi"
	file, err := os.Open(initiatorNameFilePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if equal := strings.Index(line, "="); equal >= 0 {
			if strings.TrimSpace(line[:equal]) == "InitiatorName" {
				return []string{strings.TrimSpace(line[equal+1:])}, nil
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return nil, fmt.Errorf("InitiatorName key is missing from %s", initiatorNameFilePath)
}
