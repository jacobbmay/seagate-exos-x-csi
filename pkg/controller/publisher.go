package controller

import (
	"context"
	"crypto/sha256"
	"fmt"

	storageapitypes "github.com/Seagate/seagate-exos-x-api-go/v2/pkg/common"

	"github.com/Seagate/seagate-exos-x-csi/pkg/common"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

type initiatorRegistrationClient interface {
	CreateNickname(name, iqn string) (*storageapitypes.ResponseStatus, error)
	GetInitiatorHostGroup(initiator string) (hostGroup string, host string, err error)
}

// ControllerPublishVolume attaches the given volume to the node
func (driver *Controller) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cannot publish volume with empty ID")
	}
	if len(req.GetNodeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cannot publish volume to a node with empty ID")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "cannot publish volume without capabilities")
	}

	nodeIP := req.GetNodeId()
	parameters := req.GetVolumeContext()

	initiators, err := driver.GetNodeInitiators(ctx, nodeIP, parameters[common.StorageProtocolKey])
	if err != nil {
		klog.ErrorS(err, "error getting node initiators", "node-ip", nodeIP, "storage-protocol", parameters[common.StorageProtocolKey])
		return nil, status.Error(codes.NotFound, fmt.Sprintf("Could not retrieve initiators for scheduled node(%s)", nodeIP))
	}

	volumeName, _ := common.VolumeIdGetName(req.GetVolumeId())

	klog.InfoS("attach request", "initiator(s)", initiators, "volume", volumeName)
	if parameters[common.StorageProtocolKey] == common.StorageProtocolISCSI {
		registeredInitiators := map[string]bool{}
		if driver.client.Info != nil {
			for initiator := range driver.client.Info.InitiatorMap {
				registeredInitiators[initiator] = true
			}
		}
		if err := ensureInitiatorsRegistered(driver.client, registeredInitiators, initiators); err != nil {
			return nil, err
		}
	}

	lun, err := driver.client.PublishVolume(volumeName, initiators)

	if err != nil {
		return nil, err
	}

	return &csi.ControllerPublishVolumeResponse{
		PublishContext: map[string]string{"lun": lun},
	}, err
}

// ensureInitiatorsRegistered creates deterministic nicknames for iSCSI
// initiators that the array has not seen yet. PowerVault arrays do not allow a
// volume to be mapped to an initiator until it is either connected or present
// in the initiator nickname table. ControllerPublishVolume runs before the
// node-side iSCSI discovery, so registration must happen before mapping.
func ensureInitiatorsRegistered(client initiatorRegistrationClient, registered map[string]bool, initiators []string) error {
	for _, initiator := range initiators {
		if initiator == "" {
			return status.Error(codes.InvalidArgument, "cannot register an empty iSCSI initiator")
		}
		if registered[initiator] {
			continue
		}

		nickname := initiatorNickname(initiator)
		apiStatus, createErr := client.CreateNickname(nickname, initiator)

		// Verify through a fresh array query instead of trusting cached system
		// information. This also makes the operation idempotent if another
		// controller registered the initiator concurrently or the create response
		// was lost after the array applied it.
		_, _, verifyErr := client.GetInitiatorHostGroup(initiator)
		if verifyErr != nil {
			if createErr != nil {
				return status.Errorf(codes.Internal, "failed to register iSCSI initiator %q: %v", initiator, createErr)
			}
			if apiStatus != nil && apiStatus.ResponseTypeNumeric != 0 {
				return status.Errorf(codes.Internal, "failed to register iSCSI initiator %q: %s", initiator, apiStatus.Response)
			}
			return status.Errorf(codes.Internal, "iSCSI initiator %q was not found after registration: %v", initiator, verifyErr)
		}

		registered[initiator] = true
		klog.InfoS("registered iSCSI initiator", "initiator", initiator, "nickname", nickname)
	}

	return nil
}

func initiatorNickname(initiator string) string {
	digest := sha256.Sum256([]byte(initiator))
	return fmt.Sprintf("csi-%x", digest[:8])
}

// ControllerUnpublishVolume detaches the given volume from the node
func (driver *Controller) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cannot unpublish volume with empty ID")
	}

	volumeName, _ := common.VolumeIdGetName(req.GetVolumeId())
	nodeIP := req.GetNodeId()
	storageProtocol, err := common.VolumeIdGetStorageProtocol(req.GetVolumeId())
	if err != nil {
		klog.ErrorS(err, "No storage protocol found in ControllerUnpublishVolume", "storage protocol", storageProtocol, "volume ID:", req.GetVolumeId())
		return nil, err
	}

	initiators, err := driver.GetNodeInitiators(ctx, nodeIP, storageProtocol)
	initiators, err = requireUnpublishInitiators(nodeIP, storageProtocol, initiators, err)
	if err != nil {
		klog.ErrorS(err, "refusing to report controller unpublish success without node initiators",
			"nodeIP", nodeIP, "storageProtocol", storageProtocol)
		return nil, err
	}

	klog.InfoS("unmapping volume from initiator", "volumeName", volumeName, "initiators", initiators)
	for _, initiator := range initiators {
		apiStatus, unmapErr := driver.client.UnmapVolume(volumeName, initiator)
		alreadyUnmapped, resultErr := validateControllerUnmapResult(volumeName, initiator, apiStatus, unmapErr)
		if resultErr != nil {
			return nil, resultErr
		}
		if alreadyUnmapped {
			klog.InfoS("array reports volume is already unmapped", "volumeName", volumeName, "initiator", initiator)
		}
	}

	// Array-side unmap can race late iSCSI discovery while a node is booting.
	// Do not let the external-attacher remove its finalizer until the node has
	// reconciled the exact volume identity and observed it stably absent. Pass
	// the augmented ID so the node can select the protocol safely.
	if err := driver.NotifyUnmap(ctx, nodeIP, req.GetVolumeId()); err != nil {
		return nil, status.Errorf(codes.Aborted,
			"array unmapped volume %s, but node %s has not completed post-unmap reconciliation: %v",
			volumeName, nodeIP, err)
	}

	klog.Infof("successfully unmapped volume %s from all initiators", volumeName)
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func validateControllerUnmapResult(
	volumeName string,
	initiator string,
	apiStatus *storageapitypes.ResponseStatus,
	unmapErr error,
) (alreadyUnmapped bool, err error) {
	if unmapErr == nil {
		return false, nil
	}
	if apiStatus != nil && apiStatus.ReturnCode == storageapitypes.UnmapFailedErrorCode {
		return true, nil
	}
	return false, status.Errorf(codes.Internal,
		"failed to unmap volume %s from initiator %s: %v", volumeName, initiator, unmapErr)
}

// requireUnpublishInitiators prevents a node outage from turning detach into
// false success. Without an initiator, the controller cannot issue any array
// unmap request; returning success lets the external attacher remove the
// VolumeAttachment even though the host mapping remains live.
func requireUnpublishInitiators(nodeIP, storageProtocol string, initiators []string, lookupErr error) ([]string, error) {
	if lookupErr != nil {
		return nil, status.Errorf(codes.Unavailable,
			"cannot retrieve %s initiators from node %s for controller unpublish: %v",
			storageProtocol, nodeIP, lookupErr)
	}
	if len(initiators) == 0 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"node %s returned no %s initiators for controller unpublish",
			nodeIP, storageProtocol)
	}
	for _, initiator := range initiators {
		if initiator == "" {
			return nil, status.Errorf(codes.FailedPrecondition,
				"node %s returned an empty %s initiator for controller unpublish",
				nodeIP, storageProtocol)
		}
	}
	return initiators, nil
}
