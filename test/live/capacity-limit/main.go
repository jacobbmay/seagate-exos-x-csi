package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const miB int64 = 1024 * 1024

type liveClient struct {
	client  csi.ControllerClient
	secrets map[string]string
	params  map[string]string
	prefix  string
}

func main() {
	var endpoint, pool, protocol, volumePrefix, namePrefix string
	flag.StringVar(&endpoint, "endpoint", "/csi/csi.sock", "controller Unix socket")
	flag.StringVar(&pool, "pool", "", "PowerVault pool")
	flag.StringVar(&protocol, "protocol", "iscsi", "storage protocol")
	flag.StringVar(&volumePrefix, "volume-prefix", "csi", "driver volume prefix")
	flag.StringVar(&namePrefix, "name-prefix", "exos-capacity-qualification", "unique test name prefix")
	flag.Parse()

	if pool == "" {
		fatal(errors.New("--pool is required"))
	}
	secrets, err := readSecrets(os.Stdin)
	if err != nil {
		fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	conn, err := grpc.DialContext(ctx, "passthrough:///csi-controller",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
		}),
		grpc.WithBlock(),
	)
	if err != nil {
		fatal(fmt.Errorf("connect to %s: %w", endpoint, err))
	}
	defer conn.Close()

	runID := time.Now().UTC().Format("20060102-150405")
	live := &liveClient{
		client:  csi.NewControllerClient(conn),
		secrets: secrets,
		params: map[string]string{
			"pool":            pool,
			"storageProtocol": protocol,
			"volPrefix":       volumePrefix,
		},
		prefix: namePrefix + "-" + runID,
	}

	matrix := []struct {
		label    string
		required int64
		want     int64
	}{
		{label: "1mi", required: miB, want: 4 * miB},
		{label: "4mi", required: 4 * miB, want: 4 * miB},
		{label: "10mi", required: 10 * miB, want: 12 * miB},
		{label: "12mi", required: 12 * miB, want: 12 * miB},
		{label: "1gi", required: 1024 * miB, want: 1024 * miB},
	}
	for _, tc := range matrix {
		if err := live.expectCreate(ctx, "matrix-"+tc.label, tc.required, 0, tc.want); err != nil {
			fatal(err)
		}
	}

	if err := live.expectReject(ctx, "limit-reject", 10*miB, 10*miB, codes.OutOfRange); err != nil {
		fatal(err)
	}
	if err := live.expectCreate(ctx, "limit-accept", 10*miB, 12*miB, 12*miB); err != nil {
		fatal(err)
	}

	fmt.Println("PASS: capacity matrix and limit_bytes qualification completed; all created volumes were deleted")
}

func readSecrets(reader io.Reader) (map[string]string, error) {
	secrets := map[string]string{}
	if err := json.NewDecoder(reader).Decode(&secrets); err != nil {
		return nil, fmt.Errorf("decode credentials JSON from stdin: %w", err)
	}
	for _, key := range []string{"username", "password", "apiAddress"} {
		if secrets[key] == "" {
			return nil, fmt.Errorf("credentials JSON is missing %q", key)
		}
	}
	return secrets, nil
}

func (l *liveClient) request(name string, required, limit int64) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name: l.prefix + "-" + name,
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: required,
			LimitBytes:    limit,
		},
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		}},
		Parameters: l.params,
		Secrets:    l.secrets,
	}
}

func (l *liveClient) expectCreate(ctx context.Context, label string, required, limit, want int64) error {
	rpcCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	response, err := l.client.CreateVolume(rpcCtx, l.request(label, required, limit))
	if err != nil {
		return fmt.Errorf("%s CreateVolume: %w", label, err)
	}
	if response.GetVolume() == nil || response.GetVolume().GetVolumeId() == "" {
		return fmt.Errorf("%s CreateVolume returned no volume ID", label)
	}
	volumeID := response.GetVolume().GetVolumeId()
	got := response.GetVolume().GetCapacityBytes()
	var validationErr error
	if got != want {
		validationErr = fmt.Errorf("%s capacity: got %d bytes, want %d", label, got, want)
	}

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 90*time.Second)
	_, cleanupErr := l.client.DeleteVolume(cleanupCtx, &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
		Secrets:  l.secrets,
	})
	cleanupCancel()
	if cleanupErr != nil {
		if validationErr != nil {
			return fmt.Errorf("%v; cleanup failed: %w", validationErr, cleanupErr)
		}
		return fmt.Errorf("%s cleanup failed: %w", label, cleanupErr)
	}
	if validationErr != nil {
		return validationErr
	}
	fmt.Printf("PASS: %-12s required=%d limit=%d actual=%d\n", label, required, limit, got)
	return nil
}

func (l *liveClient) expectReject(ctx context.Context, label string, required, limit int64, want codes.Code) error {
	rpcCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	response, err := l.client.CreateVolume(rpcCtx, l.request(label, required, limit))
	if err == nil {
		if response.GetVolume() != nil && response.GetVolume().GetVolumeId() != "" {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 90*time.Second)
			_, cleanupErr := l.client.DeleteVolume(cleanupCtx, &csi.DeleteVolumeRequest{
				VolumeId: response.GetVolume().GetVolumeId(),
				Secrets:  l.secrets,
			})
			cleanupCancel()
			if cleanupErr != nil {
				return fmt.Errorf("%s unexpectedly succeeded and cleanup failed: %w", label, cleanupErr)
			}
		}
		return fmt.Errorf("%s unexpectedly succeeded", label)
	}
	if got := status.Code(err); got != want {
		return fmt.Errorf("%s status: got %s, want %s: %w", label, got, want, err)
	}
	fmt.Printf("PASS: %-12s required=%d limit=%d status=%s\n", label, required, limit, want)
	return nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "FAIL:", err)
	os.Exit(1)
}
