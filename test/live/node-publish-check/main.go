package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func main() {
	var endpoint, volumeID, targetPath, iqn, portals, lun, expectedCode string
	var unpublishAfterSuccess, unpublishOnly bool
	var timeout time.Duration
	flag.StringVar(&endpoint, "endpoint", "/csi/csi.sock", "node Unix socket")
	flag.StringVar(&volumeID, "volume-id", "", "augmented CSI volume ID")
	flag.StringVar(&targetPath, "target-path", "", "unused kubelet-visible target path")
	flag.StringVar(&iqn, "iqn", "", "iSCSI target IQN")
	flag.StringVar(&portals, "portals", "", "comma-separated iSCSI portals")
	flag.StringVar(&lun, "lun", "", "numeric LUN publish context")
	flag.StringVar(&expectedCode, "expected-code", "FailedPrecondition", "expected gRPC status code")
	flag.BoolVar(&unpublishAfterSuccess, "unpublish-after-success", false, "call NodeUnpublishVolume after an expected successful publish")
	flag.BoolVar(&unpublishOnly, "unpublish-only", false, "call only NodeUnpublishVolume (iqn, portals, and lun are not required)")
	flag.DurationVar(&timeout, "timeout", 90*time.Second, "overall publish and optional cleanup timeout")
	flag.Parse()

	if volumeID == "" || targetPath == "" {
		fatal(fmt.Errorf("--volume-id and --target-path are required"))
	}
	if !unpublishOnly && (iqn == "" || portals == "" || lun == "") {
		fatal(fmt.Errorf("--iqn, --portals, and --lun are required unless --unpublish-only is set"))
	}
	if unpublishOnly && unpublishAfterSuccess {
		fatal(fmt.Errorf("--unpublish-only and --unpublish-after-success are mutually exclusive"))
	}
	wantCode, ok := parseCode(expectedCode)
	if !ok {
		fatal(fmt.Errorf("unknown --expected-code %q", expectedCode))
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	connection, err := grpc.DialContext(ctx, "passthrough:///csi-node",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
		}),
		grpc.WithBlock(),
	)
	if err != nil {
		fatal(fmt.Errorf("connect to %s: %w", endpoint, err))
	}
	defer connection.Close()
	if unpublishOnly {
		response, err := csi.NewNodeClient(connection).NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
			VolumeId:   volumeID,
			TargetPath: targetPath,
		})
		gotCode := status.Code(err)
		if gotCode != wantCode {
			fatal(fmt.Errorf("NodeUnpublishVolume returned code %s, want %s; response=%v error=%v",
				gotCode, wantCode, response, err))
		}
		fmt.Printf("PASS: NodeUnpublishVolume returned expected code %s\n", gotCode)
		return
	}

	response, err := csi.NewNodeClient(connection).NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
		PublishContext: map[string]string{
			"lun": lun,
		},
		VolumeContext: map[string]string{
			"iqn":             iqn,
			"portals":         portals,
			"storageProtocol": "iscsi",
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	})
	gotCode := status.Code(err)
	if gotCode != wantCode {
		fatal(fmt.Errorf("NodePublishVolume returned code %s, want %s; response=%v error=%v",
			gotCode, wantCode, response, err))
	}
	if unpublishAfterSuccess && gotCode != codes.OK {
		fatal(fmt.Errorf("--unpublish-after-success requires --expected-code OK"))
	}
	if unpublishAfterSuccess {
		_, err = csi.NewNodeClient(connection).NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
			VolumeId:   volumeID,
			TargetPath: targetPath,
		})
		if err != nil {
			fatal(fmt.Errorf("NodePublishVolume succeeded but cleanup NodeUnpublishVolume failed: %w", err))
		}
		fmt.Println("PASS: NodePublishVolume returned OK and cleanup NodeUnpublishVolume returned OK")
		return
	}

	fmt.Printf("PASS: NodePublishVolume returned expected code %s\n", gotCode)
}

func parseCode(name string) (codes.Code, bool) {
	for code := codes.OK; code <= codes.Unauthenticated; code++ {
		if strings.EqualFold(code.String(), name) {
			return code, true
		}
	}
	return codes.Unknown, false
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "FAIL:", err)
	os.Exit(1)
}
