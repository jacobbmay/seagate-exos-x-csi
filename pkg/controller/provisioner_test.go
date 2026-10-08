package controller

import (
	"math"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNormalizeCreateCapacity(t *testing.T) {
	const (
		miB = int64(1024 * 1024)
		giB = int64(1024 * 1024 * 1024)
	)

	tests := []struct {
		name     string
		required int64
		limit    int64
		want     int64
		wantCode codes.Code
	}{
		{name: "unspecified", want: 4 * miB},
		{name: "one MiB", required: miB, want: 4 * miB},
		{name: "four MiB", required: 4 * miB, want: 4 * miB},
		{name: "ten MiB", required: 10 * miB, want: 12 * miB},
		{name: "twelve MiB", required: 12 * miB, want: 12 * miB},
		{name: "one GiB", required: giB, want: giB},
		{name: "aligned within limit", required: 10 * miB, limit: 12 * miB, want: 12 * miB},
		{name: "alignment exceeds limit", required: 10 * miB, limit: 10 * miB, wantCode: codes.OutOfRange},
		{name: "required exceeds limit", required: 12 * miB, limit: 10 * miB, wantCode: codes.InvalidArgument},
		{name: "negative required", required: -1, wantCode: codes.InvalidArgument},
		{name: "negative limit", limit: -1, wantCode: codes.InvalidArgument},
		{name: "alignment overflow", required: math.MaxInt64, wantCode: codes.OutOfRange},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeCreateCapacity(tt.required, tt.limit)
			if tt.wantCode != codes.OK {
				if status.Code(err) != tt.wantCode {
					t.Fatalf("expected %s, got capacity=%d error=%v", tt.wantCode, got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("expected %d bytes, got %d", tt.want, got)
			}
		})
	}
}

func TestVolumeCapacityBytes(t *testing.T) {
	tests := []struct {
		name      string
		blocks    int64
		blockSize int64
		want      int64
		wantError bool
	}{
		{name: "four MiB", blocks: 8192, blockSize: 512, want: 4 * 1024 * 1024},
		{name: "one GiB", blocks: 2097152, blockSize: 512, want: 1024 * 1024 * 1024},
		{name: "zero blocks", blockSize: 512, wantError: true},
		{name: "zero block size", blocks: 1, wantError: true},
		{name: "overflow", blocks: math.MaxInt64, blockSize: 2, wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := volumeCapacityBytes(tt.blocks, tt.blockSize)
			if tt.wantError {
				if err == nil {
					t.Fatalf("expected an error, got %d", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("expected %d bytes, got %d", tt.want, got)
			}
		})
	}
}
