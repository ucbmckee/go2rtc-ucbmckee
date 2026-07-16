package baichuan

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestCameraPreview(t *testing.T) {
	cfg := cameraConfig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Dial(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	preview, err := client.StartPreview(ctx, 0, StreamMain)
	if err != nil {
		t.Fatal(err)
	}
	defer preview.Close()

	var info, video, audio bool
	for i := 0; i < 64 && (!info || !video || !audio); i++ {
		packet, err := preview.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		switch packet.Kind {
		case MediaInfo:
			info = packet.Width > 0 && packet.Height > 0 && packet.FPS > 0
		case MediaVideoI, MediaVideoP:
			video = packet.Codec == "H264" || packet.Codec == "H265"
		case MediaAAC, MediaADPCM:
			audio = len(packet.Data) > 0
		}
	}
	if !info || !video || !audio {
		t.Fatalf("missing media: info=%t video=%t audio=%t", info, video, audio)
	}
}

func TestCameraProfiles(t *testing.T) {
	cfg := cameraConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errs := make(chan error, 2)
	for _, stream := range []Stream{StreamMain, StreamSub} {
		go func() { errs <- readCameraProfile(ctx, cfg, 0, stream) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestCameraCapabilities(t *testing.T) {
	cfg := cameraConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Dial(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	channel := uint8(0)
	if value := os.Getenv("REOLINK_CHANNEL"); value != "" {
		if _, err = fmt.Sscanf(value, "%d", &channel); err != nil {
			t.Fatalf("invalid REOLINK_CHANNEL: %v", err)
		}
	}
	capabilities, err := client.Capabilities(ctx, channel)
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities.ReadOnly)+len(capabilities.ReadWrite) == 0 {
		t.Fatal("camera returned no capabilities")
	}
	t.Logf("channel=%d observed_channels=%v read_only=%d read_write=%d",
		channel, capabilities.ObservedChannels, len(capabilities.ReadOnly), len(capabilities.ReadWrite))
	if os.Getenv("REOLINK_TEST_CAPABILITIES") == "1" {
		t.Logf("read_only=%v", capabilities.ReadOnly)
		t.Logf("read_write=%v", capabilities.ReadWrite)
	}
}

func TestCameraDeviceInfo(t *testing.T) {
	cfg := cameraConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Dial(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	info, err := client.DeviceInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("type=%q model=%q hardware=%q firmware=%q",
		info.Type, info.Model, info.Hardware, info.Firmware)
}

func TestCameraTalkSilent(t *testing.T) {
	if os.Getenv("REOLINK_TEST_TALK") != "1" {
		t.Skip("silent camera talk test is not enabled")
	}
	cfg := cameraConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Dial(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	talk, err := client.StartTalk(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	format := talk.Format()
	block, err := (&ADPCMEncoder{}).EncodeBlock(make([]int16, format.SamplesPerBlock))
	if err != nil {
		t.Fatal(err)
	}
	if err = talk.WriteBlock(ctx, block); err != nil {
		t.Fatal(err)
	}
	if err = talk.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func cameraConfig(t *testing.T) Config {
	t.Helper()
	host := os.Getenv("REOLINK_HOST")
	username := os.Getenv("REOLINK_USER")
	password := os.Getenv("REOLINK_PASSWORD")
	if host == "" || username == "" || password == "" {
		t.Skip("camera credentials are not configured")
	}
	return NewConfig(host, username, password)
}

func readCameraProfile(ctx context.Context, cfg Config, channel uint8, stream Stream) error {
	client, err := Dial(ctx, cfg)
	if err != nil {
		return fmt.Errorf("channel %d %s: %w", channel, stream, err)
	}
	defer client.Close()
	preview, err := client.StartPreview(ctx, channel, stream)
	if err != nil {
		return fmt.Errorf("channel %d %s: %w", channel, stream, err)
	}
	defer preview.Close()
	var info, video, audio bool
	for i := 0; i < 128 && (!info || !video || !audio); i++ {
		packet, err := preview.Read(ctx)
		if err != nil {
			return fmt.Errorf("channel %d %s: %w", channel, stream, err)
		}
		switch packet.Kind {
		case MediaInfo:
			info = packet.Width > 0 && packet.Height > 0 && packet.FPS > 0
		case MediaVideoI, MediaVideoP:
			video = packet.Codec == "H264" || packet.Codec == "H265"
		case MediaAAC, MediaADPCM:
			audio = len(packet.Data) > 0
		}
	}
	if !info || !video || !audio {
		return fmt.Errorf("channel %d %s: missing media: info=%t video=%t audio=%t", channel, stream, info, video, audio)
	}
	return nil
}
