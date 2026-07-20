package nest

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	go2rtcwebrtc "github.com/AlexxIT/go2rtc/pkg/webrtc"
	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
)

func TestH264ProbePassesRTPUnchanged(t *testing.T) {
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	codec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96}
	source := core.NewReceiver(media, codec)
	probe := newH264Probe(media, codec, source)
	probe.handoffStarted.Store(true)

	want := &rtp.Packet{Header: rtp.Header{Version: 2, Marker: true, SequenceNumber: 7}, Payload: []byte{0x65, 1, 2}}
	var got *rtp.Packet
	probe.track.AppendChild(&core.Node{Input: func(packet *rtp.Packet) { got = packet }})
	source.WriteRTP(want)

	if got != want || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatal("probe changed RTP packet")
	}
	if probe.idrs.Load() != 1 || probe.idrsAfterHandoff.Load() != 1 {
		t.Fatalf("wrong IDR counters: %d/%d", probe.idrs.Load(), probe.idrsAfterHandoff.Load())
	}
}

func TestWebRTCClientProbeTrackAndPLI(t *testing.T) {
	media := &core.Media{
		Kind: core.KindVideo, Direction: core.DirectionRecvonly,
		Codecs: []*core.Codec{{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96}},
	}
	conn := &go2rtcwebrtc.Conn{
		Connection: core.Connection{Medias: []*core.Media{media}},
		Mode:       core.ModeActiveProducer,
	}
	client := &WebRTCClient{conn: conn, done: make(chan struct{})}
	var pliCalls int
	client.requestPLI = func(*core.Media) error {
		pliCalls++
		return nil
	}
	if err := client.prepareH264Probe(); err != nil {
		t.Fatal(err)
	}
	probe := client.h264.Load()
	client.pliOnce.Do(func() {})
	track, err := client.GetTrack(media, media.Codecs[0])
	if err != nil || track != probe.track || track.Codec != media.Codecs[0] {
		t.Fatalf("wrong probe track: track=%p err=%v", track, err)
	}

	client.requestKeyframesAfter(probe, []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond})
	if probe.plis.Load() != 3 || pliCalls != 3 {
		t.Fatalf("wrong PLI count: %d", probe.plis.Load())
	}
	close(client.done)
	client.requestKeyframesAfter(probe, []time.Duration{time.Hour})
	if probe.plis.Load() != 3 {
		t.Fatalf("PLI sent after cancellation: %d", probe.plis.Load())
	}
}

func TestWebRTCClientMatchesWildcardProbe(t *testing.T) {
	media := &core.Media{
		Kind: core.KindVideo, Direction: core.DirectionRecvonly,
		Codecs: []*core.Codec{{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96}},
	}
	conn := &go2rtcwebrtc.Conn{Connection: core.Connection{Medias: []*core.Media{media}}, Mode: core.ModeActiveProducer}
	client := &WebRTCClient{conn: conn, done: make(chan struct{})}
	if err := client.prepareH264Probe(); err != nil {
		t.Fatal(err)
	}

	track, err := client.GetTrack(media, &core.Codec{Name: core.CodecAny})
	if err != nil || track != client.h264.Load().track {
		t.Fatalf("wildcard did not reuse probe: track=%p err=%v", track, err)
	}
	if !client.h264.Load().handoffStarted.Load() {
		t.Fatal("wildcard request did not start handoff")
	}
	otherMedia := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly, ID: "other"}
	other, err := client.GetTrack(otherMedia, &core.Codec{Name: core.CodecH264})
	if err != nil || other == client.h264.Load().track {
		t.Fatalf("different media incorrectly reused probe: track=%p err=%v", other, err)
	}
	close(client.done)
}

func TestWebRTCClientContinuesAfterPLIError(t *testing.T) {
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	probe := newH264Probe(media, &core.Codec{Name: core.CodecH264}, core.NewReceiver(media, &core.Codec{Name: core.CodecH264}))
	var calls int
	client := &WebRTCClient{done: make(chan struct{}), requestPLI: func(*core.Media) error {
		calls++
		if calls == 1 {
			return errTestPLI
		}
		return nil
	}}
	client.requestKeyframesAfter(probe, []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond})
	if probe.plis.Load() != 2 || probe.pliErrors.Load() != 1 || calls != 3 {
		t.Fatalf("PLI retry sequence was not preserved: sent=%d errors=%d calls=%d", probe.plis.Load(), probe.pliErrors.Load(), calls)
	}
}

func TestWebRTCClientMarshalIncludesProbeDiagnostics(t *testing.T) {
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	codec := &core.Codec{Name: core.CodecH264}
	conn := &go2rtcwebrtc.Conn{Connection: core.Connection{Medias: []*core.Media{media}}, Mode: core.ModeActiveProducer}
	client := &WebRTCClient{conn: conn, done: make(chan struct{})}
	client.h264.Store(newH264Probe(media, codec, core.NewReceiver(media, codec)))

	data, err := client.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["nest"]; !ok {
		t.Fatal("probe diagnostics missing from connection JSON")
	}
}

func TestWebRTCClientStopConcurrent(t *testing.T) {
	rtcAPI, err := go2rtcwebrtc.NewAPI()
	if err != nil {
		t.Fatal(err)
	}
	pc, err := rtcAPI.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	client := &WebRTCClient{
		conn: go2rtcwebrtc.NewConn(pc), api: &API{}, done: make(chan struct{}),
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = client.Stop()
		}()
	}
	wg.Wait()
}

var errTestPLI = errors.New("test PLI failure")

func TestEachRTPNALU(t *testing.T) {
	stap := []byte{24, 0, 1, 0x67, 0, 1, 0x68, 0, 1, 0x65}
	var types []byte
	if !eachRTPNALU(stap, func(typ byte) { types = append(types, typ) }) {
		t.Fatal("valid STAP-A rejected")
	}
	if want := []byte{h264.NALUTypeSPS, h264.NALUTypePPS, h264.NALUTypeIFrame}; !bytes.Equal(types, want) {
		t.Fatalf("wrong STAP-A types: %v", types)
	}

	types = nil
	if !eachRTPNALU([]byte{28, 0x80 | h264.NALUTypeIFrame}, func(typ byte) { types = append(types, typ) }) ||
		!bytes.Equal(types, []byte{h264.NALUTypeIFrame}) {
		t.Fatalf("wrong FU-A types: %v", types)
	}
	if eachRTPNALU([]byte{24, 0, 2, 0x67}, func(byte) {}) {
		t.Fatal("truncated STAP-A accepted")
	}
}
