package reolink

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/baichuan"
)

type fakeDiscoveryClient struct {
	device       func(context.Context) (baichuan.DeviceInfo, error)
	capabilities func(context.Context, uint8) (baichuan.Capabilities, error)
}

func (c fakeDiscoveryClient) DeviceInfo(ctx context.Context) (baichuan.DeviceInfo, error) {
	return c.device(ctx)
}

func (c fakeDiscoveryClient) Capabilities(
	ctx context.Context, channel uint8,
) (baichuan.Capabilities, error) {
	return c.capabilities(ctx, channel)
}

func TestDiscoveryCacheDeduplicatesConcurrentProfiles(t *testing.T) {
	var deviceCalls atomic.Int32
	var capabilityCalls [256]atomic.Int32
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	client := fakeDiscoveryClient{
		device: func(ctx context.Context) (baichuan.DeviceInfo, error) {
			deviceCalls.Add(1)
			started <- struct{}{}
			select {
			case <-release:
				return baichuan.DeviceInfo{Model: "camera"}, nil
			case <-ctx.Done():
				return baichuan.DeviceInfo{}, ctx.Err()
			}
		},
		capabilities: func(ctx context.Context, channel uint8) (baichuan.Capabilities, error) {
			capabilityCalls[channel].Add(1)
			started <- struct{}{}
			select {
			case <-release:
				return baichuan.Capabilities{
					Channel: channel, ObservedChannels: []uint8{channel}, ReadWrite: []string{"preview"},
				}, nil
			case <-ctx.Done():
				return baichuan.Capabilities{}, ctx.Err()
			}
		},
	}

	camera := &Camera{}
	const profiles = 16
	type discoveryResult struct {
		channel  uint8
		snapshot discoverySnapshot
	}
	results := make(chan discoveryResult, profiles)
	start := make(chan struct{})
	for i := range profiles {
		go func() {
			<-start
			channel := uint8(i % 2)
			results <- discoveryResult{
				channel: channel, snapshot: camera.discover(context.Background(), client, channel),
			}
		}()
	}
	close(start)
	for range 3 {
		<-started
	}
	close(release)

	for range profiles {
		result := <-results
		if result.snapshot.deviceStatus != "available" || result.snapshot.capabilityStatus != "available" ||
			result.snapshot.device.Model != "camera" || len(result.snapshot.capabilities.ReadWrite) != 1 ||
			result.snapshot.capabilities.Channel != result.channel {
			t.Fatalf("unexpected channel %d discovery result: %+v", result.channel, result.snapshot)
		}
	}
	if got := deviceCalls.Load(); got != 1 {
		t.Fatalf("device calls = %d, want 1", got)
	}
	for channel := range 2 {
		if got := capabilityCalls[channel].Load(); got != 1 {
			t.Fatalf("channel %d capability calls = %d, want 1", channel, got)
		}
	}

	result := camera.discover(context.Background(), client, 0)
	result.capabilities.ReadWrite[0] = "changed"
	result = camera.discover(context.Background(), client, 0)
	if result.capabilities.ReadWrite[0] != "preview" {
		t.Fatal("caller mutated cached capabilities")
	}
}

func TestDiscoveryCacheRetriesFailures(t *testing.T) {
	var deviceCalls atomic.Int32
	var capabilityCalls atomic.Int32
	client := fakeDiscoveryClient{
		device: func(context.Context) (baichuan.DeviceInfo, error) {
			if deviceCalls.Add(1) == 1 {
				return baichuan.DeviceInfo{}, errors.New("temporary device failure")
			}
			return baichuan.DeviceInfo{Model: "camera"}, nil
		},
		capabilities: func(context.Context, uint8) (baichuan.Capabilities, error) {
			capabilityCalls.Add(1)
			return baichuan.Capabilities{Channel: 3}, nil
		},
	}
	camera := &Camera{}

	if result := camera.discover(context.Background(), client, 3); result.deviceStatus == "available" {
		t.Fatal("failed device query was cached as available")
	}
	for range 2 {
		result := camera.discover(context.Background(), client, 3)
		if result.deviceStatus != "available" || result.capabilityStatus != "available" {
			t.Fatalf("unexpected retry result: %+v", result)
		}
	}
	if got := deviceCalls.Load(); got != 2 {
		t.Fatalf("device calls = %d, want 2", got)
	}
	if got := capabilityCalls.Load(); got != 1 {
		t.Fatalf("capability calls = %d, want 1", got)
	}
}

func TestDiscoveryCacheWaiterCancellation(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	client := fakeDiscoveryClient{device: func(ctx context.Context) (baichuan.DeviceInfo, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return baichuan.DeviceInfo{Model: "camera"}, nil
		case <-ctx.Done():
			return baichuan.DeviceInfo{}, ctx.Err()
		}
	}}
	cache := &discoveryCache{}
	owner := make(chan error, 1)
	go func() {
		_, err := cache.deviceInfo(context.Background(), client)
		owner <- err
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.deviceInfo(ctx, client); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter error = %v, want canceled", err)
	}
	close(release)
	if err := <-owner; err != nil {
		t.Fatal(err)
	}
	if value, err := cache.deviceInfo(context.Background(), client); err != nil || value.Model != "camera" {
		t.Fatalf("cached device = %+v, %v", value, err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("device calls = %d, want 1", got)
	}
}
