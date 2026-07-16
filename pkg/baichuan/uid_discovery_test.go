package baichuan

import (
	"context"
	"encoding/xml"
	"errors"
	"net"
	"net/netip"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestUIDDiscoveryCamera(t *testing.T) {
	uid := os.Getenv("REOLINK_UID")
	if uid == "" {
		t.Skip("REOLINK_UID is not set")
	}
	discovery, err := discoverUID(context.Background(), uid, os.Getenv("REOLINK_UID_LOCAL"),
		os.Getenv("REOLINK_UID_BROADCAST"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = discovery.conn.Close() })
	if !discovery.remote.Addr().IsPrivate() || discovery.remote.Port() == 0 ||
		discovery.clientID == 0 || discovery.cameraID == 0 {
		t.Fatalf("invalid redacted discovery result")
	}
}

func TestUIDLoginCamera(t *testing.T) {
	uid := os.Getenv("REOLINK_UID")
	username := os.Getenv("REOLINK_USER")
	password := os.Getenv("REOLINK_PASSWORD")
	if uid == "" || username == "" || password == "" {
		t.Skip("UID camera credentials are not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Dial(ctx, uidCameraConfig(uid, username, password))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err = client.Login(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestUIDPreviewCamera(t *testing.T) {
	uid := os.Getenv("REOLINK_UID")
	username := os.Getenv("REOLINK_USER")
	password := os.Getenv("REOLINK_PASSWORD")
	if uid == "" || username == "" || password == "" {
		t.Skip("UID camera credentials are not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Dial(ctx, uidCameraConfig(uid, username, password))
	if err != nil {
		t.Fatal(err)
	}
	preview, err := client.StartPreview(ctx, 0, StreamMain)
	if err != nil {
		t.Fatal(errors.Join(err, client.Close()))
	}
	_, readErr := preview.Read(ctx)
	closeErr := errors.Join(preview.Close(), client.Close())
	if err = errors.Join(readErr, closeErr); err != nil {
		t.Fatal(err)
	}
}

func TestUIDCameraProfiles(t *testing.T) {
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
	if channels < 1 || channels > 16 {
		t.Fatal("invalid REOLINK_CHANNELS")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errs := make(chan error, channels*2)
	for channel := range channels {
		for _, stream := range []Stream{StreamMain, StreamSub} {
			go func() {
				errs <- readCameraProfile(ctx, uidCameraConfig(uid, username, password), uint8(channel), stream)
			}()
		}
	}
	for range channels * 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestUIDCameraTalkSilent(t *testing.T) {
	if os.Getenv("REOLINK_TEST_TALK") != "1" {
		t.Skip("silent UID camera talk test is not enabled")
	}
	uid := os.Getenv("REOLINK_UID")
	username := os.Getenv("REOLINK_USER")
	password := os.Getenv("REOLINK_PASSWORD")
	if uid == "" || username == "" || password == "" {
		t.Skip("UID camera credentials are not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Dial(ctx, uidCameraConfig(uid, username, password))
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

func TestUIDCameraSustainedProfiles(t *testing.T) {
	seconds, _ := strconv.Atoi(os.Getenv("REOLINK_UID_SOAK_SECONDS"))
	if seconds == 0 {
		t.Skip("UID camera soak is not enabled")
	}
	if seconds < 1 || seconds > 300 {
		t.Fatal("invalid REOLINK_UID_SOAK_SECONDS")
	}
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
	if channels < 1 || channels > 16 {
		t.Fatal("invalid REOLINK_CHANNELS")
	}
	streams := []Stream{StreamMain, StreamSub}
	switch os.Getenv("REOLINK_UID_STREAM") {
	case "":
	case "main":
		streams = streams[:1]
	case "sub":
		streams = streams[1:]
	default:
		t.Fatal("invalid REOLINK_UID_STREAM")
	}
	type result struct {
		channel uint8
		stream  Stream
		packets int
		stats   TransportStats
		err     error
	}
	results := make(chan result, channels*len(streams))
	for channel := range channels {
		for _, stream := range streams {
			go func() {
				packets, stats, err := readUIDCameraFor(
					uidCameraConfig(uid, username, password), uint8(channel), stream, time.Duration(seconds)*time.Second,
				)
				results <- result{uint8(channel), stream, packets, stats, err}
			}()
		}
	}
	for range channels * len(streams) {
		result := <-results
		if result.err != nil || result.packets == 0 || result.stats.ReceivedPackets == 0 {
			t.Errorf("channel %d %s: packets=%d stats=%+v err=%v",
				result.channel, result.stream, result.packets, result.stats, result.err)
			continue
		}
		t.Logf("channel %d %s: media=%d transport=%+v",
			result.channel, result.stream, result.packets, result.stats)
	}
}

func uidCameraConfig(uid, username, password string) Config {
	cfg := NewUIDConfig(uid, username, password)
	cfg.UIDLocalAddr = os.Getenv("REOLINK_UID_LOCAL")
	cfg.UIDBroadcastAddr = os.Getenv("REOLINK_UID_BROADCAST")
	return cfg
}

func readUIDCameraFor(cfg Config, channel uint8, stream Stream, duration time.Duration) (int, TransportStats, error) {
	ctx, cancel := context.WithTimeout(context.Background(), duration+10*time.Second)
	defer cancel()
	client, err := Dial(ctx, cfg)
	if err != nil {
		return 0, TransportStats{}, err
	}
	preview, err := client.StartPreview(ctx, channel, stream)
	if err != nil {
		_ = client.Close()
		return 0, TransportStats{}, err
	}
	readCtx, stopRead := context.WithTimeout(ctx, duration)
	packets := 0
	for {
		if _, err = preview.Read(readCtx); err != nil {
			break
		}
		packets++
	}
	stopRead()
	stats := client.TransportStats()
	closeErr := errors.Join(preview.Close(), client.Close())
	if !errors.Is(err, context.DeadlineExceeded) {
		return packets, stats, errors.Join(err, closeErr)
	}
	return packets, stats, closeErr
}

func TestUIDDiscoveryRejectsInvalidConfig(t *testing.T) {
	for _, uid := range []string{"", "bad uid", string(make([]byte, 65))} {
		if _, err := discoverUID(context.Background(), uid, "", "", time.Second); err == nil {
			t.Fatalf("accepted UID of length %d", len(uid))
		}
	}
	if _, err := discoverUID(context.Background(), "valid", "", "", 0); err == nil {
		t.Fatal("accepted zero timeout")
	}
	if _, err := discoverUID(context.Background(), "valid", "not-an-address", "", time.Second); err == nil {
		t.Fatal("accepted invalid local address")
	}
	if _, err := discoverUID(context.Background(), "valid", "", "127.0.0.1", time.Second); err == nil {
		t.Fatal("accepted invalid broadcast address")
	}
}

func TestUIDBroadcastSocketOption(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = enableUIDBroadcast(conn); err != nil {
		t.Fatal(err)
	}
}

func TestUIDBroadcastSelectionRejectsInactiveAddress(t *testing.T) {
	if _, err := uidBroadcasts(netip.MustParseAddr("192.0.2.1"), netip.Addr{}); err == nil {
		t.Fatal("accepted inactive UID local address")
	}
}

func TestUIDRoutedBroadcastOverride(t *testing.T) {
	local := netip.MustParseAddr("192.0.2.211")
	want := netip.MustParseAddr("198.51.100.255")
	broadcasts, err := uidBroadcasts(local, want)
	if err != nil || len(broadcasts) != 1 || broadcasts[0] != want {
		t.Fatalf("unexpected UID broadcast override: %v %v", broadcasts, err)
	}
}

func TestUIDDiscoveryResponseMatchesTransaction(t *testing.T) {
	const clientID int32 = 71
	const transaction uint32 = 29
	payload, err := xml.Marshal(uidEnvelope{Response: &uidResponse{CID: clientID, DID: 83, Rsp: 0}})
	if err != nil {
		t.Fatal(err)
	}
	payload = append([]byte(xml.Header), payload...)
	xorUID(payload, payload, transaction)
	packet, err := marshalUIDDiscovery(transaction, payload)
	if err != nil {
		t.Fatal(err)
	}
	remote := netip.MustParseAddrPort("203.0.113.67:2015")
	if _, ok := parseUIDResponse(append([]byte(nil), packet...), remote, clientID, transaction+1); ok {
		t.Fatal("accepted UID discovery response for another transaction")
	}
	if _, ok := parseUIDResponse(append([]byte(nil), packet...), remote, clientID+1, transaction); ok {
		t.Fatal("accepted UID discovery response for another client")
	}
	if cameraID, ok := parseUIDResponse(packet, remote, clientID, transaction); !ok || cameraID != 83 {
		t.Fatalf("valid UID response = %d, %t", cameraID, ok)
	}
}
