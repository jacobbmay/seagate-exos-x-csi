package controller

import (
	"context"
	"fmt"
	"math"
	"strings"

	storageapi "github.com/Seagate/seagate-exos-x-api-go/v2/pkg/api"
	storageapitypes "github.com/Seagate/seagate-exos-x-api-go/v2/pkg/common"

	"github.com/Seagate/seagate-exos-x-csi/pkg/common"
	"github.com/Seagate/seagate-exos-x-csi/pkg/storage"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

const me5AllocationUnitBytes int64 = 4 * 1024 * 1024

// Extract available SAS addresses for Nodes from topology segments
// This will contain all SAS initiators for all nodes unless the storage class
// has specified allowed or preferred topologies
func parseTopology(topologies []*csi.Topology, storageProtocol string, parameters *map[string]string) ([]*csi.Topology, error) {
	klog.V(5).Infof("parseTopology: %v", topologies)

	accessibleTopology := []*csi.Topology{}
	hasInitiators := false
	for _, topo := range topologies {

		segments := topo.GetSegments()

		nodeID := segments[common.TopologyNodeIDKey]
		hasInitiators = false
		for key, val := range segments {
			if strings.Contains(key, common.TopologySASInitiatorLabel) || strings.Contains(key, common.TopologyFCInitiatorLabel) {
				hasInitiators = true
				newKey := strings.TrimPrefix(key, common.TopologyInitiatorPrefix)
				// insert the node ID into the key so we can retrieve the node specific addresses after scheduling by the CO
				newKey = nodeID + newKey
				(*parameters)[newKey] = val
			}
		}
		if hasInitiators {
			accessibleTopology = append(accessibleTopology, topo)
		}

	}
	if len(accessibleTopology) == 0 {
		accessibleTopology = nil
	}
	return accessibleTopology, nil
}

// CreateVolume creates a new volume from the given request. The function is idempotent.
func (controller *Controller) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {

	parameters := req.GetParameters()

	volumeName, err := common.TranslateName(req.GetName(), parameters[common.VolumePrefixKey])
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "translate volume name contains invalid characters")
	}

	// Extract the storage interface protocol to be used for this volume (iscsi, fc, sas, etc)
	storageProtocol := storage.ValidateStorageProtocol(parameters[common.StorageProtocolKey])

	if !common.ValidateName(volumeName) {
		return nil, status.Error(codes.InvalidArgument, "volume name contains invalid characters")
	}

	volumeCapabilities := req.GetVolumeCapabilities()
	if err := isValidVolumeCapabilities(volumeCapabilities); err != nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("CreateVolume Volume capabilities not valid: %v", err))
	}

	requiredBytes := req.GetCapacityRange().GetRequiredBytes()
	limitBytes := req.GetCapacityRange().GetLimitBytes()
	size, err := normalizeCreateCapacity(requiredBytes, limitBytes)
	if err != nil {
		return nil, err
	}
	sizeStr := getSizeStr(size)
	pool := parameters[common.PoolConfigKey]
	wwn := ""

	klog.Infof("creating volume %q (size %s) pool %q using protocol (%s)", volumeName, sizeStr, pool, storageProtocol)

	volumeExists, err := controller.client.CheckVolumeExists(volumeName, size)
	if err != nil {
		return nil, err
	}

	if !volumeExists {
		var sourceId string

		if volume := req.VolumeContentSource.GetVolume(); volume != nil {
			sourceId = volume.VolumeId
			klog.Infof("-- GetVolume sourceID %q", sourceId)
		}

		if snapshot := req.VolumeContentSource.GetSnapshot(); sourceId == "" && snapshot != nil {
			sourceId = snapshot.SnapshotId
			klog.Infof("-- GetSnapshot sourceID %q", sourceId)
		}

		if sourceId != "" {
			sourceName, err := common.VolumeIdGetName(sourceId)
			if err != nil {
				return nil, err
			}
			apiStatus, err2 := controller.client.CopyVolume(sourceName, volumeName, parameters[common.PoolConfigKey])
			if err2 != nil {
				klog.Infof("-- CopyVolume apiStatus.ReturnCode %v", apiStatus.ReturnCode)
				if apiStatus != nil && apiStatus.ReturnCode == storageapitypes.SnapshotNotFoundErrorCode {
					return nil, status.Errorf(codes.NotFound, "Snapshot source (%s) not found", sourceId)
				} else {
					return nil, err2
				}
			}

		} else {
			volume, apiStatus, err2 := controller.client.CreateVolume(volumeName, sizeStr, parameters[common.PoolConfigKey])
			if err2 != nil {
				return nil, err2
			} else if apiStatus.ResponseTypeNumeric != 0 {
				return nil, status.Errorf(codes.Unknown, "Error creating volume: %s", apiStatus.Response)
			}
			if volume != nil {
				wwn = volume.Wwn
			}
		}
	}
	backendVolumes, backendStatus, err := controller.client.ShowVolumes(volumeName)
	if err != nil {
		klog.ErrorS(err, "Error retrieving new volume", "volumeName", volumeName)
		return nil, err
	}
	if backendStatus == nil || backendStatus.ResponseTypeNumeric != 0 {
		return nil, status.Errorf(codes.Unknown, "storage array did not return a successful status for volume %q", volumeName)
	}

	var backendVolume *storageapitypes.VolumeObject
	for i := range backendVolumes {
		if backendVolumes[i].VolumeName == volumeName {
			backendVolume = &backendVolumes[i]
			break
		}
	}
	if backendVolume == nil {
		return nil, status.Errorf(codes.NotFound, "created volume %q was not returned by the storage array", volumeName)
	}

	actualBytes, err := volumeCapacityBytes(backendVolume.Blocks, backendVolume.BlockSize)
	if err != nil {
		return nil, status.Errorf(codes.Unknown, "invalid capacity returned for volume %q: %v", volumeName, err)
	}
	if actualBytes < requiredBytes {
		return nil, status.Errorf(codes.OutOfRange,
			"storage array returned %d bytes for volume %q, below required_bytes %d",
			actualBytes, volumeName, requiredBytes)
	}
	if limitBytes > 0 && actualBytes > limitBytes {
		return nil, status.Errorf(codes.OutOfRange,
			"storage array returned %d bytes for volume %q, above limit_bytes %d",
			actualBytes, volumeName, limitBytes)
	}

	wwn = backendVolume.Wwn
	if wwn == "" {
		return nil, status.Errorf(codes.Unknown, "storage array returned an empty WWN for volume %q", volumeName)
	}

	if storageProtocol == common.StorageProtocolISCSI {
		// Fill iSCSI context parameters
		targetId, err1 := storageapi.GetTargetId(controller.client.Info, "iSCSI")
		if err1 != nil {
			klog.Errorf("++ GetTargetId error: %v", err1)
		}
		req.GetParameters()["iqn"] = targetId
		portals, err2 := controller.client.GetPortals()
		if err2 != nil {
			klog.Errorf("++ GetPortals error: %v", err2)
		}
		req.GetParameters()["portals"] = portals
		klog.V(2).Infof("Storing iSCSI iqn: %s, portals: %v", targetId, portals)
	}

	volumeId := common.VolumeIdAugment(volumeName, storageProtocol, wwn)

	volume := &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeId,
			VolumeContext: parameters,
			CapacityBytes: actualBytes,
			ContentSource: req.GetVolumeContentSource(),
		},
	}

	klog.Infof("created volume %s (requested=%dB provisioned=%dB)", volumeId, requiredBytes, actualBytes)

	// Log struct with field names
	klog.V(8).Infof("created volume %+v", volume)
	return volume, nil
}

func normalizeCreateCapacity(requiredBytes, limitBytes int64) (int64, error) {
	if requiredBytes < 0 {
		return 0, status.Error(codes.InvalidArgument, "required_bytes cannot be negative")
	}
	if limitBytes < 0 {
		return 0, status.Error(codes.InvalidArgument, "limit_bytes cannot be negative")
	}
	if limitBytes > 0 && requiredBytes > limitBytes {
		return 0, status.Error(codes.InvalidArgument, "required_bytes cannot exceed limit_bytes")
	}

	requestedBytes := requiredBytes
	if requestedBytes == 0 {
		requestedBytes = me5AllocationUnitBytes
	}
	if requestedBytes > math.MaxInt64-(me5AllocationUnitBytes-1) {
		return 0, status.Error(codes.OutOfRange, "required_bytes is too large to align")
	}

	alignedBytes := ((requestedBytes + me5AllocationUnitBytes - 1) / me5AllocationUnitBytes) * me5AllocationUnitBytes
	if limitBytes > 0 && alignedBytes > limitBytes {
		return 0, status.Errorf(codes.OutOfRange,
			"minimum aligned capacity %d exceeds limit_bytes %d", alignedBytes, limitBytes)
	}
	return alignedBytes, nil
}

func volumeCapacityBytes(blocks, blockSize int64) (int64, error) {
	if blocks <= 0 || blockSize <= 0 {
		return 0, fmt.Errorf("non-positive block geometry: blocks=%d block-size=%d", blocks, blockSize)
	}
	if blocks > math.MaxInt64/blockSize {
		return 0, fmt.Errorf("block geometry overflows int64: blocks=%d block-size=%d", blocks, blockSize)
	}
	return blocks * blockSize, nil
}

// DeleteVolume deletes the given volume. The function is idempotent.
func (controller *Controller) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cannot delete volume with empty ID")
	}
	volumeName, _ := common.VolumeIdGetName(req.GetVolumeId())
	klog.Infof("deleting volume %s", volumeName)

	respStatus, err := controller.client.DeleteVolume(volumeName)
	if err != nil {
		if respStatus != nil {
			if respStatus.ReturnCode == storageapitypes.VolumeNotFoundErrorCode {
				klog.Infof("volume %s does not exist, assuming it has already been deleted", volumeName)
				return &csi.DeleteVolumeResponse{}, nil
			} else if respStatus.ReturnCode == storageapitypes.VolumeHasSnapshot {
				return nil, status.Error(codes.FailedPrecondition, fmt.Sprintf("volume %s cannot be deleted since it has snapshots", volumeName))
			}
		}
		return nil, err
	}

	klog.Infof("successfully deleted volume %s", volumeName)
	return &csi.DeleteVolumeResponse{}, nil
}

func getSizeStr(size int64) string {
	if size == 0 {
		size = 4096
	}

	return fmt.Sprintf("%dB", size)
}

// isValidVolumeCapabilities validates the given VolumeCapability array is valid
func isValidVolumeCapabilities(volCaps []*csi.VolumeCapability) error {
	if len(volCaps) == 0 {
		return fmt.Errorf("volume capabilities to validate not provided")
	}

	hasSupport := func(cap *csi.VolumeCapability) bool {
		for _, supportedMode := range common.SupportedAccessModes {
			// we currently support block and mount volumes with both supported access modes, so don't check mount types
			if cap.GetAccessMode().Mode == supportedMode {
				return true
			}
		}
		return false
	}

	for _, c := range volCaps {
		if !hasSupport(c) {
			return fmt.Errorf("driver does not support access mode %v", c.GetAccessMode())
		}
	}
	return nil
}
