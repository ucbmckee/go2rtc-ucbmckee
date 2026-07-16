package reolink

import (
	"crypto/sha256"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/baichuan"
	"github.com/rs/zerolog"
)

func TestCameraRegistryProfiles(t *testing.T) {
	host := os.Getenv("REOLINK_HOST")
	username := os.Getenv("REOLINK_USER")
	password := os.Getenv("REOLINK_PASSWORD")
	if host == "" || username == "" || password == "" {
		t.Skip("camera credentials are not configured")
	}
	secret := sha256.Sum256([]byte("live camera test registry"))
	registry := newRegistry(zerolog.Nop(), secret, openBaichuan, sessionBudget{
		main: defaultMainSessions, other: defaultOtherSessions,
	})
	mainURL := cameraSourceURL(host, username, password, "main")
	main, err := registry.Dial(mainURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = main.Stop() })
	sub, err := registry.Dial(cameraSourceURL(host, username, password, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Stop() })
	if main.profile == sub.profile || main.profile.camera != sub.profile.camera {
		t.Fatal("unexpected live camera/profile ownership")
	}
	for _, producer := range []*Producer{main, sub} {
		requireCameraDiscovery(t, producer)
	}
	errs := make(chan error, 2)
	for _, producer := range []*Producer{main, sub} {
		go func() { errs <- producer.Start() }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for main.profile.videoFrames.Load() == 0 || sub.profile.videoFrames.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("live producers did not receive media")
		}
		time.Sleep(10 * time.Millisecond)
	}
	profiles := []*Profile{main.profile, sub.profile}
	for _, producer := range []*Producer{main, sub} {
		if err = producer.Stop(); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		<-errs
	}
	for _, profile := range profiles {
		select {
		case <-profile.done:
		case <-time.After(2 * baichuan.DefaultTimeout):
			t.Fatal("live profile did not stop")
		}
	}
}

func TestUIDCameraRegistryProfiles(t *testing.T) {
	uid := os.Getenv("REOLINK_UID")
	username := os.Getenv("REOLINK_USER")
	password := os.Getenv("REOLINK_PASSWORD")
	if uid == "" || username == "" || password == "" {
		t.Skip("UID camera credentials are not configured")
	}
	channels, _ := strconv.Atoi(os.Getenv("REOLINK_CHANNELS"))
	if channels == 0 {
		channels = 1
	}
	if channels < 1 || channels > 2 {
		t.Fatal("invalid REOLINK_CHANNELS")
	}
	secret := sha256.Sum256([]byte("live UID camera test registry"))
	registry := newRegistry(zerolog.Nop(), secret, openBaichuan, sessionBudget{
		main: defaultMainSessions, other: defaultOtherSessions,
	})
	producers := make([]*Producer, 0, channels*2)
	for channel := range channels {
		for _, stream := range []string{"main", "sub"} {
			producer, err := registry.Dial(uidCameraSourceURL(uid, username, password, channel, stream))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = producer.Stop() })
			producers = append(producers, producer)
			requireCameraDiscovery(t, producer)
		}
	}
	errs := make(chan error, len(producers))
	for _, producer := range producers {
		go func() { errs <- producer.Start() }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ready := true
		for _, producer := range producers {
			ready = ready && producer.profile.videoFrames.Load() != 0
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("UID producers did not receive media")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, producer := range producers {
		if err := producer.Stop(); err != nil {
			t.Fatal(err)
		}
	}
	for range producers {
		<-errs
	}
}

func requireCameraDiscovery(t *testing.T, producer *Producer) {
	t.Helper()
	diagnostics := producer.diagnostics()
	if diagnostics.DeviceInfo != "available" || diagnostics.CapabilityInfo != "available" ||
		diagnostics.CameraType == "" || len(diagnostics.Channels) == 0 {
		t.Fatalf("incomplete camera discovery: %+v", diagnostics)
	}
}

func cameraSourceURL(host, username, password, stream string) string {
	u := &url.URL{
		Scheme: "reolink", Host: host, User: url.UserPassword(username, password), Path: "/" + stream,
		RawQuery: "backchannel=0",
	}
	return u.String()
}

func uidCameraSourceURL(uid, username, password string, channel int, stream string) string {
	query := url.Values{
		"backchannel": {"0"}, "channel": {strconv.Itoa(channel)}, "transport": {"uid"},
	}
	if local := os.Getenv("REOLINK_UID_LOCAL"); local != "" {
		query.Set("local", local)
	}
	if broadcast := os.Getenv("REOLINK_UID_BROADCAST"); broadcast != "" {
		query.Set("broadcast", broadcast)
	}
	u := &url.URL{
		Scheme: "reolink", Host: uid, User: url.UserPassword(username, password),
		Path: "/" + stream, RawQuery: query.Encode(),
	}
	return u.String()
}
