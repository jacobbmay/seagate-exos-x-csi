//
// Copyright (c) 2026 Seagate Technology LLC and/or its Affiliates
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
//

package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iscsilib "github.com/Seagate/csi-lib-iscsi/iscsi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCanonicalMultipathWWID(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "array WWN", input: "600c0ff000f8b0a13eb1be6a01000000", want: "3600c0ff000f8b0a13eb1be6a01000000"},
		{name: "already canonical", input: "3600c0ff000f8b0a13eb1be6a01000000", want: "3600c0ff000f8b0a13eb1be6a01000000"},
		{name: "trim whitespace", input: " 600c0ff000f8b0a13eb1be6a01000000\n", want: "3600c0ff000f8b0a13eb1be6a01000000"},
		{name: "empty", input: "", wantErr: true},
		{name: "path injection", input: "../../dm-0", wantErr: true},
		{name: "whitespace", input: "600c0ff0 bad", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := canonicalMultipathWWID(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("canonicalMultipathWWID(%q) unexpectedly succeeded with %q", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("canonicalMultipathWWID(%q): %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("canonicalMultipathWWID(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestValidatePersistedSinglePathMissingDevice(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-device")
	alreadyGone, err := validatePersistedSinglePath(missing, "3600c0ff000f8b0a13eb1be6a01000000")
	if err != nil {
		t.Fatal(err)
	}
	if !alreadyGone {
		t.Fatal("missing single-path device was not reported as already gone")
	}
}

func TestValidatePersistedSinglePathRejectsEmptyDevice(t *testing.T) {
	alreadyGone, err := validatePersistedSinglePath("", "3600c0ff000f8b0a13eb1be6a01000000")
	if err == nil {
		t.Fatal("empty single-path device unexpectedly passed validation")
	}
	if alreadyGone {
		t.Fatal("empty device path was incorrectly reported as an idempotent missing device")
	}
}

func TestIsRequestedISCSIByPath(t *testing.T) {
	const iqn = "iqn.1988-11.com.dell:01.array.example"
	tests := []struct {
		name string
		path string
		lun  int
		want bool
	}{
		{name: "matching IPv4 portal", path: "ip-10.10.71.2:3260-iscsi-" + iqn + "-lun-13", lun: 13, want: true},
		{name: "matching IPv6-style portal", path: "ip-[fd00::2]:3260-iscsi-" + iqn + "-lun-13", lun: 13, want: true},
		{name: "wrong LUN", path: "ip-10.10.71.2:3260-iscsi-" + iqn + "-lun-12", lun: 13},
		{name: "wrong target", path: "ip-10.10.71.2:3260-iscsi-iqn.other-lun-13", lun: 13},
		{name: "not by path", path: "pci-0000:00:00.0-iscsi-" + iqn + "-lun-13", lun: 13},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRequestedISCSIByPath(tt.path, iqn, tt.lun); got != tt.want {
				t.Fatalf("isRequestedISCSIByPath(%q, %q, %d) = %v, want %v",
					tt.path, iqn, tt.lun, got, tt.want)
			}
		})
	}
}

func TestValidateDetachPathWWID(t *testing.T) {
	const expected = "3600c0ff000f8b0a13eb1be6a01000000"
	tests := []struct {
		name    string
		actual  string
		wantErr bool
	}{
		{name: "expected identity", actual: expected},
		{name: "NAA all-zero sentinel", actual: "360000000000000000000000000000000"},
		{name: "type-only all-zero sentinel", actual: "300000000000000000000000000000000"},
		{name: "plain all-zero sentinel", actual: "000000000000000000000000000000000"},
		{name: "different real WWID", actual: "3600c0ff000f8b0a13eb1be6a02000000", wantErr: true},
		{name: "empty identity", actual: "", wantErr: true},
		{name: "short nonzero identity", actual: "36", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDetachPathWWID("/dev/sdz", tt.actual, expected)
			if tt.wantErr && err == nil {
				t.Fatalf("validateDetachPathWWID(%q) unexpectedly succeeded", tt.actual)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validateDetachPathWWID(%q): %v", tt.actual, err)
			}
		})
	}
}

func TestWaitForMissingISCSIConnectorStateCatchesLateMap(t *testing.T) {
	inspections := 0
	mapDevice, inaccessible, clean, err := waitForMissingISCSIConnectorState(
		context.Background(),
		"600c0ff000f8b0a13eb1be6a01000000",
		"3600c0ff000f8b0a13eb1be6a01000000",
		3,
		0,
		func(string) (string, int, error) {
			inspections++
			if inspections < 3 {
				return "", 0, os.ErrNotExist
			}
			return "/dev/dm-7", 8, nil
		},
		func(string) ([]string, error) {
			t.Fatal("path scan ran even though a late map appeared")
			return nil, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if clean || mapDevice != "/dev/dm-7" || inaccessible != 8 {
		t.Fatalf("got map=%q inaccessible=%d clean=%v", mapDevice, inaccessible, clean)
	}
}

func TestWaitForMissingISCSIConnectorStateRequiresStableAbsence(t *testing.T) {
	inspections := 0
	pathScans := 0
	_, _, clean, err := waitForMissingISCSIConnectorState(
		context.Background(), "volume", "expected", 4, 0,
		func(string) (string, int, error) {
			inspections++
			return "", 0, os.ErrNotExist
		},
		func(string) ([]string, error) {
			pathScans++
			return nil, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !clean || inspections != 4 || pathScans != 1 {
		t.Fatalf("got clean=%v inspections=%d pathScans=%d", clean, inspections, pathScans)
	}
}

func TestWaitForMissingISCSIConnectorStateRejectsResidualPaths(t *testing.T) {
	_, _, clean, err := waitForMissingISCSIConnectorState(
		context.Background(), "volume", "expected", 1, 0,
		func(string) (string, int, error) { return "", 0, os.ErrNotExist },
		func(string) ([]string, error) { return []string{"/dev/sdz"}, nil },
	)
	if err == nil || clean {
		t.Fatalf("got clean=%v err=%v, want unsafe residual-path error", clean, err)
	}
}

func TestWaitForMissingISCSIConnectorStateHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, clean, err := waitForMissingISCSIConnectorState(
		ctx, "volume", "expected", 2, time.Hour,
		func(string) (string, int, error) { return "", 0, os.ErrNotExist },
		func(string) ([]string, error) { return nil, nil },
	)
	if !errors.Is(err, context.Canceled) || clean {
		t.Fatalf("got clean=%v err=%v, want context cancellation", clean, err)
	}
}

func TestReconcileControllerUnmappedISCSIRequiresStableAbsence(t *testing.T) {
	inspections := 0
	err := reconcileControllerUnmappedISCSI(
		context.Background(),
		"600c0ff000f8b0a13eb1be6a01000000",
		4,
		0,
		func(string) (string, int, error) {
			inspections++
			return "", 0, os.ErrNotExist
		},
		func(string) bool { return false },
		func(string) (bool, error) { return false, nil },
		func(iscsilib.Connector) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if inspections != 4 {
		t.Fatalf("inspections = %d, want 4", inspections)
	}
}

func TestReconcileControllerUnmappedISCSIRemovesLateExactMapAndForcesRetry(t *testing.T) {
	inspections := 0
	var disconnected iscsilib.Connector
	err := reconcileControllerUnmappedISCSI(
		context.Background(),
		"600c0ff000f8b0a13eb1be6a01000000",
		4,
		0,
		func(string) (string, int, error) {
			inspections++
			if inspections == 3 {
				return "/dev/dm-7", 8, nil
			}
			return "", 0, os.ErrNotExist
		},
		func(string) bool { return false },
		func(string) (bool, error) { return false, nil },
		func(connector iscsilib.Connector) error {
			disconnected = connector
			return nil
		},
	)
	if status.Code(err) != codes.Aborted {
		t.Fatalf("status code = %s, want Aborted (error %v)", status.Code(err), err)
	}
	if disconnected.DevicePath != "/dev/dm-7" || !disconnected.Multipath {
		t.Fatalf("disconnected connector = %#v", disconnected)
	}
}

func TestReconcileControllerUnmappedISCSIRefusesOpenMap(t *testing.T) {
	disconnectCalled := false
	err := reconcileControllerUnmappedISCSI(
		context.Background(),
		"600c0ff000f8b0a13eb1be6a01000000",
		2,
		0,
		func(string) (string, int, error) { return "/dev/dm-7", 0, nil },
		func(string) bool { return false },
		func(string) (bool, error) { return true, nil },
		func(iscsilib.Connector) error {
			disconnectCalled = true
			return nil
		},
	)
	if status.Code(err) != codes.Aborted || disconnectCalled {
		t.Fatalf("got code=%s disconnectCalled=%v error=%v", status.Code(err), disconnectCalled, err)
	}
}

func TestRollbackFailedISCSIAttachRemovesUnusedExactMap(t *testing.T) {
	var disconnected iscsilib.Connector
	err := rollbackFailedISCSIAttach(
		"600c0ff000f8b0a13eb1be6a01000000",
		func(string) (string, int, error) { return "/dev/dm-7", 3, nil },
		func(string) bool { return false },
		func(string) (bool, error) { return false, nil },
		func(connector iscsilib.Connector) error {
			disconnected = connector
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if disconnected.DevicePath != "/dev/dm-7" || !disconnected.Multipath {
		t.Fatalf("disconnected connector = %#v", disconnected)
	}
}

func TestRollbackFailedISCSIAttachTreatsAbsentMapAsClean(t *testing.T) {
	err := rollbackFailedISCSIAttach(
		"600c0ff000f8b0a13eb1be6a01000000",
		func(string) (string, int, error) { return "", 0, os.ErrNotExist },
		func(string) bool { return false },
		func(string) (bool, error) { return false, nil },
		func(iscsilib.Connector) error { return errors.New("must not disconnect") },
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRollbackFailedISCSIAttachRefusesMountedOrOpenMap(t *testing.T) {
	tests := []struct {
		name       string
		mounted    bool
		open       bool
		openErr    error
		wantSubstr string
	}{
		{name: "mounted", mounted: true, wantSubstr: "mounted"},
		{name: "open", open: true, wantSubstr: "open"},
		{name: "open inspection failure", openErr: errors.New("dmsetup failed"), wantSubstr: "open state"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			disconnectCalled := false
			err := rollbackFailedISCSIAttach(
				"600c0ff000f8b0a13eb1be6a01000000",
				func(string) (string, int, error) { return "/dev/dm-7", 0, nil },
				func(string) bool { return tt.mounted },
				func(string) (bool, error) { return tt.open, tt.openErr },
				func(iscsilib.Connector) error {
					disconnectCalled = true
					return nil
				},
			)
			if err == nil || !strings.Contains(err.Error(), tt.wantSubstr) || disconnectCalled {
				t.Fatalf("got error=%v disconnectCalled=%v", err, disconnectCalled)
			}
		})
	}
}
