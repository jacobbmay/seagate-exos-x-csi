package controller

import (
	"errors"
	"testing"

	storageapitypes "github.com/Seagate/seagate-exos-x-api-go/v2/pkg/common"
)

type fakeInitiatorRegistrationClient struct {
	createCalls  []string
	registered   map[string]bool
	createErr    error
	createStatus *storageapitypes.ResponseStatus
}

func (client *fakeInitiatorRegistrationClient) CreateNickname(name, iqn string) (*storageapitypes.ResponseStatus, error) {
	client.createCalls = append(client.createCalls, name+":"+iqn)
	if client.createErr == nil && (client.createStatus == nil || client.createStatus.ResponseTypeNumeric == 0) {
		client.registered[iqn] = true
	}
	return client.createStatus, client.createErr
}

func (client *fakeInitiatorRegistrationClient) GetInitiatorHostGroup(initiator string) (string, string, error) {
	if client.registered[initiator] {
		return "", "", nil
	}
	return "", "", errors.New("initiator not found")
}

func TestEnsureInitiatorsRegisteredSkipsKnownInitiator(t *testing.T) {
	initiator := "iqn.1994-05.com.redhat:known"
	client := &fakeInitiatorRegistrationClient{registered: map[string]bool{initiator: true}}

	err := ensureInitiatorsRegistered(client, map[string]bool{initiator: true}, []string{initiator})
	if err != nil {
		t.Fatal(err)
	}
	if len(client.createCalls) != 0 {
		t.Fatalf("expected no registration calls, got %v", client.createCalls)
	}
}

func TestEnsureInitiatorsRegisteredCreatesAndVerifiesNickname(t *testing.T) {
	initiator := "iqn.1994-05.com.redhat:new"
	client := &fakeInitiatorRegistrationClient{registered: map[string]bool{}}
	known := map[string]bool{}

	err := ensureInitiatorsRegistered(client, known, []string{initiator})
	if err != nil {
		t.Fatal(err)
	}
	if len(client.createCalls) != 1 {
		t.Fatalf("expected one registration call, got %v", client.createCalls)
	}
	if !known[initiator] {
		t.Fatal("registered initiator was not added to the known set")
	}
}

func TestEnsureInitiatorsRegisteredReconcilesAmbiguousCreateError(t *testing.T) {
	initiator := "iqn.1994-05.com.redhat:ambiguous"
	client := &fakeInitiatorRegistrationClient{
		registered: map[string]bool{initiator: true},
		createErr:  errors.New("connection reset"),
	}

	err := ensureInitiatorsRegistered(client, map[string]bool{}, []string{initiator})
	if err != nil {
		t.Fatalf("expected verification to reconcile the create error, got %v", err)
	}
}

func TestEnsureInitiatorsRegisteredReturnsCreateFailure(t *testing.T) {
	initiator := "iqn.1994-05.com.redhat:failed"
	client := &fakeInitiatorRegistrationClient{
		registered: map[string]bool{},
		createErr:  errors.New("API unavailable"),
	}

	if err := ensureInitiatorsRegistered(client, map[string]bool{}, []string{initiator}); err == nil {
		t.Fatal("expected registration failure")
	}
}

func TestInitiatorNicknameIsStableAndBounded(t *testing.T) {
	initiator := "iqn.1994-05.com.redhat:eec4323cc38a"
	first := initiatorNickname(initiator)
	second := initiatorNickname(initiator)
	if first != second {
		t.Fatalf("nickname is not stable: %q != %q", first, second)
	}
	if len(first) != 20 {
		t.Fatalf("expected a 20-character nickname, got %q", first)
	}
	if first == initiatorNickname(initiator+"-other") {
		t.Fatal("different initiators produced the same nickname")
	}
}
