//
// Copyright (c) 2021 Seagate Technology LLC and/or its Affiliates
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
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	saslib "github.com/Seagate/csi-lib-sas/sas"
	"github.com/Seagate/seagate-exos-x-csi/pkg/common"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

const (
	SASAddressFilePath = "/etc/kubernetes/sas-addresses"
	FCAddressFilePath  = "/etc/kubernetes/fc-addresses"
)

type StorageOperations interface {
	csi.NodeServer
	AttachStorage(ctx context.Context, req *csi.NodePublishVolumeRequest) (string, error)
	DetachStorage(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) error
}

type commonService struct {
	storagePoolIdName map[int64]string
	driverVersion     string
}

type fcStorage struct {
	cs                commonService
	connectorInfoPath string
}

type iscsiStorage struct {
	cs                commonService
	connectorInfoPath string
}

type sasStorage struct {
	cs                commonService
	connectorInfoPath string
}

// Map of device WWNs to timestamp of when they were unpublished from the node
var SASandFCRemovedDevicesMap = map[string]time.Time{}

// buildCommonService:
func buildCommonService(config map[string]string) (commonService, error) {
	commonserv := commonService{}
	commonserv.driverVersion = config["driverversion"]
	klog.V(2).Infof("buildCommonService commonservice configuration done.")
	return commonserv, nil
}

// NewStorageNode : To return specific implementation of storage
func NewStorageNode(storageProtocol string, config map[string]string) (StorageOperations, error) {
	comnserv, err := buildCommonService(config)
	if err == nil {
		storageProtocol = strings.TrimSpace(storageProtocol)
		klog.V(2).Infof("NewStorageNode for (%s)", storageProtocol)
		if storageProtocol == common.StorageProtocolFC {
			return &fcStorage{cs: comnserv, connectorInfoPath: config["connectorInfoPath"]}, nil
		} else if storageProtocol == common.StorageProtocolSAS {
			return &sasStorage{cs: comnserv, connectorInfoPath: config["connectorInfoPath"]}, nil
		} else if storageProtocol == common.StorageProtocolISCSI {
			return &iscsiStorage{cs: comnserv, connectorInfoPath: config["connectorInfoPath"]}, nil
		} else {
			klog.Warningf("Invalid or no storage protocol specified (%s)", storageProtocol)
			klog.Warningf("Expecting storageProtocol (iscsi, fc, sas, etc) in StorageClass YAML. Default of (%s) used.", common.StorageProtocolISCSI)
			return &iscsiStorage{cs: comnserv, connectorInfoPath: config["connectorInfoPath"]}, nil
		}
	}
	return nil, err
}

// ValidateStorageProtocol: Verifies that a correct protocol is chosen or returns a valid default storage protocol.
func ValidateStorageProtocol(storageProtocol string) string {
	if storageProtocol == common.StorageProtocolFC || storageProtocol == common.StorageProtocolISCSI || storageProtocol == common.StorageProtocolSAS {
		return storageProtocol
	} else {
		klog.Warningf("Invalid or no storage protocol specified (%s)", storageProtocol)
		klog.Warningf("Expecting storageProtocol (iscsi, fc, sas, etc) in StorageClass YAML. Default of (%s) used.", common.StorageProtocolISCSI)
		return common.StorageProtocolISCSI
	}
}

// gateKeepers is a thread safe map indexed by volume name.
var gatekeepers = common.NewStringLock()

// addGatekeeper: Ensure that NodePublishVolume and NodeUnpublishVolume are only called once per volume
func AddGatekeeper(volumeName string) {
	klog.V(4).Infof("[LOCK] volume (%s) gatekeeper", volumeName)
	gatekeepers.Lock(volumeName)
}

// removeGatekeeper: Unlock the volume function mutex when the Publish/Unpublish is complete
func RemoveGatekeeper(volumeName string) {
	klog.V(4).Infof("[UNLOCK] volume (%s) gatekeeper", volumeName)
	gatekeepers.Unlock(volumeName)
}

// wrap the new FS type specification and fall back to the old parameter if necessary
func GetFsType(req *csi.NodePublishVolumeRequest) string {
	fsType := ""
	if fsType = req.GetVolumeCapability().GetMount().GetFsType(); fsType == "" {
		fsType = req.GetVolumeContext()[common.FsTypeConfigKey]
	}
	return fsType
}

// CheckFs: Perform a file system validation
func CheckFs(path string, fstype string, context string) error {

	if IsVolumeInUse(path) {
		klog.Infof("Volume already mounted, not performing FS check")
		return nil
	}

	fsRepairCommand := "e2fsck"
	if fstype == "xfs" {
		fsRepairCommand = "xfs_repair"
	}
	klog.Infof("Checking filesystem (%s -n %s) [%s]", fsRepairCommand, path, context)
	if out, err := exec.Command(fsRepairCommand, "-n", path).CombinedOutput(); err != nil {
		return errors.New(string(out))
	}
	return nil
}

// Check for and remove any rediscovered iscsi devices that were previously unmapped
// This is a common function for SAS and FC
func CheckPreviouslyRemovedDevices(ctx context.Context) error {
	klog.Info("Checking previously removed devices")
	for wwn := range SASandFCRemovedDevicesMap {
		klog.Infof("Checking for rediscovery of wwn:%s", wwn)

		dm, devices := saslib.FindDiskById(klog.FromContext(ctx), wwn, &saslib.OSioHandler{})
		if dm != "" {
			klog.Infof("Rediscovery found for wwn:%s -- mpath device: %s, devices: %v", wwn, dm, devices)
			saslib.Detach(ctx, dm, &saslib.OSioHandler{})
		}
	}
	return nil
}

// FindDeviceFormat:
func FindDeviceFormat(device string) (string, error) {
	klog.V(2).Infof("Trying to find filesystem format on device %q", device)

	ctx, cancel := context.WithTimeout(context.Background(), BlkidTimeout*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "blkid",
		"-p",
		"-s", "TYPE",
		"-s", "PTTYPE",
		"-o", "export",
		device).CombinedOutput()

	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("command timed out after %d seconds", BlkidTimeout)
	}

	klog.V(2).Infof("blkid output: %q, err=%v", output, err)

	if err != nil {
		// blkid exit with code 2 if the specified token (TYPE/PTTYPE, etc) could not be found or if device could not be identified.
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 2 {
			klog.V(2).Infof("Device seems to be is unformatted (%v)", err)
			return "", nil
		}
		return "", fmt.Errorf("could not not find format for device %q (%v)", device, err)
	}

	re := regexp.MustCompile(`([A-Z]+)="?([^"\n]+)"?`) // Handles alpine and debian outputs
	matches := re.FindAllSubmatch(output, -1)

	var filesystemType, partitionType string
	for _, match := range matches {
		if len(match) != 3 {
			return "", fmt.Errorf("invalid blkid output: %s", output)
		}
		key := string(match[1])
		value := string(match[2])

		if key == "TYPE" {
			filesystemType = value
		} else if key == "PTTYPE" {
			partitionType = value
		}
	}

	if partitionType != "" {
		klog.V(2).Infof("Device %q seems to have a partition table type: %s", partitionType)
		return "OTHER/PARTITIONS", nil
	}

	return filesystemType, nil
}

// EnsureFsType:
func EnsureFsType(fsType string, disk string) error {
	currentFsType, err := FindDeviceFormat(disk)
	if err != nil {
		return err
	}

	klog.V(1).Infof("Detected filesystem: %q", currentFsType)
	if currentFsType != fsType {
		if currentFsType != "" {
			return fmt.Errorf("Could not create %s filesystem on device %s since it already has one (%s)", fsType, disk, currentFsType)
		}

		klog.Infof("Creating %s filesystem on device %s", fsType, disk)
		out, err := exec.Command(fmt.Sprintf("mkfs.%s", fsType), disk).CombinedOutput()
		if err != nil {
			return errors.New(string(out))
		}
	}

	return nil
}

func MountFilesystem(req *csi.NodePublishVolumeRequest, path string) error {
	return mountFilesystemWithOperations(req, path, filesystemMountOperations{
		ensureFsType:    EnsureFsType,
		checkFs:         CheckFs,
		findMountpoints: findDeviceMountpoints,
		prepareTarget:   prepareFilesystemTarget,
		mountDevice:     mountFilesystemDevice,
		bindMount:       bindMountFilesystem,
		setReadOnly:     setBindMountReadOnly,
		unmount:         Unmount,
	})
}

type filesystemMountOperations struct {
	ensureFsType    func(string, string) error
	checkFs         func(string, string, string) error
	findMountpoints func(string) ([]string, error)
	prepareTarget   func(string) error
	mountDevice     func(string, string, string) error
	bindMount       func(string, string) error
	setReadOnly     func(string, bool) error
	unmount         func(string) error
}

func mountFilesystemWithOperations(req *csi.NodePublishVolumeRequest, devicePath string, operations filesystemMountOperations) error {
	targetPath := filepath.Clean(req.GetTargetPath())
	mountpoints, err := operations.findMountpoints(devicePath)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}

	for _, mountpoint := range mountpoints {
		if filepath.Clean(mountpoint) == targetPath {
			klog.InfoS("volume already mounted", "targetPath", targetPath)
			return nil
		}
	}

	if err := operations.prepareTarget(targetPath); err != nil {
		return status.Error(codes.Internal, err.Error())
	}

	if len(mountpoints) == 0 {
		fsType := GetFsType(req)
		if err := operations.ensureFsType(fsType, devicePath); err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		if err := operations.checkFs(devicePath, fsType, "Publish"); err != nil {
			return err
		}
		if err := operations.mountDevice(devicePath, targetPath, fsType); err != nil {
			return err
		}
		if req.GetReadonly() {
			if err := operations.setReadOnly(targetPath, true); err != nil {
				_ = operations.unmount(targetPath)
				return err
			}
		}
	} else {
		// The filesystem is already mounted for another pod on this node.
		// Bind-mount that filesystem into the new pod target instead of
		// mounting the block device a second time.
		sourcePath := filepath.Clean(mountpoints[0])
		if err := operations.bindMount(sourcePath, targetPath); err != nil {
			return err
		}
		if err := operations.setReadOnly(targetPath, req.GetReadonly()); err != nil {
			_ = operations.unmount(targetPath)
			return err
		}
	}

	klog.InfoS("successfully mounted volume", "targetPath", targetPath)
	return nil
}

func findDeviceMountpoints(devicePath string) ([]string, error) {
	output, err := exec.Command("findmnt", "--output", "TARGET", "--noheadings", "--raw", devicePath).Output()
	if err != nil {
		if _, notFound := err.(*exec.ExitError); notFound {
			return nil, nil
		}
		return nil, fmt.Errorf("find mounts for device %q: %w", devicePath, err)
	}

	var mountpoints []string
	for _, line := range strings.Split(string(output), "\n") {
		if mountpoint := strings.TrimSpace(line); mountpoint != "" {
			mountpoints = append(mountpoints, mountpoint)
		}
	}
	return mountpoints, nil
}

func prepareFilesystemTarget(targetPath string) error {
	if err := os.MkdirAll(targetPath, 0755); err != nil {
		return fmt.Errorf("create filesystem target %q: %w", targetPath, err)
	}
	return nil
}

func mountFilesystemDevice(devicePath, targetPath, fsType string) error {
	klog.V(1).InfoS("mount filesystem device", "devicePath", devicePath, "targetPath", targetPath, "fsType", fsType)
	output, err := exec.Command("mount", "-t", fsType, devicePath, targetPath).CombinedOutput()
	if err != nil {
		return status.Errorf(codes.Internal, "mount filesystem device: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

func bindMountFilesystem(sourcePath, targetPath string) error {
	klog.V(1).InfoS("bind mount filesystem", "sourcePath", sourcePath, "targetPath", targetPath)
	output, err := exec.Command("mount", "--bind", sourcePath, targetPath).CombinedOutput()
	if err != nil {
		return status.Errorf(codes.Internal, "bind mount filesystem: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

func setBindMountReadOnly(targetPath string, readOnly bool) error {
	accessMode := "rw"
	if readOnly {
		accessMode = "ro"
	}
	output, err := exec.Command("mount", "-o", "remount,bind,"+accessMode, targetPath).CombinedOutput()
	if err != nil {
		return status.Errorf(codes.Internal, "set bind mount %s: %s", accessMode, strings.TrimSpace(string(output)))
	}
	return nil
}

func MountDevice(req *csi.NodePublishVolumeRequest, path string) error {
	deviceFile, err := os.Create(req.GetTargetPath())
	if err != nil {
		klog.ErrorS(err, "could not create file", "TargetPath", req.GetTargetPath())
		return err
	}
	deviceFile.Chmod(00755)
	deviceFile.Close()
	out, err := exec.Command("mount", "-o", "bind", path, req.GetTargetPath()).CombinedOutput()
	if err != nil {
		return status.Error(codes.Internal, string(out))
	}
	return nil
}

// Unmount a given path, usually req.GetVolumePath() from NodeUnpublishVolume
// used for both block and filesystem mount types
func Unmount(path string) error {
	_, err := os.Stat(path)
	if err == nil {
		klog.InfoS("unmounting volume", "path", path)
		klog.V(4).InfoS("mountpoint command", "command", "mountpoint "+path)
		out, err := exec.Command("mountpoint", path).CombinedOutput()
		if err == nil {
			klog.V(4).InfoS("umount command", "command", "umount -l "+path)
			out, err := exec.Command("umount", "-l", path).CombinedOutput()
			if err != nil {
				return status.Error(codes.Internal, string(out))
			}
		} else {
			klog.ErrorS(err, "assuming that volume is already unmounted", "mountpoint_output", out)
		}

		err = os.Remove(path)
		if err != nil && !os.IsNotExist(err) {
			return status.Error(codes.Internal, err.Error())
		}
	} else {
		klog.ErrorS(err, "assuming that volume is already unmounted")
	}
	return nil
}

// IsKubeletBlockVolumeTarget reports whether path is a per-pod raw block
// publication target in kubelet's CSI volumeDevices tree.
func IsKubeletBlockVolumeTarget(path string) bool {
	cleanPath := filepath.Clean(path)
	marker := string(os.PathSeparator) + filepath.Join("plugins", "kubernetes.io", "csi", "volumeDevices", "publish") + string(os.PathSeparator)
	markerIndex := strings.LastIndex(cleanPath, marker)
	if markerIndex < 0 {
		return false
	}

	relativeTarget := cleanPath[markerIndex+len(marker):]
	parts := strings.Split(relativeTarget, string(os.PathSeparator))
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

// HasOtherBlockVolumePublications checks for another active raw block bind
// mount for the same volume. Kubelet stores every per-pod target for a volume
// as a sibling beneath the same volumeDevices/publish directory.
func HasOtherBlockVolumePublications(targetPath string) (bool, error) {
	return hasOtherBlockVolumePublications(targetPath, isActiveBlockTarget)
}

// HasOtherVolumePublications checks kubelet's publication tree for another
// active target of the same volume on this node. Unknown target layouts are
// left to the storage-specific in-use checks during detach.
func HasOtherVolumePublications(targetPath string) (bool, error) {
	if IsKubeletBlockVolumeTarget(targetPath) {
		return HasOtherBlockVolumePublications(targetPath)
	}

	podsRoot, volumeName, isFilesystemTarget := kubeletFilesystemTargetInfo(targetPath)
	if !isFilesystemTarget {
		return false, nil
	}
	return hasOtherFilesystemVolumePublications(targetPath, podsRoot, volumeName, isActiveMountTarget)
}

func hasOtherBlockVolumePublications(targetPath string, isActive func(string) (bool, error)) (bool, error) {
	cleanTarget := filepath.Clean(targetPath)
	publicationDir := filepath.Dir(cleanTarget)
	entries, err := os.ReadDir(publicationDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect raw block publications in %q: %w", publicationDir, err)
	}

	for _, entry := range entries {
		candidate := filepath.Join(publicationDir, entry.Name())
		if candidate == cleanTarget {
			continue
		}

		active, err := isActive(candidate)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, fmt.Errorf("inspect raw block publication %q: %w", candidate, err)
		}
		if active {
			return true, nil
		}
	}

	return false, nil
}

func isActiveBlockTarget(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}

	mode := info.Mode()
	return mode&os.ModeDevice != 0 && mode&os.ModeCharDevice == 0, nil
}

func kubeletFilesystemTargetInfo(targetPath string) (podsRoot, volumeName string, ok bool) {
	cleanPath := filepath.Clean(targetPath)
	marker := string(os.PathSeparator) + "pods" + string(os.PathSeparator)
	markerIndex := strings.LastIndex(cleanPath, marker)
	if markerIndex < 0 {
		return "", "", false
	}

	relativeTarget := cleanPath[markerIndex+len(marker):]
	parts := strings.Split(relativeTarget, string(os.PathSeparator))
	if len(parts) != 5 || parts[0] == "" || parts[1] != "volumes" || parts[2] != "kubernetes.io~csi" || parts[3] == "" || parts[4] != "mount" {
		return "", "", false
	}

	podsRoot = cleanPath[:markerIndex] + marker[:len(marker)-1]
	return podsRoot, parts[3], true
}

func hasOtherFilesystemVolumePublications(
	targetPath, podsRoot, volumeName string,
	isActive func(string) (bool, error),
) (bool, error) {
	cleanTarget := filepath.Clean(targetPath)
	pods, err := os.ReadDir(podsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect kubelet pod publications in %q: %w", podsRoot, err)
	}

	for _, pod := range pods {
		if !pod.IsDir() {
			continue
		}
		candidate := filepath.Join(podsRoot, pod.Name(), "volumes", "kubernetes.io~csi", volumeName, "mount")
		if candidate == cleanTarget {
			continue
		}

		active, err := isActive(candidate)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, fmt.Errorf("inspect filesystem publication %q: %w", candidate, err)
		}
		if active {
			return true, nil
		}
	}

	return false, nil
}

func isActiveMountTarget(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		return false, err
	}

	_, err := exec.Command("mountpoint", "--quiet", path).CombinedOutput()
	if err == nil {
		return true, nil
	}
	if _, notMounted := err.(*exec.ExitError); notMounted {
		return false, nil
	}
	return false, err
}

// IsMultipathDeviceOpen returns true when device-mapper reports one or more
// open references. Disconnecting an open map is unsafe because the iSCSI
// library force-removes it, which can replace the live map with an error table.
func IsMultipathDeviceOpen(devicePath string) (bool, error) {
	output, err := exec.Command("dmsetup", "info", "--columns", "--noheadings", "--options", "open", devicePath).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("query open count for multipath device %q: %s (%w)", devicePath, strings.TrimSpace(string(output)), err)
	}

	return parseMultipathOpenCount(output)
}

func parseMultipathOpenCount(output []byte) (bool, error) {
	fields := strings.Fields(string(output))
	if len(fields) != 1 {
		return false, fmt.Errorf("unexpected dmsetup open count output %q", strings.TrimSpace(string(output)))
	}

	openCount, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return false, fmt.Errorf("parse dmsetup open count %q: %w", fields[0], err)
	}
	return openCount > 0, nil
}

// IsVolumeInUse: Use findmnt to determine if the device path is mounted or not.
func IsVolumeInUse(devicePath string) bool {
	_, err := exec.Command("findmnt", devicePath).CombinedOutput()
	klog.Infof("isVolumeInUse: findmnt %s, err=%v", devicePath, err)
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return false
		}
	}
	return true
}

// DebugCorruption: Display additional information for debugging
func DebugCorruption(prefix, path string) {
	out, err := exec.Command("ls", "-l", path).CombinedOutput()
	klog.Infof("%s ls -l %s, err = %v, out = \n%s", prefix, path, err, string(out))

	out, err = exec.Command("multipath", "-ll", "-v2", path).CombinedOutput()
	klog.Infof("%s multipath -ll -v2 %s, err = %v, out = \n%s", prefix, path, err, string(out))

	out, err = exec.Command("ls", "-lR", "/dev/disk").CombinedOutput()
	klog.Infof("%s ls -lR /dev/disk, err = %v, out = \n%s", prefix, err, string(out))
}
