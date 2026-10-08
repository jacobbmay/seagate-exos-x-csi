package controller

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
)

func TestAuthenticatedControllerRPCsShareOneSerializationBoundary(t *testing.T) {
	interceptor := serializeAuthenticatedControllerRPCs()
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan struct{})
	secondAttempted := make(chan struct{})
	secondStarted := make(chan struct{})
	secondDone := make(chan struct{})

	go func() {
		defer close(firstDone)
		_, _ = interceptor(context.Background(), nil,
			&grpc.UnaryServerInfo{FullMethod: "/csi.v1.Controller/CreateVolume"},
			func(context.Context, interface{}) (interface{}, error) {
				close(firstStarted)
				<-releaseFirst
				return nil, nil
			})
	}()

	<-firstStarted
	go func() {
		defer close(secondDone)
		close(secondAttempted)
		_, _ = interceptor(context.Background(), nil,
			&grpc.UnaryServerInfo{FullMethod: "/csi.v1.Controller/ControllerPublishVolume"},
			func(context.Context, interface{}) (interface{}, error) {
				close(secondStarted)
				return nil, nil
			})
	}()

	<-secondAttempted
	select {
	case <-secondStarted:
		t.Fatal("a different authenticated RPC entered while CreateVolume owned the shared client")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseFirst)
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second authenticated RPC did not enter after the first completed")
	}
	<-firstDone
	<-secondDone
}

func TestUnauthenticatedControllerRPCDoesNotWaitForStorageClient(t *testing.T) {
	interceptor := serializeAuthenticatedControllerRPCs()
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan struct{})

	go func() {
		defer close(firstDone)
		_, _ = interceptor(context.Background(), nil,
			&grpc.UnaryServerInfo{FullMethod: "/csi.v1.Controller/DeleteVolume"},
			func(context.Context, interface{}) (interface{}, error) {
				close(firstStarted)
				<-releaseFirst
				return nil, nil
			})
	}()

	<-firstStarted
	probeStarted := make(chan struct{})
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		_, _ = interceptor(context.Background(), nil,
			&grpc.UnaryServerInfo{FullMethod: "/csi.v1.Identity/Probe"},
			func(context.Context, interface{}) (interface{}, error) {
				close(probeStarted)
				return nil, nil
			})
	}()

	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("unauthenticated Probe was unnecessarily blocked")
	}

	close(releaseFirst)
	<-firstDone
	<-probeDone
}
