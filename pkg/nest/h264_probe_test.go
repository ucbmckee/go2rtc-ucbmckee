package nest

import (
	"bytes"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	go2rtcwebrtc "github.com/AlexxIT/go2rtc/pkg/webrtc"
	"github.com/pion/rtp"
)

func TestH264ProbePassesRTPUnchanged(t *testing.T) {
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	codec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96}
	source := core.NewReceiver(media, codec)
	probe := newH264Probe(media, codec, source)
	probe.requested.Store(true)

	want := &rtp.Packet{Header: rtp.Header{Version: 2, Marker: true, SequenceNumber: 7}, Payload: []byte{0x65, 1, 2}}
	var got *rtp.Packet
	probe.track.AppendChild(&core.Node{Input: func(packet *rtp.Packet) { got = packet }})
	source.WriteRTP(want)

	if got != want || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatal("probe changed RTP packet")
	}
	if probe.idrs.Load() != 1 || probe.idrsAfterRequest.Load() != 1 {
		t.Fatalf("wrong IDR counters: %d/%d", probe.idrs.Load(), probe.idrsAfterRequest.Load())
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
	if probe.plis.Load() != 3 {
		t.Fatalf("wrong PLI count: %d", probe.plis.Load())
	}
	close(client.done)
	client.requestKeyframesAfter(probe, []time.Duration{time.Hour})
	if probe.plis.Load() != 3 {
		t.Fatalf("PLI sent after cancellation: %d", probe.plis.Load())
	}
}

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
