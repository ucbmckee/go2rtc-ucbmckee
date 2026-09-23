package nest

import (
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	go2rtcwebrtc "github.com/AlexxIT/go2rtc/pkg/webrtc"
	pion "github.com/pion/webrtc/v4"
)

func TestRedactedSource(t *testing.T) {
	const secret = "nest:?client_secret=secret&refresh_token=token"
	if got := redactedSource(secret); got != "nest:" {
		t.Fatalf("wrong diagnostic source: %q", got)
	}
	if got := redactedSource("%zz"); got != "nest:" {
		t.Fatalf("malformed source wasn't redacted: %q", got)
	}
}

func newHandoffTestClient(t *testing.T) (*WebRTCClient, *core.Receiver, *core.Receiver, *h264Probe) {
	t.Helper()
	api, err := go2rtcwebrtc.NewAPI()
	if err != nil {
		t.Fatal(err)
	}
	pc, err := api.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	conn := go2rtcwebrtc.NewConn(pc)
	conn.Mode = core.ModeActiveProducer
	videoMedia := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	videoCodec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96}
	videoSource, err := conn.GetTrack(videoMedia, videoCodec)
	if err != nil {
		t.Fatal(err)
	}
	probe := newH264Probe(videoMedia, videoCodec, videoSource)
	audioMedia := &core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly}
	audioCodec := &core.Codec{Name: core.CodecOpus, ClockRate: 48000, Channels: 2, PayloadType: 111}
	audioSource, err := conn.GetTrack(audioMedia, audioCodec)
	if err != nil {
		t.Fatal(err)
	}
	client := &WebRTCClient{
		conn: conn, api: &API{StreamExpiresAt: time.Now().Add(time.Hour)}, done: make(chan struct{}),
	}
	client.watchPeer()
	client.h264.Store(probe)
	t.Cleanup(func() { _ = client.Stop() })
	return client, videoSource, audioSource, probe
}

func expectHandoffPacket(t *testing.T, delivered <-chan byte, want byte) {
	t.Helper()
	select {
	case got := <-delivered:
		if got != want {
			t.Fatalf("delivered payload: got=%d want=%d", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for payload %d", want)
	}
}

func TestTerminalStartPreservesAudioUntilReconnect(t *testing.T) {
	client, videoSource, audioSource, probe := newHandoffTestClient(t)
	videoDelivered := make(chan byte, 2)
	videoSender := core.NewSender(nil, probe.track.Codec.Clone())
	videoSender.Handler = func(packet *core.Packet) { videoDelivered <- packet.Payload[len(packet.Payload)-1] }
	videoSender.HandleRTP(probe.track)
	defer func() { videoSender.Close(); videoSender.Wait() }()
	audioDelivered := make(chan byte, 2)
	audioSender := core.NewSender(nil, audioSource.Codec.Clone())
	audioSender.Handler = func(packet *core.Packet) { audioDelivered <- packet.Payload[0] }
	audioSender.HandleRTP(audioSource)
	defer func() { audioSender.Close(); audioSender.Wait() }()
	videoSource.WriteRTP(testRTP(1, 90, 0x65, 1))
	audioSource.Input(&core.Packet{Payload: []byte{1}})
	expectHandoffPacket(t, videoDelivered, 1)
	expectHandoffPacket(t, audioDelivered, 1)
	if videoSender.Packets != 1 || audioSender.Packets != 1 {
		t.Fatal("initial video/audio senders did not receive packets")
	}

	finished := make(chan error, 1)
	go func() { finished <- client.Start() }()
	if err := client.conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Nest Start did not exit")
	}
	if audioSender.State() != "connected" {
		t.Fatal("audio sender was closed before reconnect")
	}

	// Match streams.Producer.reconnect: move receivers, then stop the old producer.
	next := &go2rtcwebrtc.Conn{Mode: core.ModeActiveProducer}
	nextVideoSource, _ := next.GetTrack(probe.media, probe.track.Codec.Clone())
	nextProbe := newH264Probe(probe.media, probe.track.Codec.Clone(), nextVideoSource)
	nextAudio, _ := next.GetTrack(audioSource.Media, audioSource.Codec.Clone())
	probe.track.Replace(nextProbe.track)
	audioSource.Replace(nextAudio)
	if err := client.Stop(); err != nil {
		t.Fatal(err)
	}
	nextVideoSource.WriteRTP(testRTP(1, 90, 0x65, 2))
	nextAudio.Input(&core.Packet{Payload: []byte{2}})
	expectHandoffPacket(t, videoDelivered, 2)
	expectHandoffPacket(t, audioDelivered, 2)
	if videoSender.Packets != 2 || audioSender.Packets != 2 {
		t.Fatalf("replacement video/audio packets: %d/%d", videoSender.Packets, audioSender.Packets)
	}
}

func TestDirectStopReleasesAudioAndTimer(t *testing.T) {
	client, _, audioSource, _ := newHandoffTestClient(t)
	sender := core.NewSender(nil, audioSource.Codec.Clone())
	sender.Handler = func(*core.Packet) {}
	sender.HandleRTP(audioSource)
	defer func() { sender.Close(); sender.Wait() }()
	finished := make(chan error, 1)
	go func() { finished <- client.Start() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		client.api.extendMu.Lock()
		active := client.api.extendTimer != nil
		client.api.extendMu.Unlock()
		if active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("extension timer did not start")
		}
		time.Sleep(time.Millisecond)
	}

	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := client.Stop(); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not exit after Stop")
	}
	if len(audioSource.Senders()) != 0 {
		t.Fatal("direct Stop retained an audio child")
	}
	audioSource.Input(&core.Packet{Payload: []byte{1}})
	if sender.Packets != 0 {
		t.Fatal("direct Stop still delivered audio")
	}
	client.api.extendMu.Lock()
	active := client.api.extendTimer != nil
	client.api.extendMu.Unlock()
	if active {
		t.Fatal("extension timer remains active")
	}
}

func TestStopBeforeStart(t *testing.T) {
	client, _, _, _ := newHandoffTestClient(t)
	if err := client.Stop(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- client.Start() }()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not exit after earlier Stop")
	}
	client.api.extendMu.Lock()
	active := client.api.extendTimer != nil
	client.api.extendMu.Unlock()
	if active {
		t.Fatal("Start restarted the extension timer")
	}
}

func TestPeerTerminalStates(t *testing.T) {
	for _, state := range []pion.PeerConnectionState{
		pion.PeerConnectionStateDisconnected,
		pion.PeerConnectionStateFailed,
		pion.PeerConnectionStateClosed,
	} {
		t.Run(state.String(), func(t *testing.T) {
			client, _, _, _ := newHandoffTestClient(t)
			client.conn.Fire(state)
			client.conn.Fire(state)
			finished := make(chan error, 1)
			go func() { finished <- client.Start() }()
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Start did not exit after terminal peer event")
			}
			if got := client.conn.Connection.Transport.(*pion.PeerConnection).ConnectionState(); got != pion.PeerConnectionStateClosed {
				t.Fatalf("peer connection state: %s", got)
			}
		})
	}
}

func TestWatchdogTerminationPreservesAudioUntilReconnect(t *testing.T) {
	client, _, audioSource, probe := newHandoffTestClient(t)
	delivered := make(chan byte, 2)
	sender := core.NewSender(nil, audioSource.Codec.Clone())
	sender.Handler = func(packet *core.Packet) { delivered <- packet.Payload[0] }
	sender.HandleRTP(audioSource)
	defer func() { sender.Close(); sender.Wait() }()
	audioSource.Input(&core.Packet{Payload: []byte{1}})
	expectHandoffPacket(t, delivered, 1)
	probe.beginHandoff()

	finished := make(chan error, 1)
	go func() { finished <- client.Start() }()
	ticks := make(chan time.Time)
	monitorDone := make(chan struct{})
	go func() { client.monitorVideoTicks(ticks); close(monitorDone) }()
	now := time.Now()
	ticks <- now
	ticks <- now.Add(videoWatchdogRestart)
	select {
	case <-monitorDone:
	case <-time.After(5 * time.Second):
		t.Fatal("video watchdog did not terminate")
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not exit after watchdog termination")
	}
	if probe.watchdogRestarts.Load() != 1 || len(audioSource.Senders()) != 1 {
		t.Fatalf("watchdog restart=%d, retained audio children=%d", probe.watchdogRestarts.Load(), len(audioSource.Senders()))
	}
	nextAudio := core.NewReceiver(audioSource.Media, audioSource.Codec.Clone())
	audioSource.Replace(nextAudio)
	if err := client.Stop(); err != nil {
		t.Fatal(err)
	}
	nextAudio.Input(&core.Packet{Payload: []byte{2}})
	expectHandoffPacket(t, delivered, 2)
	if sender.Packets != 2 {
		t.Fatalf("watchdog replacement did not receive audio: %d packets", sender.Packets)
	}
}
