package controller

import (
	"errors"
	"testing"

	storageapitypes "github.com/Seagate/seagate-exos-x-api-go/v2/pkg/common"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

func TestRequireUnpublishInitiators(t *testing.T) {
	tests := []struct {
		name       string
		initiators []string
		lookupErr  error
		wantCode   codes.Code
	}{
		{name: "lookup unavailable", lookupErr: errors.New("node service refused connection"), wantCode: codes.Unavailable},
		{name: "empty result", wantCode: codes.FailedPrecondition},
		{name: "empty initiator", initiators: []string{""}, wantCode: codes.FailedPrecondition},
		{name: "valid initiator", initiators: []string{"iqn.1994-05.com.redhat:test"}, wantCode: codes.OK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := requireUnpublishInitiators("10.10.33.51", "iscsi", tt.initiators, tt.lookupErr)
			if status.Code(err) != tt.wantCode {
				t.Fatalf("status code = %s, want %s (error %v)", status.Code(err), tt.wantCode, err)
			}
			if tt.wantCode == codes.OK && len(got) != 1 {
				t.Fatalf("valid initiators = %v, want one entry", got)
			}
		})
	}
}

func TestValidateControllerUnmapResult(t *testing.T) {
	tests := []struct {
		name            string
		apiStatus       *storageapitypes.ResponseStatus
		unmapErr        error
		wantAlreadyGone bool
		wantCode        codes.Code
	}{
		{name: "success", wantCode: codes.OK},
		{
			name:            "array reports already unmapped",
			apiStatus:       &storageapitypes.ResponseStatus{ReturnCode: storageapitypes.UnmapFailedErrorCode},
			unmapErr:        errors.New("mapping does not exist"),
			wantAlreadyGone: true,
			wantCode:        codes.OK,
		},
		{
			name:      "generic array error is not success",
			apiStatus: &storageapitypes.ResponseStatus{ReturnCode: -1},
			unmapErr:  errors.New("controller communication failed"),
			wantCode:  codes.Internal,
		},
		{
			name:     "transport error without status is not success",
			unmapErr: errors.New("connection reset"),
			wantCode: codes.Internal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			alreadyGone, err := validateControllerUnmapResult("volume", "initiator", tt.apiStatus, tt.unmapErr)
			if alreadyGone != tt.wantAlreadyGone {
				t.Fatalf("already unmapped = %v, want %v", alreadyGone, tt.wantAlreadyGone)
			}
			if status.Code(err) != tt.wantCode {
				t.Fatalf("status code = %s, want %s (error %v)", status.Code(err), tt.wantCode, err)
			}
		})
	}
}
