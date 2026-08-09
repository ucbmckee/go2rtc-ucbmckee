package nest

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
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
	probe.beginHandoff()

	want := testRTP(7, 90, 0x65, 1, 2)
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

func TestH264ProbeReadinessRequiresPostHandoffSequence(t *testing.T) {
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	codec := &core.Codec{Name: core.CodecH264}
	probe := newH264Probe(media, codec, core.NewReceiver(media, codec))

	probe.inspect(testRTP(1, 90, 0x67, 1))
	probe.inspect(testRTP(2, 90, 0x68, 1))
	probe.inspect(testRTP(3, 90, 0x65, 1))
	if !probe.readyBeforeHandoff.Load() {
		t.Fatal("pre-handoff SPS/PPS/IDR was not recorded")
	}

	probe.beginHandoff()
	probe.inspect(testRTP(4, 180, 0x65, 1))
	if probe.readyAfterHandoff.Load() != 0 {
		t.Fatal("pre-handoff parameter sets satisfied post-handoff readiness")
	}
	probe.inspect(testRTP(5, 180, 0x67, 1))
	probe.inspect(testRTP(6, 180, 0x68, 1))
	probe.inspect(testRTP(7, 180, 0x65, 1))
	if probe.readyAfterHandoff.Load() == 0 {
		t.Fatal("post-handoff SPS/PPS/IDR was not recorded")
	}
}

func TestH264ProbeReceiverHandoff(t *testing.T) {
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	codec := &core.Codec{Name: core.CodecH264}
	oldSource := core.NewReceiver(media, codec)
	newSource := core.NewReceiver(media, codec)
	oldProbe := newH264Probe(media, codec, oldSource)
	newProbe := newH264Probe(media, codec, newSource)

	var got *rtp.Packet
	oldProbe.track.AppendChild(&core.Node{Input: func(packet *rtp.Packet) { got = packet }})
	oldProbe.track.Replace(newProbe.track)
	want := testRTP(1, 90, 0x65, 1)
	newSource.WriteRTP(want)
	if got != want {
		t.Fatal("replacement receiver did not inherit the existing consumer")
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
	client := &WebRTCClient{
		conn: conn, done: make(chan struct{}),
		pliDelays: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
	}
	var calls atomic.Int32
	requested := make(chan struct{}, 3)
	client.requestPLI = func(*core.Media) error {
		calls.Add(1)
		requested <- struct{}{}
		return nil
	}
	if err := client.prepareH264Probe(); err != nil {
		t.Fatal(err)
	}

	probe := client.h264.Load()
	track, err := client.GetTrack(media, media.Codecs[0])
	if err != nil || track != probe.track || track.Codec != media.Codecs[0] {
		t.Fatalf("wrong probe track: track=%p err=%v", track, err)
	}
	for i := 0; i < 3; i++ {
		select {
		case <-requested:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for PLI sequence")
		}
	}
	if probe.plis.Load() != 3 || calls.Load() != 3 || !probe.handoffStarted.Load() {
		t.Fatalf("wrong handoff state: plis=%d calls=%d started=%t", probe.plis.Load(), calls.Load(), probe.handoffStarted.Load())
	}
	close(client.done)
}

func TestWebRTCClientPreparesNegotiatedH264Profiles(t *testing.T) {
	media := &core.Media{
		Kind: core.KindVideo, Direction: core.DirectionRecvonly,
		Codecs: []*core.Codec{
			{Name: core.CodecH264, ClockRate: 90000, FmtpLine: "profile-level-id=42e01f", PayloadType: 96},
			{Name: core.CodecH264, ClockRate: 90000, FmtpLine: "profile-level-id=64001f", PayloadType: 98},
		},
	}
	conn := &go2rtcwebrtc.Conn{
		Connection: core.Connection{Medias: []*core.Media{media}},
		Mode:       core.ModeActiveProducer,
	}
	client := &WebRTCClient{conn: conn, done: make(chan struct{})}
	if err := client.prepareH264Probe(); err != nil {
		t.Fatal(err)
	}
	if len(conn.Receivers) != 2 || len(client.h264.Load().sources) != 2 {
		t.Fatal("H264 profiles did not get independent source receivers")
	}
	selected, err := conn.GetTrack(media, &core.Codec{
		Name: core.CodecH264, ClockRate: 90000, FmtpLine: "profile-level-id=64001f", PayloadType: 98,
	})
	if err != nil {
		t.Fatal(err)
	}
	if selected != conn.Receivers[1] || len(conn.Receivers) != 2 {
		t.Fatal("negotiated H264 profile did not reuse its source receiver")
	}

	var got *rtp.Packet
	client.h264.Load().track.AppendChild(&core.Node{Input: func(packet *rtp.Packet) { got = packet }})
	want := testRTP(1, 90, 0x65, 1)
	selected.WriteRTP(want)
	if got != want || client.h264.Load().packets.Load() != 1 {
		t.Fatal("negotiated H264 profile did not feed the probe")
	}
	if err = conn.Stop(); err != nil {
		t.Fatal(err)
	}
	selected.WriteRTP(testRTP(2, 90, 0x65, 1))
	if client.h264.Load().packets.Load() != 1 {
		t.Fatal("closed H264 source still fed the probe")
	}
}

func TestWebRTCClientMatchesWildcardProbe(t *testing.T) {
	media := &core.Media{
		Kind: core.KindVideo, Direction: core.DirectionRecvonly,
		Codecs: []*core.Codec{{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96}},
	}
	conn := &go2rtcwebrtc.Conn{Connection: core.Connection{Medias: []*core.Media{media}}, Mode: core.ModeActiveProducer}
	client := &WebRTCClient{conn: conn, done: make(chan struct{}), pliDelays: []time.Duration{time.Hour}}
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

func TestVideoWatchdogThresholds(t *testing.T) {
	tests := []struct {
		name string
		idle time.Duration
		pli  time.Duration
		want videoWatchdogAction
	}{
		{name: "healthy", idle: videoWatchdogPLIAfter - time.Nanosecond},
		{name: "request", idle: videoWatchdogPLIAfter, want: videoWatchdogPLI},
		{name: "rate limited", idle: videoWatchdogPLIAfter, pli: videoWatchdogPLISpacing - time.Nanosecond},
		{name: "restart", idle: videoWatchdogRestart, want: videoWatchdogRestartAction},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := videoWatchdogDecision(tt.idle, tt.pli); got != tt.want {
				t.Fatalf("watchdog decision mismatch: want=%d got=%d", tt.want, got)
			}
		})
	}
}

func TestVideoWatchdogActions(t *testing.T) {
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	probe := newH264Probe(media, &core.Codec{Name: core.CodecH264}, core.NewReceiver(media, &core.Codec{Name: core.CodecH264}))
	calls := 0
	client := &WebRTCClient{requestPLI: func(*core.Media) error { calls++; return nil }}
	if client.checkVideoWatchdog(probe, videoWatchdogPLIAfter, 0) {
		t.Fatal("PLI should not restart the producer")
	}
	if calls != 1 || probe.watchdogPLIs.Load() != 1 || probe.plis.Load() != 1 {
		t.Fatalf("PLI action not recorded: calls=%d watchdog=%d total=%d", calls, probe.watchdogPLIs.Load(), probe.plis.Load())
	}
	if client.checkVideoWatchdog(probe, videoWatchdogPLIAfter, videoWatchdogPLISpacing-time.Nanosecond) {
		t.Fatal("rate-limited PLI should not restart the producer")
	}
	if calls != 1 {
		t.Fatalf("rate-limited PLI was sent: %d", calls)
	}
	if !client.checkVideoWatchdog(probe, videoWatchdogRestart, 0) || probe.watchdogRestarts.Load() != 1 {
		t.Fatal("restart action was not recorded")
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
	client := &WebRTCClient{conn: go2rtcwebrtc.NewConn(pc), api: &API{}, done: make(chan struct{})}

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

func TestH264RTPParserFragmentedNALU(t *testing.T) {
	var parser h264RTPParser
	var types []byte
	inspect := func(typ byte) { types = append(types, typ) }
	if !parser.eachNALU(testRTP(10, 90, 28, 0x80|h264.NALUTypeIFrame, 1), inspect) ||
		!parser.eachNALU(testRTP(11, 90, 28, h264.NALUTypeIFrame, 2), inspect) ||
		!parser.eachNALU(testRTP(12, 90, 28, 0x40|h264.NALUTypeIFrame, 3), inspect) {
		t.Fatal("valid FU-A rejected")
	}
	if !bytes.Equal(types, []byte{h264.NALUTypeIFrame}) {
		t.Fatalf("fragmented IDR reported before completion: %v", types)
	}

	tests := []struct {
		name  string
		start *rtp.Packet
		end   *rtp.Packet
	}{
		{name: "sequence gap", start: testRTP(20, 90, 28, 0x80|5, 1), end: testRTP(22, 90, 28, 0x40|5, 2)},
		{name: "timestamp change", start: testRTP(20, 90, 28, 0x80|5, 1), end: testRTP(21, 91, 28, 0x40|5, 2)},
		{name: "type change", start: testRTP(20, 90, 28, 0x80|5, 1), end: testRTP(21, 90, 28, 0x40|1, 2)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p h264RTPParser
			called := false
			if !p.eachNALU(tt.start, func(byte) { called = true }) {
				t.Fatal("valid FU-A start rejected")
			}
			if p.eachNALU(tt.end, func(byte) { called = true }) || called {
				t.Fatal("invalid FU-A completed a NALU")
			}
		})
	}
}

func TestH264RTPParserRepeatedStart(t *testing.T) {
	var parser h264RTPParser
	var types []byte
	inspect := func(typ byte) { types = append(types, typ) }
	if !parser.eachNALU(testRTP(1, 90, 28, 0x80|5, 1), inspect) {
		t.Fatal("first FU-A start rejected")
	}
	if parser.eachNALU(testRTP(2, 90, 28, 0x80|5, 2), inspect) {
		t.Fatal("repeated FU-A start did not report the abandoned fragment")
	}
	if !parser.eachNALU(testRTP(3, 90, 28, 0x40|5, 3), inspect) || !bytes.Equal(types, []byte{5}) {
		t.Fatal("replacement FU-A did not complete normally")
	}
}

func TestH264RTPParserAllowsEmptyFragments(t *testing.T) {
	var parser h264RTPParser
	var types []byte
	inspect := func(typ byte) { types = append(types, typ) }
	if !parser.eachNALU(testRTP(1, 90, 28, 0x80|5), inspect) ||
		!parser.eachNALU(testRTP(2, 90, 28, 5, 1), inspect) ||
		!parser.eachNALU(testRTP(3, 90, 28, 0x40|5), inspect) ||
		!bytes.Equal(types, []byte{5}) {
		t.Fatal("empty FU-A fragments broke a valid sequence")
	}

	types = nil
	if !parser.eachNALU(testRTP(4, 180, 28, 0x80|5), inspect) ||
		!parser.eachNALU(testRTP(5, 180, 28, 0x40|5), inspect) || len(types) != 0 {
		t.Fatal("empty FU-A sequence produced a decoder-ready NALU")
	}
}

func TestH264RTPParserSTAPAIsAtomic(t *testing.T) {
	valid := testRTP(1, 90, 24, 0, 1, 0x67, 0, 1, 0x68, 0, 1, 0x65)
	var parser h264RTPParser
	var types []byte
	if !parser.eachNALU(valid, func(typ byte) { types = append(types, typ) }) {
		t.Fatal("valid STAP-A rejected")
	}
	want := []byte{h264.NALUTypeSPS, h264.NALUTypePPS, h264.NALUTypeIFrame}
	if !bytes.Equal(types, want) {
		t.Fatalf("wrong STAP-A types: %v", types)
	}

	types = nil
	truncated := testRTP(2, 90, 24, 0, 1, 0x67, 0, 2, 0x68)
	if parser.eachNALU(truncated, func(typ byte) { types = append(types, typ) }) || len(types) != 0 {
		t.Fatalf("truncated STAP-A produced partial results: %v", types)
	}
}

func FuzzH264RTPParser(f *testing.F) {
	f.Add([]byte{0x65, 1})
	f.Add([]byte{24, 0, 1, 0x67})
	f.Add([]byte{28, 0x80 | 5, 1})
	f.Fuzz(func(t *testing.T, payload []byte) {
		var parser h264RTPParser
		parser.eachNALU(&rtp.Packet{Payload: payload}, func(typ byte) {
			if typ == 0 || typ >= 24 {
				t.Fatalf("invalid NALU type: %d", typ)
			}
		})
	})
}

func BenchmarkH264ProbeInspect(b *testing.B) {
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	codec := &core.Codec{Name: core.CodecH264}
	probe := newH264Probe(media, codec, core.NewReceiver(media, codec))
	packet := testRTP(1, 90, 0x65, 1)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		probe.inspect(packet)
	}
}

func testRTP(sequence uint16, timestamp uint32, payload ...byte) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: sequence, Timestamp: timestamp}, Payload: payload}
}

var errTestPLI = errors.New("test PLI failure")
