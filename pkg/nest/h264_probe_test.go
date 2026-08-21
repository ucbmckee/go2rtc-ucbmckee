package nest

import (
	"bytes"
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

func TestH264ProbeGatesUntilDecoderReadyAndPassesRTPUnchanged(t *testing.T) {
	probe, source := newTestH264Probe()
	probe.beginHandoff()

	var got []*rtp.Packet
	probe.track.AppendChild(&core.Node{Input: func(packet *rtp.Packet) { got = append(got, packet) }})
	source.WriteRTP(testRTP(1, 90, 0x61, 1))
	want := []*rtp.Packet{
		testRTP(2, 180, 0x67, 1),
		testRTP(3, 180, 0x68, 1),
		testRTP(4, 180, 0x65, 1, 2),
		testRTP(5, 270, 0x61, 1),
	}
	for _, packet := range want {
		source.WriteRTP(packet)
	}
	if len(got) != len(want) {
		t.Fatalf("wrong forwarded packet count: want=%d got=%d", len(want), len(got))
	}
	for i := range want {
		if got[i] != want[i] || !bytes.Equal(got[i].Payload, want[i].Payload) {
			t.Fatalf("probe changed RTP packet %d", i)
		}
	}
	if probe.idrs.Load() != 1 || probe.idrsAfterHandoff.Load() != 1 {
		t.Fatalf("wrong IDR counters: %d/%d", probe.idrs.Load(), probe.idrsAfterHandoff.Load())
	}
}

func TestH264ProbeGatesFragmentedIDR(t *testing.T) {
	probe, source := newTestH264Probe()
	probe.beginHandoff()
	var got []*rtp.Packet
	probe.track.AppendChild(&core.Node{Input: func(packet *rtp.Packet) { got = append(got, packet) }})

	for _, packet := range []*rtp.Packet{
		testRTP(1, 90, 0x67, 1), testRTP(2, 90, 0x68, 1),
		testRTP(3, 180, 28, 0x80|h264.NALUTypeIFrame, 1),
		testRTP(4, 180, 28, h264.NALUTypeIFrame, 2),
		testRTP(5, 180, 28, 0x40|h264.NALUTypeIFrame, 3),
		testRTP(6, 270, 0x61, 1),
	} {
		source.WriteRTP(packet)
	}
	if len(got) != 6 || probe.readyIDRs.Load() != 1 {
		t.Fatalf("fragmented IDR did not open gate: packets=%d ready=%d", len(got), probe.readyIDRs.Load())
	}

	probe, source = newTestH264Probe()
	probe.beginHandoff()
	got = nil
	probe.track.AppendChild(&core.Node{Input: func(packet *rtp.Packet) { got = append(got, packet) }})
	source.WriteRTP(testRTP(7, 360, 0x67, 1))
	source.WriteRTP(testRTP(8, 360, 0x68, 1))
	source.WriteRTP(testRTP(9, 450, 28, 0x80|h264.NALUTypeIFrame, 1))
	source.WriteRTP(testRTP(11, 450, 28, 0x40|h264.NALUTypeIFrame, 2))
	source.WriteRTP(testRTP(12, 540, 0x61, 1))
	if len(got) != 3 {
		t.Fatalf("broken IDR did not close gate: packets=%d", len(got))
	}
	source.WriteRTP(testRTP(13, 630, 0x65, 1))
	source.WriteRTP(testRTP(14, 720, 0x61, 1))
	if len(got) != 5 || probe.readyIDRs.Load() != 1 {
		t.Fatal("complete replacement IDR did not reopen gate")
	}
}

func TestH264ProbeReadinessRequiresPostHandoffSequence(t *testing.T) {
	probe, _ := newTestH264Probe()

	probe.track.Input(testRTP(1, 90, 0x67, 1))
	probe.track.Input(testRTP(2, 90, 0x68, 1))
	probe.track.Input(testRTP(3, 90, 0x65, 1))
	if !probe.readyBeforeHandoff.Load() {
		t.Fatal("pre-handoff SPS/PPS/IDR was not recorded")
	}

	probe.track.Input(testRTP(4, 180, 28, 0x80|h264.NALUTypeSPS, 1))
	probe.beginHandoff()
	probe.track.Input(testRTP(5, 180, 28, 0x40|h264.NALUTypeSPS, 1))
	probe.track.Input(testRTP(6, 180, 0x68, 1))
	probe.track.Input(testRTP(7, 180, 0x65, 1))
	if probe.readyAfterHandoff.Load() != 0 || probe.readyIDRs.Load() != 0 {
		t.Fatal("pre-handoff fragment satisfied post-handoff readiness")
	}
	probe.track.Input(testRTP(8, 180, 0x67, 1))
	probe.track.Input(testRTP(9, 180, 0x68, 1))
	probe.track.Input(testRTP(10, 180, 0x65, 1))
	if probe.readyAfterHandoff.Load() == 0 || probe.readyIDRs.Load() != 1 {
		t.Fatal("post-handoff SPS/PPS/IDR was not recorded")
	}
}

func TestH264ProbeRejectsHeaderOnlyReadiness(t *testing.T) {
	probe, _ := newTestH264Probe()
	probe.beginHandoff()

	probe.track.Input(testRTP(1, 90, 0x67))
	probe.track.Input(testRTP(2, 90, 0x68))
	probe.track.Input(testRTP(3, 90, 0x65))
	probe.track.Input(testRTP(4, 90, 24, 0, 1, 0x67, 0, 1, 0x68, 0, 1, 0x65))
	if probe.readyAfterHandoff.Load() != 0 || probe.readyIDRs.Load() != 0 {
		t.Fatal("header-only NALUs satisfied decoder readiness")
	}
	if probe.malformed.Load() != 4 || probe.sps.Load() != 0 || probe.pps.Load() != 0 || probe.idrs.Load() != 0 {
		t.Fatal("header-only NALUs were not rejected atomically")
	}
}

func TestWebRTCClientProbeTrackAndPLI(t *testing.T) {
	client, media := newTestWebRTCClient()
	client.pliDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	requested := make(chan struct{}, 3)
	client.requestPLI = func(*core.Media) error {
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
	if !probe.handoffStarted.Load() {
		t.Fatal("probe handoff did not start")
	}
	data, err := client.MarshalJSON()
	if err != nil || !bytes.Contains(data, []byte(`"nest":`)) {
		t.Fatalf("probe diagnostics missing from connection JSON: %v", err)
	}
	close(client.done)
}

func TestWebRTCClientPreparesNegotiatedH264Profiles(t *testing.T) {
	client, media := newTestWebRTCClient(
		&core.Codec{Name: core.CodecH264, ClockRate: 90000, FmtpLine: "profile-level-id=42e01f", PayloadType: 96},
		&core.Codec{Name: core.CodecH264, ClockRate: 90000, FmtpLine: "profile-level-id=64001f", PayloadType: 98},
	)
	conn := client.conn
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

func TestH264ProbeKeepsSourcesAndStreamsIsolated(t *testing.T) {
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	codec := &core.Codec{Name: core.CodecH264}
	first := core.NewReceiver(media, codec)
	second := core.NewReceiver(media, codec)
	probe := newH264Probe(media, codec, first)
	probe.addSource(second)
	probe.beginHandoff()

	var forwarded atomic.Int32
	probe.track.AppendChild(&core.Node{Input: func(*rtp.Packet) { forwarded.Add(1) }})
	write := func(source *core.Receiver, pt uint8, sequence uint16, payload ...byte) {
		packet := testRTP(sequence, 90, payload...)
		packet.PayloadType = pt
		source.WriteRTP(packet)
	}

	write(first, 96, 1, 0x67, 1)
	write(second, 98, 1, 0x68, 1)
	write(second, 98, 2, 0x65, 1)
	if probe.readyIDRs.Load() != 0 || forwarded.Load() != 1 {
		t.Fatal("competing source contributed to probe state or output")
	}

	write(first, 98, 2, 0x68, 1)
	write(first, 98, 3, 0x65, 1)
	if probe.readyIDRs.Load() != 0 || forwarded.Load() != 2 {
		t.Fatal("payload change retained prior readiness or forwarded its IDR")
	}
	write(first, 98, 4, 0x67, 1)
	write(first, 98, 5, 0x68, 1)
	write(first, 98, 6, 0x65, 1)
	if probe.readyIDRs.Load() != 1 {
		t.Fatal("active source did not become decoder-ready")
	}
	before := forwarded.Load()
	otherSSRC := testRTP(7, 90, 0x67, 1)
	otherSSRC.PayloadType = 98
	otherSSRC.SSRC = 1
	first.WriteRTP(otherSSRC)
	if forwarded.Load() != before {
		t.Fatal("competing SSRC reached probe output")
	}

	var wg sync.WaitGroup
	for _, source := range []*core.Receiver{first, second} {
		wg.Add(1)
		go func(source *core.Receiver) {
			defer wg.Done()
			for i := uint16(0); i < 100; i++ {
				write(source, 98, 100+i, 0x61, 1)
			}
		}(source)
	}
	wg.Wait()
	if forwarded.Load()-before != 100 {
		t.Fatal("competing source reached probe output")
	}
	before = forwarded.Load()
	malformed := probe.malformed.Load()
	probe.close()
	write(first, 98, 200, 0x61, 1)
	if forwarded.Load() != before || probe.malformed.Load() != malformed {
		t.Fatal("closed probe forwarded or misclassified a valid packet")
	}
}

func TestWebRTCClientMatchesWildcardProbe(t *testing.T) {
	client, media := newTestWebRTCClient()
	client.pliDelays = []time.Duration{time.Hour}
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
	probe, _ := newTestH264Probe()
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

func TestVideoWatchdogRequiresDecoderReadyIDR(t *testing.T) {
	probe, _ := newTestH264Probe()
	start := time.Unix(1, 0)
	lastIDR := start
	var readyIDRs uint64
	var lastPLI time.Time
	want := map[time.Duration]videoWatchdogAction{
		15 * time.Second: videoWatchdogPLI, 30 * time.Second: videoWatchdogPLI,
		45 * time.Second: videoWatchdogPLI, 60 * time.Second: videoWatchdogRestartAction,
	}

	for elapsed := videoWatchdogTick; elapsed <= videoWatchdogRestart; elapsed += videoWatchdogTick {
		now := start.Add(elapsed)
		probe.idrs.Add(1)
		readyIDRs, lastIDR = videoWatchdogProgress(probe, readyIDRs, lastIDR, now)
		sincePLI := time.Duration(0)
		if !lastPLI.IsZero() {
			sincePLI = now.Sub(lastPLI)
		}
		action := videoWatchdogDecision(now.Sub(lastIDR), sincePLI)
		if action != want[elapsed] {
			t.Fatalf("wrong watchdog action at %s: want=%d got=%d", elapsed, want[elapsed], action)
		}
		if action == videoWatchdogPLI {
			lastPLI = now
		}
	}

	now := start.Add(videoWatchdogRestart + videoWatchdogTick)
	probe.readyIDRs.Add(1)
	_, lastIDR = videoWatchdogProgress(probe, readyIDRs, lastIDR, now)
	if lastIDR != now {
		t.Fatal("decoder-ready IDR did not reset watchdog liveness")
	}
}

func TestVideoWatchdogActions(t *testing.T) {
	probe, _ := newTestH264Probe()
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
	valid := testRTP(1, 90, 24, 0, 2, 0x67, 1, 0, 2, 0x68, 1, 0, 2, 0x65, 1)
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

func BenchmarkH264ProbeInput(b *testing.B) {
	_, source := newTestH264Probe()
	packet := testRTP(1, 90, 0x65, 1)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		source.WriteRTP(packet)
	}
}

func newTestH264Probe() (*h264Probe, *core.Receiver) {
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly}
	codec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96}
	source := core.NewReceiver(media, codec)
	return newH264Probe(media, codec, source), source
}

func newTestWebRTCClient(codecs ...*core.Codec) (*WebRTCClient, *core.Media) {
	if len(codecs) == 0 {
		codecs = []*core.Codec{{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96}}
	}
	media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly, Codecs: codecs}
	conn := &go2rtcwebrtc.Conn{Connection: core.Connection{Medias: []*core.Media{media}}, Mode: core.ModeActiveProducer}
	return &WebRTCClient{conn: conn, done: make(chan struct{})}, media
}

func testRTP(sequence uint16, timestamp uint32, payload ...byte) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: sequence, Timestamp: timestamp}, Payload: payload}
}

var errTestPLI = errors.New("test PLI failure")
