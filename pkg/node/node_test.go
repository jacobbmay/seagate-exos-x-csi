package node

import (
	"errors"
	"testing"
)

const rawBlockTarget = "/var/lib/kubelet/plugins/kubernetes.io/csi/volumeDevices/publish/pvc-test/preparation-pod"

func TestUnpublishTargetKeepsDeviceForAnotherBlockPublication(t *testing.T) {
	unmountCalls := 0
	inspectionCalls := 0
	detachCalls := 0

	err := unpublishTargetWithOperations(
		rawBlockTarget,
		func(string) error {
			unmountCalls++
			return nil
		},
		func(string) (bool, error) {
			inspectionCalls++
			return true, nil
		},
		func() error {
			detachCalls++
			return nil
		},
	)

	if err != nil {
		t.Fatal(err)
	}
	if unmountCalls != 1 || inspectionCalls != 1 || detachCalls != 0 {
		t.Fatalf("unexpected calls: unmount=%d inspection=%d detach=%d", unmountCalls, inspectionCalls, detachCalls)
	}
}

func TestUnpublishTargetDetachesFinalBlockPublication(t *testing.T) {
	detachCalls := 0
	err := unpublishTargetWithOperations(
		rawBlockTarget,
		func(string) error { return nil },
		func(string) (bool, error) { return false, nil },
		func() error {
			detachCalls++
			return nil
		},
	)

	if err != nil {
		t.Fatal(err)
	}
	if detachCalls != 1 {
		t.Fatalf("expected one detach, got %d", detachCalls)
	}
}

func TestUnpublishTargetTreatsAbsentTargetIdempotently(t *testing.T) {
	detachCalls := 0
	err := unpublishTargetWithOperations(
		rawBlockTarget,
		func(string) error { return nil }, // Target was already removed.
		func(string) (bool, error) { return true, nil },
		func() error {
			detachCalls++
			return nil
		},
	)

	if err != nil {
		t.Fatal(err)
	}
	if detachCalls != 0 {
		t.Fatalf("stale unpublish detached storage %d times", detachCalls)
	}
}

func TestUnpublishTargetFailsClosedOnInspectionError(t *testing.T) {
	detachCalls := 0
	wantErr := errors.New("cannot inspect publications")
	err := unpublishTargetWithOperations(
		rawBlockTarget,
		func(string) error { return nil },
		func(string) (bool, error) { return false, wantErr },
		func() error {
			detachCalls++
			return nil
		},
	)

	if !errors.Is(err, wantErr) {
		t.Fatalf("expected inspection error, got %v", err)
	}
	if detachCalls != 0 {
		t.Fatalf("inspection failure detached storage %d times", detachCalls)
	}
}

func TestUnpublishTargetDoesNotDetachAfterUnmountFailure(t *testing.T) {
	detachCalls := 0
	wantErr := errors.New("unmount failed")
	err := unpublishTargetWithOperations(
		rawBlockTarget,
		func(string) error { return wantErr },
		func(string) (bool, error) {
			t.Fatal("publication inspection must not run after an unmount failure")
			return false, nil
		},
		func() error {
			detachCalls++
			return nil
		},
	)

	if !errors.Is(err, wantErr) {
		t.Fatalf("expected unmount error, got %v", err)
	}
	if detachCalls != 0 {
		t.Fatalf("unmount failure detached storage %d times", detachCalls)
	}
}

func TestUnpublishTargetKeepsDeviceForAnotherFilesystemPublication(t *testing.T) {
	inspectionCalls := 0
	detachCalls := 0
	err := unpublishTargetWithOperations(
		"/var/lib/kubelet/pods/pod-a/volumes/kubernetes.io~csi/pvc-test/mount",
		func(string) error { return nil },
		func(string) (bool, error) {
			inspectionCalls++
			return true, nil
		},
		func() error {
			detachCalls++
			return nil
		},
	)

	if err != nil {
		t.Fatal(err)
	}
	if inspectionCalls != 1 || detachCalls != 0 {
		t.Fatalf("unexpected calls: inspection=%d detach=%d", inspectionCalls, detachCalls)
	}
}

func TestUnpublishTargetDetachesFinalFilesystemPublication(t *testing.T) {
	detachCalls := 0
	err := unpublishTargetWithOperations(
		"/var/lib/kubelet/pods/pod-a/volumes/kubernetes.io~csi/pvc-test/mount",
		func(string) error { return nil },
		func(string) (bool, error) { return false, nil },
		func() error {
			detachCalls++
			return nil
		},
	)

	if err != nil {
		t.Fatal(err)
	}
	if detachCalls != 1 {
		t.Fatalf("expected one detach, got %d", detachCalls)
	}
}
