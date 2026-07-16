package baichuan

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestConfigRedaction(t *testing.T) {
	cfg := NewConfig("camera.local", "secret-user", "secret-password")
	values := []string{
		fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg),
		fmt.Sprintf("%s", cfg), fmt.Sprintf("%q", cfg),
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	values = append(values, string(data))
	for _, value := range values {
		if strings.Contains(value, "secret-user") || strings.Contains(value, "secret-password") {
			t.Fatalf("credential leaked: %s", value)
		}
	}
}

func TestUIDConfigRedaction(t *testing.T) {
	const uid = "SentinelCameraUID1"
	cfg := NewUIDConfig(uid, "secret-user", "secret-password")
	cfg.UIDLocalAddr = "192.0.2.28"
	cfg.UIDBroadcastAddr = "198.51.100.255"
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg), string(data),
	} {
		if strings.Contains(value, uid) || strings.Contains(value, "secret-user") ||
			strings.Contains(value, "secret-password") {
			t.Fatalf("UID config leaked: %s", value)
		}
	}
	if _, err = cfg.normalized(); err != nil {
		t.Fatal(err)
	}
}

func TestUIDLocalAddress(t *testing.T) {
	cfg := NewUIDConfig("CameraUID1", "user", "password")
	cfg.UIDLocalAddr = " 192.0.2.28 "
	normalized, err := cfg.normalized()
	if err != nil || normalized.UIDLocalAddr != "192.0.2.28" {
		t.Fatalf("unexpected local address: %q %v", normalized.UIDLocalAddr, err)
	}
	for _, value := range []string{"loopback", "127.0.0.1", "::1"} {
		cfg.UIDLocalAddr = value
		if _, err = cfg.normalized(); err == nil {
			t.Fatalf("accepted invalid UID local address %q", value)
		}
	}
	tcp := NewConfig("camera", "user", "password")
	tcp.UIDLocalAddr = "192.0.2.28"
	if _, err = tcp.normalized(); err == nil {
		t.Fatal("accepted UID local address for TCP transport")
	}
}

func TestUIDBroadcastAddress(t *testing.T) {
	cfg := NewUIDConfig("CameraUID1", "user", "password")
	cfg.UIDBroadcastAddr = " 198.51.100.255 "
	normalized, err := cfg.normalized()
	if err != nil || normalized.UIDBroadcastAddr != "198.51.100.255" {
		t.Fatalf("unexpected broadcast address: %q %v", normalized.UIDBroadcastAddr, err)
	}
	for _, value := range []string{"broadcast", "0.0.0.0", "127.0.0.1", "224.0.0.1", "::1"} {
		cfg.UIDBroadcastAddr = value
		if _, err = cfg.normalized(); err == nil {
			t.Fatalf("accepted invalid UID broadcast address %q", value)
		}
	}
	tcp := NewConfig("camera", "user", "password")
	tcp.UIDBroadcastAddr = "198.51.100.255"
	if _, err = tcp.normalized(); err == nil {
		t.Fatal("accepted UID broadcast address for TCP transport")
	}
}

func TestAggregateFormattingRedactsCredentials(t *testing.T) {
	cfg := NewConfig("camera.local", "sentinel-user", "sentinel-pass")
	client := &Client{cfg: cfg}
	preview := &Preview{client: client, key: previewKey{channel: 2}, stream: StreamMain}
	talk := &Talk{
		client: client, channel: 2,
		format: TalkFormat{SampleRate: 16000, SamplePrecision: 16, SamplesPerBlock: 1016},
	}
	for _, value := range []any{client, preview, talk} {
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
			text := fmt.Sprintf(format, value)
			if strings.Contains(text, "sentinel-user") || strings.Contains(text, "sentinel-pass") {
				t.Fatalf("credential leaked from %T with %s: %s", value, format, text)
			}
		}
	}
}

func TestLimits(t *testing.T) {
	limits, err := (Limits{}).normalized()
	if err != nil {
		t.Fatal(err)
	}
	if limits.MaxBody != defaultMaxBody || limits.MaxPending != defaultMaxPending {
		t.Fatalf("unexpected defaults: %+v", limits)
	}
	if _, err = (Limits{MaxBody: 65 << 20}).normalized(); err == nil {
		t.Fatal("expected hard ceiling error")
	}
	if _, err = (Limits{MaxBody: 1024, MaxExtension: 2048}).normalized(); err == nil {
		t.Fatal("expected inconsistent limits error")
	}
}
