package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func filesystemPublishRequest(targetPath string, readOnly bool) *csi.NodePublishVolumeRequest {
	return &csi.NodePublishVolumeRequest{
		TargetPath: targetPath,
		Readonly:   readOnly,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"},
			},
		},
	}
}

func TestMountFilesystemFirstPublication(t *testing.T) {
	req := filesystemPublishRequest("/target-a", false)
	ensureCalls := 0
	checkCalls := 0
	mountCalls := 0
	bindCalls := 0

	err := mountFilesystemWithOperations(req, "/dev/dm-4", filesystemMountOperations{
		ensureFsType: func(fsType, devicePath string) error {
			ensureCalls++
			if fsType != "ext4" || devicePath != "/dev/dm-4" {
				t.Fatalf("unexpected format arguments: %q %q", fsType, devicePath)
			}
			return nil
		},
		checkFs: func(devicePath, fsType, context string) error {
			checkCalls++
			return nil
		},
		findMountpoints: func(string) ([]string, error) { return nil, nil },
		prepareTarget:   func(string) error { return nil },
		mountDevice: func(devicePath, targetPath, fsType string) error {
			mountCalls++
			return nil
		},
		bindMount: func(string, string) error {
			bindCalls++
			return nil
		},
		setReadOnly: func(string, bool) error { return nil },
		unmount:     func(string) error { return nil },
	})

	if err != nil {
		t.Fatal(err)
	}
	if ensureCalls != 1 || checkCalls != 1 || mountCalls != 1 || bindCalls != 0 {
		t.Fatalf("unexpected calls: ensure=%d check=%d mount=%d bind=%d", ensureCalls, checkCalls, mountCalls, bindCalls)
	}
}

func TestMountFilesystemRepeatedPublicationIsIdempotent(t *testing.T) {
	req := filesystemPublishRequest("/target-a", false)
	operationCalled := false
	err := mountFilesystemWithOperations(req, "/dev/dm-4", filesystemMountOperations{
		ensureFsType: func(string, string) error { operationCalled = true; return nil },
		checkFs:      func(string, string, string) error { operationCalled = true; return nil },
		findMountpoints: func(string) ([]string, error) {
			return []string{"/target-a"}, nil
		},
		prepareTarget: func(string) error { operationCalled = true; return nil },
		mountDevice:   func(string, string, string) error { operationCalled = true; return nil },
		bindMount:     func(string, string) error { operationCalled = true; return nil },
		setReadOnly:   func(string, bool) error { operationCalled = true; return nil },
		unmount:       func(string) error { operationCalled = true; return nil },
	})

	if err != nil {
		t.Fatal(err)
	}
	if operationCalled {
		t.Fatal("idempotent publish performed an additional mount operation")
	}
}

func TestMountFilesystemBindsExistingPublicationToNewTarget(t *testing.T) {
	req := filesystemPublishRequest("/target-b", false)
	ensureCalls := 0
	bindSource := ""
	bindTarget := ""
	readOnlyConfigured := true

	err := mountFilesystemWithOperations(req, "/dev/dm-4", filesystemMountOperations{
		ensureFsType: func(string, string) error { ensureCalls++; return nil },
		checkFs:      func(string, string, string) error { return nil },
		findMountpoints: func(string) ([]string, error) {
			return []string{"/target-a"}, nil
		},
		prepareTarget: func(string) error { return nil },
		mountDevice:   func(string, string, string) error { return nil },
		bindMount: func(sourcePath, targetPath string) error {
			bindSource = sourcePath
			bindTarget = targetPath
			return nil
		},
		setReadOnly: func(targetPath string, readOnly bool) error {
			readOnlyConfigured = readOnly
			return nil
		},
		unmount: func(string) error { return nil },
	})

	if err != nil {
		t.Fatal(err)
	}
	if ensureCalls != 0 {
		t.Fatalf("existing filesystem was formatted or checked %d times", ensureCalls)
	}
	if bindSource != "/target-a" || bindTarget != "/target-b" {
		t.Fatalf("unexpected bind mount: %q -> %q", bindSource, bindTarget)
	}
	if readOnlyConfigured {
		t.Fatal("writable publication was configured read-only")
	}
}

func TestMountFilesystemPreservesReadOnlyOnBindTarget(t *testing.T) {
	req := filesystemPublishRequest("/target-b", true)
	readOnlyConfigured := false
	err := mountFilesystemWithOperations(req, "/dev/dm-4", filesystemMountOperations{
		ensureFsType:    func(string, string) error { return nil },
		checkFs:         func(string, string, string) error { return nil },
		findMountpoints: func(string) ([]string, error) { return []string{"/target-a"}, nil },
		prepareTarget:   func(string) error { return nil },
		mountDevice:     func(string, string, string) error { return nil },
		bindMount:       func(string, string) error { return nil },
		setReadOnly: func(targetPath string, readOnly bool) error {
			readOnlyConfigured = readOnly
			return nil
		},
		unmount: func(string) error { return nil },
	})

	if err != nil {
		t.Fatal(err)
	}
	if !readOnlyConfigured {
		t.Fatal("read-only publication was not configured read-only")
	}
}

func TestMountFilesystemPreservesReadOnlyOnFirstTarget(t *testing.T) {
	req := filesystemPublishRequest("/target-a", true)
	readOnlyConfigured := false
	err := mountFilesystemWithOperations(req, "/dev/dm-4", filesystemMountOperations{
		ensureFsType:    func(string, string) error { return nil },
		checkFs:         func(string, string, string) error { return nil },
		findMountpoints: func(string) ([]string, error) { return nil, nil },
		prepareTarget:   func(string) error { return nil },
		mountDevice:     func(string, string, string) error { return nil },
		bindMount:       func(string, string) error { return nil },
		setReadOnly: func(targetPath string, readOnly bool) error {
			readOnlyConfigured = readOnly
			return nil
		},
		unmount: func(string) error { return nil },
	})

	if err != nil {
		t.Fatal(err)
	}
	if !readOnlyConfigured {
		t.Fatal("first read-only publication was not configured read-only")
	}
}

func TestMountFilesystemCleansUpFailedBindConfiguration(t *testing.T) {
	req := filesystemPublishRequest("/target-b", true)
	wantErr := errors.New("remount failed")
	unmountedTarget := ""
	err := mountFilesystemWithOperations(req, "/dev/dm-4", filesystemMountOperations{
		ensureFsType:    func(string, string) error { return nil },
		checkFs:         func(string, string, string) error { return nil },
		findMountpoints: func(string) ([]string, error) { return []string{"/target-a"}, nil },
		prepareTarget:   func(string) error { return nil },
		mountDevice:     func(string, string, string) error { return nil },
		bindMount:       func(string, string) error { return nil },
		setReadOnly:     func(string, bool) error { return wantErr },
		unmount: func(targetPath string) error {
			unmountedTarget = targetPath
			return nil
		},
	})

	if !errors.Is(err, wantErr) {
		t.Fatalf("expected remount error, got %v", err)
	}
	if unmountedTarget != "/target-b" {
		t.Fatalf("failed bind target was not cleaned up: %q", unmountedTarget)
	}
}

func TestIsKubeletBlockVolumeTarget(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{
			name: "raw block target",
			path: "/var/lib/kubelet/plugins/kubernetes.io/csi/volumeDevices/publish/pvc-id/pod-id",
			want: true,
		},
		{
			name: "custom kubelet root",
			path: "/custom/kubelet/plugins/kubernetes.io/csi/volumeDevices/publish/pvc-id/pod-id",
			want: true,
		},
		{
			name: "filesystem target",
			path: "/var/lib/kubelet/pods/pod-id/volumes/kubernetes.io~csi/pvc-id/mount",
			want: false,
		},
		{
			name: "publication directory without pod target",
			path: "/var/lib/kubelet/plugins/kubernetes.io/csi/volumeDevices/publish/pvc-id",
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsKubeletBlockVolumeTarget(test.path); got != test.want {
				t.Fatalf("IsKubeletBlockVolumeTarget(%q) = %v, want %v", test.path, got, test.want)
			}
		})
	}
}

func TestHasOtherBlockVolumePublications(t *testing.T) {
	publicationDir := t.TempDir()
	oldTarget := filepath.Join(publicationDir, "preparation-pod")
	activeTarget := filepath.Join(publicationDir, "osd-pod")
	staleTarget := filepath.Join(publicationDir, "stale-pod")
	for _, path := range []string{oldTarget, activeTarget, staleTarget} {
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}

	checked := map[string]bool{}
	hasOther, err := hasOtherBlockVolumePublications(oldTarget, func(path string) (bool, error) {
		checked[path] = true
		return path == activeTarget, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasOther {
		t.Fatal("expected active sibling publication to be detected")
	}
	if checked[oldTarget] {
		t.Fatal("the target being unpublished must be excluded")
	}
}

func TestHasOtherBlockVolumePublicationsIgnoresStaleTargets(t *testing.T) {
	publicationDir := t.TempDir()
	oldTarget := filepath.Join(publicationDir, "preparation-pod")
	staleTarget := filepath.Join(publicationDir, "stale-pod")
	if err := os.WriteFile(staleTarget, nil, 0600); err != nil {
		t.Fatal(err)
	}

	hasOther, err := hasOtherBlockVolumePublications(oldTarget, func(string) (bool, error) {
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasOther {
		t.Fatal("stale regular file was treated as an active block publication")
	}
}

func TestHasOtherBlockVolumePublicationsFailsClosed(t *testing.T) {
	publicationDir := t.TempDir()
	oldTarget := filepath.Join(publicationDir, "preparation-pod")
	otherTarget := filepath.Join(publicationDir, "osd-pod")
	if err := os.WriteFile(otherTarget, nil, 0600); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("stat failed")
	_, err := hasOtherBlockVolumePublications(oldTarget, func(string) (bool, error) {
		return false, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected inspection error, got %v", err)
	}
}

func TestKubeletFilesystemTargetInfo(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantRoot   string
		wantVolume string
		wantOK     bool
	}{
		{
			name:       "standard kubelet target",
			path:       "/var/lib/kubelet/pods/pod-a/volumes/kubernetes.io~csi/pvc-test/mount",
			wantRoot:   "/var/lib/kubelet/pods",
			wantVolume: "pvc-test",
			wantOK:     true,
		},
		{
			name:       "custom kubelet root",
			path:       "/custom/kubelet/pods/pod-a/volumes/kubernetes.io~csi/pvc-test/mount",
			wantRoot:   "/custom/kubelet/pods",
			wantVolume: "pvc-test",
			wantOK:     true,
		},
		{
			name:   "raw block target",
			path:   "/var/lib/kubelet/plugins/kubernetes.io/csi/volumeDevices/publish/pvc-test/pod-a",
			wantOK: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, volume, ok := kubeletFilesystemTargetInfo(test.path)
			if root != test.wantRoot || volume != test.wantVolume || ok != test.wantOK {
				t.Fatalf("kubeletFilesystemTargetInfo(%q) = (%q, %q, %v), want (%q, %q, %v)", test.path, root, volume, ok, test.wantRoot, test.wantVolume, test.wantOK)
			}
		})
	}
}

func TestHasOtherFilesystemVolumePublications(t *testing.T) {
	podsRoot := filepath.Join(t.TempDir(), "pods")
	oldTarget := filepath.Join(podsRoot, "old-pod", "volumes", "kubernetes.io~csi", "pvc-test", "mount")
	activeTarget := filepath.Join(podsRoot, "new-pod", "volumes", "kubernetes.io~csi", "pvc-test", "mount")
	otherVolumeTarget := filepath.Join(podsRoot, "other-pod", "volumes", "kubernetes.io~csi", "other-pvc", "mount")
	for _, path := range []string{oldTarget, activeTarget, otherVolumeTarget} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}

	checked := map[string]bool{}
	hasOther, err := hasOtherFilesystemVolumePublications(oldTarget, podsRoot, "pvc-test", func(path string) (bool, error) {
		checked[path] = true
		return path == activeTarget, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasOther {
		t.Fatal("expected active filesystem sibling publication to be detected")
	}
	if checked[oldTarget] {
		t.Fatal("the target being unpublished must be excluded")
	}
	if checked[otherVolumeTarget] {
		t.Fatal("a target for another volume must not be inspected")
	}
}

func TestHasOtherFilesystemVolumePublicationsIgnoresStaleTargets(t *testing.T) {
	podsRoot := filepath.Join(t.TempDir(), "pods")
	oldTarget := filepath.Join(podsRoot, "old-pod", "volumes", "kubernetes.io~csi", "pvc-test", "mount")
	staleTarget := filepath.Join(podsRoot, "stale-pod", "volumes", "kubernetes.io~csi", "pvc-test", "mount")
	if err := os.MkdirAll(staleTarget, 0755); err != nil {
		t.Fatal(err)
	}

	hasOther, err := hasOtherFilesystemVolumePublications(oldTarget, podsRoot, "pvc-test", func(string) (bool, error) {
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasOther {
		t.Fatal("stale filesystem target was treated as an active publication")
	}
}

func TestParseMultipathOpenCount(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    bool
		wantErr bool
	}{
		{name: "closed", output: "0\n", want: false},
		{name: "open", output: "  2 \n", want: true},
		{name: "invalid", output: "not-a-number\n", wantErr: true},
		{name: "missing", output: "", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseMultipathOpenCount([]byte(test.output))
			if (err != nil) != test.wantErr {
				t.Fatalf("parseMultipathOpenCount() error = %v, wantErr %v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("parseMultipathOpenCount() = %v, want %v", got, test.want)
			}
		})
	}
}
