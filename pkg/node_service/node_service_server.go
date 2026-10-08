package node_service

import (
	"context"
	"fmt"
	"net"

	"github.com/Seagate/seagate-exos-x-csi/pkg/common"
	pb "github.com/Seagate/seagate-exos-x-csi/pkg/node_service/node_servicepb"
	"github.com/Seagate/seagate-exos-x-csi/pkg/storage"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
)

type server struct {
	pb.UnimplementedNodeServiceServer
}

// Retrieve initiator addresses from the node
func (s *server) GetInitiators(ctx context.Context, in *pb.InitiatorRequest) (*pb.Initiators, error) {
	initiators := []string{}
	var err error
	switch in.GetType() {
	case pb.InitiatorType_FC:
		initiators, err = storage.GetFCInitiators()
	case pb.InitiatorType_SAS:
		initiators, err = storage.GetSASInitiators()
	case pb.InitiatorType_ISCSI:
		initiators, err = storage.GetISCSIInitiators()
	case pb.InitiatorType_UNSPECIFIED:
		klog.InfoS("Unspecified initiator type in initiator request, defaulting to iSCSI")
		initiators, err = storage.GetISCSIInitiators()
	}
	if err != nil {
		return nil, err
	}
	return &pb.Initiators{Initiators: initiators}, nil
}

// Notify node that a volume has been unmapped from the controller
func (s *server) NotifyUnmap(ctx context.Context, in *pb.UnmappedVolume) (*pb.Ack, error) {
	volumeID := in.GetVolumeName()
	protocol, protocolErr := common.VolumeIdGetStorageProtocol(volumeID)
	if protocolErr == nil {
		volumeName, nameErr := common.VolumeIdGetName(volumeID)
		volumeWWN, wwnErr := common.VolumeIdGetWwn(volumeID)
		if nameErr != nil || wwnErr != nil {
			return nil, commonVolumeIDError(volumeID, nameErr, wwnErr)
		}

		storage.AddGatekeeper(volumeName)
		defer storage.RemoveGatekeeper(volumeName)

		if protocol == common.StorageProtocolISCSI {
			if err := storage.ReconcileControllerUnmappedISCSI(ctx, volumeWWN); err != nil {
				return nil, err
			}
			klog.InfoS("completed post-controller-unmap iSCSI reconciliation",
				"volumeName", volumeName, "wwn", volumeWWN)
			return &pb.Ack{Ack: 1}, nil
		}

		storage.CheckPreviouslyRemovedDevices(ctx)
		delete(storage.SASandFCRemovedDevicesMap, volumeWWN)
		klog.V(4).InfoS("Previously unmapped device - ControllerUnpublishComplete Notification",
			"deviceMap", storage.SASandFCRemovedDevicesMap, "volumeName", volumeWWN)
		return &pb.Ack{Ack: 1}, nil
	}

	// Backward compatibility for an old controller that sends only the WWN.
	storage.CheckPreviouslyRemovedDevices(ctx)
	delete(storage.SASandFCRemovedDevicesMap, volumeID)
	klog.V(4).InfoS("Previously unmapped device - legacy ControllerUnpublishComplete Notification", "deviceMap", storage.SASandFCRemovedDevicesMap, "volumeName", volumeID)
	return &pb.Ack{Ack: 1}, nil
}

func commonVolumeIDError(volumeID string, nameErr, wwnErr error) error {
	return fmt.Errorf("invalid augmented volume ID %q in unmap notification: name=%v wwn=%v", volumeID, nameErr, wwnErr)
}

func ListenAndServe(s *grpc.Server, port string) {
	lis, err := net.Listen("tcp", ":"+port)
	if err != nil {
		klog.ErrorS(err, "Node Service gRPC server failed to listen")
	}
	pb.RegisterNodeServiceServer(s, &server{})
	klog.V(0).InfoS("Node Service gRPC server listening", "address", lis.Addr())
	s.Serve(lis)
}
