//
// Copyright (c) 2026 Seagate Technology LLC and/or its Affiliates
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//

package storage

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	iscsilib "github.com/Seagate/csi-lib-iscsi/iscsi"
)

const iscsiInspectionTimeout = 10 * time.Second

const iscsiMapPreparationTimeout = 10 * time.Second

type iscsiMapInspection struct {
	Device      string
	WWID        string
	Capacity    uint64
	SlaveNames  []string
	TargetBytes uint64
}

// prepareISCSIMultipathMap handles the common case where sessions to the array
// already exist. It discovers the requested numeric LUN, verifies every
// discovered path against the volume's expected WWID and direct capacity, and
// asks multipath to construct the exact-WWID map before csi-lib-iscsi enters
// its per-path multipath wait loop. If no matching iSCSI sessions or LUN paths
// exist yet, Connect is allowed to perform the initial login and discovery.
func prepareISCSIMultipathMap(targetIQN string, lun int, volumeWWN string) error {
	expectedWWID, err := canonicalMultipathWWID(volumeWWN)
	if err != nil {
		return err
	}

	if mapDevice, findErr := findMultipathMapByWWID(expectedWWID); findErr == nil {
		slaves, readErr := os.ReadDir(filepath.Join("/sys/class/block", filepath.Base(mapDevice), "slaves"))
		if readErr != nil {
			return fmt.Errorf("list slaves for %s: %w", mapDevice, readErr)
		}
		if len(slaves) > 0 {
			_, inspectErr := inspectISCSIMultipathMap(volumeWWN)
			return inspectErr
		}
	} else if !os.IsNotExist(findErr) {
		return findErr
	}

	// A rescan can fail when the node has no session to this target yet. If it
	// also has no requested-LUN links, return immediately and let Connect perform
	// the initial login instead of adding a preparation timeout to a clean node.
	if rescanErr := iscsilib.ISCSIRescan(targetIQN, lun); rescanErr != nil {
		paths, pathErr := requestedISCSILUNPaths(targetIQN, lun)
		if pathErr != nil {
			return pathErr
		}
		if len(paths) == 0 {
			return nil
		}
	}

	deadline := time.Now().Add(iscsiMapPreparationTimeout)
	var paths []string
	for {
		paths, err = requestedISCSILUNPaths(targetIQN, lun)
		if err != nil {
			return err
		}
		if len(paths) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if len(paths) == 0 {
		return nil
	}

	var targetBytes uint64
	for _, path := range paths {
		pathWWID, readErr := readSCSIWWID(path)
		if readErr != nil {
			return readErr
		}
		if pathWWID != expectedWWID {
			return fmt.Errorf("requested LUN %d path %s reports WWID %s, expected %s",
				lun, path, pathWWID, expectedWWID)
		}
		directBytes, readErr := readSCSITargetBytes(path)
		if readErr != nil {
			return readErr
		}
		cachedBytes, readErr := readBlockDeviceBytes(path)
		if readErr != nil {
			return readErr
		}
		if cachedBytes != directBytes {
			return fmt.Errorf("requested LUN %d path %s cached capacity %d does not match target capacity %d",
				lun, path, cachedBytes, directBytes)
		}
		if targetBytes == 0 {
			targetBytes = directBytes
		} else if targetBytes != directBytes {
			return fmt.Errorf("requested LUN %d path %s target capacity %d disagrees with %d",
				lun, path, directBytes, targetBytes)
		}
	}

	if output, commandErr := runInspectionCommand("multipath", "-v2", paths[0]); commandErr != nil {
		return fmt.Errorf("create exact map for WWID %s from %s: %s: %w",
			expectedWWID, paths[0], strings.TrimSpace(string(output)), commandErr)
	}

	deadline = time.Now().Add(iscsiMapPreparationTimeout)
	var lastErr error
	for {
		_, lastErr = inspectISCSIMultipathMap(volumeWWN)
		if lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("validate prepared multipath map for WWID %s: %w", expectedWWID, lastErr)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func requestedISCSILUNPaths(targetIQN string, lun int) ([]string, error) {
	entries, err := os.ReadDir("/dev/disk/by-path")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list iSCSI by-path links: %w", err)
	}

	var paths []string
	for _, entry := range entries {
		if !isRequestedISCSIByPath(entry.Name(), targetIQN, lun) {
			continue
		}
		link := filepath.Join("/dev/disk/by-path", entry.Name())
		resolved, resolveErr := filepath.EvalSymlinks(link)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve iSCSI path %s: %w", link, resolveErr)
		}
		if !strings.HasPrefix(filepath.Base(resolved), "sd") {
			return nil, fmt.Errorf("iSCSI path %s resolved to unexpected device %s", link, resolved)
		}
		paths = append(paths, resolved)
	}
	return paths, nil
}

func isRequestedISCSIByPath(name, targetIQN string, lun int) bool {
	return strings.HasPrefix(name, "ip-") &&
		strings.HasSuffix(name, "-iscsi-"+targetIQN+"-lun-"+strconv.Itoa(lun))
}

func inspectISCSIMultipathIdentity(volumeWWN string) (string, error) {
	expectedWWID, err := canonicalMultipathWWID(volumeWWN)
	if err != nil {
		return "", err
	}
	mapDevice, err := findMultipathMapByWWID(expectedWWID)
	if err != nil {
		return "", err
	}
	slaves, err := os.ReadDir(filepath.Join("/sys/class/block", filepath.Base(mapDevice), "slaves"))
	if err != nil {
		return "", fmt.Errorf("list slaves for %s: %w", mapDevice, err)
	}
	if len(slaves) == 0 {
		// An exact-WWID pathless map can still be safely selected for detach.
		// The caller must independently verify that it has no open references.
		return mapDevice, nil
	}
	for _, slave := range slaves {
		pathDevice := filepath.Join("/dev", slave.Name())
		pathWWID, readErr := readSCSIWWID(pathDevice)
		if readErr != nil {
			return "", readErr
		}
		if pathWWID != expectedWWID {
			return "", fmt.Errorf("path %s reports WWID %s, expected %s", pathDevice, pathWWID, expectedWWID)
		}
	}
	return mapDevice, nil
}

// inspectISCSIMultipathDetachIdentity selects a map by its exact device-mapper
// WWID and validates every remaining slave before detach. A path may report the
// narrowly defined all-zero VPD sentinel after its array-side LUN mapping has
// already been removed. That state is safe to accept only for detach: the map
// UUID still proves which volume is being removed, and DetachStorage separately
// verifies that the map is neither mounted nor open before disconnecting it.
// Attach and expansion must continue to use inspectISCSIMultipathIdentity and
// reject these inaccessible paths.
func inspectISCSIMultipathDetachIdentity(volumeWWN string) (string, int, error) {
	expectedWWID, err := canonicalMultipathWWID(volumeWWN)
	if err != nil {
		return "", 0, err
	}
	mapDevice, err := findMultipathMapByWWID(expectedWWID)
	if err != nil {
		return "", 0, err
	}
	slaves, err := os.ReadDir(filepath.Join("/sys/class/block", filepath.Base(mapDevice), "slaves"))
	if err != nil {
		return "", 0, fmt.Errorf("list slaves for %s: %w", mapDevice, err)
	}

	inaccessiblePaths := 0
	for _, slave := range slaves {
		pathDevice := filepath.Join("/dev", slave.Name())
		pathWWID, readErr := readSCSIWWID(pathDevice)
		if readErr != nil {
			return "", 0, readErr
		}
		if err := validateDetachPathWWID(pathDevice, pathWWID, expectedWWID); err != nil {
			return "", 0, err
		}
		if isUnavailableSCSIWWID(pathWWID) {
			inaccessiblePaths++
		}
	}
	return mapDevice, inaccessiblePaths, nil
}

func validateDetachPathWWID(device, pathWWID, expectedWWID string) error {
	if pathWWID == expectedWWID || isUnavailableSCSIWWID(pathWWID) {
		return nil
	}
	return fmt.Errorf("path %s reports WWID %s, expected %s or an unavailable all-zero sentinel",
		device, pathWWID, expectedWWID)
}

func isUnavailableSCSIWWID(wwid string) bool {
	wwid = strings.TrimSpace(wwid)
	if allZeroes(wwid) {
		return true
	}
	// scsi_id has been observed returning either an NAA type nibble (3) or
	// an NAA type plus IEEE registered-extension nibble (36), followed only
	// by zeroes, when the backing PowerVault mapping is no longer accessible.
	return (strings.HasPrefix(wwid, "3") && allZeroes(wwid[1:])) ||
		(strings.HasPrefix(wwid, "36") && allZeroes(wwid[2:]))
}

func allZeroes(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character != '0' {
			return false
		}
	}
	return true
}

// validatePersistedSinglePath checks that a connector's non-multipath device
// still exists and belongs to the requested volume. A missing device is
// reported separately so NodeUnpublishVolume can preserve CSI idempotency and
// discard the stale connector file.
func validatePersistedSinglePath(device, expectedWWID string) (bool, error) {
	if device == "" {
		return false, fmt.Errorf("persisted iSCSI connector has an empty device path")
	}
	if _, err := os.Stat(device); err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, fmt.Errorf("inspect persisted iSCSI connector path %s: %w", device, err)
	}

	pathWWID, err := readSCSIWWID(device)
	if err != nil {
		return false, err
	}
	if pathWWID != expectedWWID {
		return false, fmt.Errorf("persisted iSCSI connector path %s reports WWID %s, expected %s",
			device, pathWWID, expectedWWID)
	}
	return false, nil
}

func findSCSIPathsByWWID(expectedWWID string) ([]string, error) {
	blocks, err := filepath.Glob("/sys/class/block/sd*")
	if err != nil {
		return nil, fmt.Errorf("list SCSI block devices: %w", err)
	}
	var matches []string
	var unreadable []string
	for _, block := range blocks {
		device := filepath.Join("/dev", filepath.Base(block))
		wwid, readErr := readSCSIWWID(device)
		if readErr != nil {
			unreadable = append(unreadable, device)
			continue
		}
		if wwid == expectedWWID {
			matches = append(matches, device)
		}
	}
	if len(unreadable) > 0 {
		return matches, fmt.Errorf("could not inspect VPD identity for %d SCSI paths: %s",
			len(unreadable), strings.Join(unreadable, ","))
	}
	return matches, nil
}

func canonicalMultipathWWID(volumeWWN string) (string, error) {
	volumeWWN = strings.TrimSpace(volumeWWN)
	if volumeWWN == "" {
		return "", fmt.Errorf("volume WWN is empty")
	}
	for _, r := range volumeWWN {
		if (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return "", fmt.Errorf("volume WWN %q contains unsupported characters", volumeWWN)
		}
	}
	if strings.HasPrefix(volumeWWN, "3") {
		return volumeWWN, nil
	}
	return "3" + volumeWWN, nil
}

func findMultipathMapByWWID(expectedWWID string) (string, error) {
	blocks, err := filepath.Glob("/sys/class/block/dm-*")
	if err != nil {
		return "", fmt.Errorf("list device-mapper devices: %w", err)
	}

	var matches []string
	for _, block := range blocks {
		uuidBytes, readErr := os.ReadFile(filepath.Join(block, "dm", "uuid"))
		if readErr != nil {
			continue
		}
		if strings.TrimSpace(string(uuidBytes)) == "mpath-"+expectedWWID {
			matches = append(matches, filepath.Join("/dev", filepath.Base(block)))
		}
	}
	if len(matches) == 0 {
		return "", os.ErrNotExist
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("WWID %s resolved to %d multipath maps", expectedWWID, len(matches))
	}
	return matches[0], nil
}

func inspectISCSIMultipathMap(volumeWWN string) (*iscsiMapInspection, error) {
	expectedWWID, err := canonicalMultipathWWID(volumeWWN)
	if err != nil {
		return nil, err
	}
	mapDevice, err := findMultipathMapByWWID(expectedWWID)
	if err != nil {
		return nil, fmt.Errorf("find exact multipath map for WWID %s: %w", expectedWWID, err)
	}

	dmName := filepath.Base(mapDevice)
	slaves, err := os.ReadDir(filepath.Join("/sys/class/block", dmName, "slaves"))
	if err != nil {
		return nil, fmt.Errorf("list slaves for %s: %w", mapDevice, err)
	}
	if len(slaves) == 0 {
		return nil, fmt.Errorf("multipath map %s has no paths", mapDevice)
	}

	mapBytes, err := readBlockDeviceBytes(mapDevice)
	if err != nil {
		return nil, err
	}
	inspection := &iscsiMapInspection{
		Device:   mapDevice,
		WWID:     expectedWWID,
		Capacity: mapBytes,
	}

	for _, slave := range slaves {
		pathName := slave.Name()
		pathDevice := filepath.Join("/dev", pathName)
		pathWWID, readErr := readSCSIWWID(pathDevice)
		if readErr != nil {
			return nil, readErr
		}
		if pathWWID != expectedWWID {
			return nil, fmt.Errorf("path %s reports WWID %s, expected %s", pathDevice, pathWWID, expectedWWID)
		}

		targetBytes, readErr := readSCSITargetBytes(pathDevice)
		if readErr != nil {
			return nil, readErr
		}
		cachedBytes, readErr := readBlockDeviceBytes(pathDevice)
		if readErr != nil {
			return nil, readErr
		}
		if cachedBytes != targetBytes {
			return nil, fmt.Errorf("path %s cached capacity %d does not match target capacity %d", pathDevice, cachedBytes, targetBytes)
		}
		if inspection.TargetBytes == 0 {
			inspection.TargetBytes = targetBytes
		} else if inspection.TargetBytes != targetBytes {
			return nil, fmt.Errorf("path %s target capacity %d disagrees with %d", pathDevice, targetBytes, inspection.TargetBytes)
		}
		inspection.SlaveNames = append(inspection.SlaveNames, pathName)
	}

	if mapBytes != inspection.TargetBytes {
		return nil, fmt.Errorf("multipath map %s capacity %d does not match target capacity %d", mapDevice, mapBytes, inspection.TargetBytes)
	}
	return inspection, nil
}

func rescanAndResizeISCSIMultipath(volumeWWN string) (*iscsiMapInspection, error) {
	mapDevice, err := inspectISCSIMultipathIdentity(volumeWWN)
	if err != nil {
		return nil, fmt.Errorf("validate multipath identity before expansion: %w", err)
	}
	slaves, err := os.ReadDir(filepath.Join("/sys/class/block", filepath.Base(mapDevice), "slaves"))
	if err != nil {
		return nil, fmt.Errorf("list slaves for %s: %w", mapDevice, err)
	}
	for _, slave := range slaves {
		rescanPath := filepath.Join("/sys/class/block", slave.Name(), "device", "rescan")
		if err := os.WriteFile(rescanPath, []byte("1\n"), 0); err != nil {
			return nil, fmt.Errorf("rescan %s: %w", filepath.Join("/dev", slave.Name()), err)
		}
	}
	if _, err := runTimedCommand(35*time.Second, "udevadm", "settle", "--timeout=30"); err != nil {
		return nil, fmt.Errorf("settle after SCSI path rescan: %w", err)
	}
	if output, err := runInspectionCommand("multipathd", "resize", "map", mapDevice); err != nil {
		return nil, fmt.Errorf("resize multipath map %s: %s: %w", mapDevice, strings.TrimSpace(string(output)), err)
	}
	if _, err := runTimedCommand(35*time.Second, "udevadm", "settle", "--timeout=30"); err != nil {
		return nil, fmt.Errorf("settle after multipath resize: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		inspection, inspectErr := inspectISCSIMultipathMap(volumeWWN)
		if inspectErr == nil {
			return inspection, nil
		}
		lastErr = inspectErr
		time.Sleep(time.Second)
	}
	return nil, fmt.Errorf("validate multipath capacity after expansion: %w", lastErr)
}

func readBlockDeviceBytes(device string) (uint64, error) {
	output, err := runInspectionCommand("blockdev", "--getsize64", device)
	if err != nil {
		return 0, fmt.Errorf("read cached capacity for %s: %w", device, err)
	}
	bytes, err := strconv.ParseUint(strings.TrimSpace(string(output)), 10, 64)
	if err != nil || bytes == 0 {
		return 0, fmt.Errorf("parse cached capacity for %s from %q", device, strings.TrimSpace(string(output)))
	}
	return bytes, nil
}

func readSCSIWWID(device string) (string, error) {
	scsiID, err := findSCSIID()
	if err != nil {
		return "", err
	}
	output, err := runInspectionCommand(scsiID, "-g", "-u", "-p", "0x83", "-d", device)
	if err != nil {
		return "", fmt.Errorf("read VPD WWID for %s: %w", device, err)
	}
	wwid := strings.TrimSpace(string(output))
	if wwid == "" || strings.ContainsAny(wwid, " \t\r\n") {
		return "", fmt.Errorf("invalid VPD WWID %q for %s", wwid, device)
	}
	return wwid, nil
}

func readSCSITargetBytes(device string) (uint64, error) {
	output, err := runInspectionCommand("sg_readcap", "--long", "--brief", "--readonly", device)
	if err != nil {
		return 0, fmt.Errorf("read target capacity for %s: %w", device, err)
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 {
		return 0, fmt.Errorf("unexpected READ CAPACITY output for %s: %q", device, strings.TrimSpace(string(output)))
	}
	blockCount, err := strconv.ParseUint(fields[0], 0, 64)
	if err != nil {
		return 0, fmt.Errorf("parse block count for %s: %w", device, err)
	}
	blockSize, err := strconv.ParseUint(fields[1], 0, 64)
	if err != nil {
		return 0, fmt.Errorf("parse block size for %s: %w", device, err)
	}
	if blockCount == 0 || blockSize == 0 || blockCount > ^uint64(0)/blockSize {
		return 0, fmt.Errorf("invalid target geometry for %s: blocks=%d block-size=%d", device, blockCount, blockSize)
	}
	return blockCount * blockSize, nil
}

func findSCSIID() (string, error) {
	for _, candidate := range []string{"scsi_id", "/usr/lib/udev/scsi_id", "/lib/udev/scsi_id"} {
		path, err := exec.LookPath(candidate)
		if err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("scsi_id executable was not found")
}

func runInspectionCommand(name string, args ...string) ([]byte, error) {
	return runTimedCommand(iscsiInspectionTimeout, name, args...)
}

func runTimedCommand(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s timed out", name)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %s (%w)", name, strings.TrimSpace(string(output)), err)
	}
	return output, nil
}
