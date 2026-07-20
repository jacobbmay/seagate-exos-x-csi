package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

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
