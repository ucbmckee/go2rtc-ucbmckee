package reolink

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/baichuan"
	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
)

func TestWritePacketRejectsVideoCodecChange(t *testing.T) {
	pipeline := &mediaPipeline{videoCodec: "H265"}
	if _, err := pipeline.frame(baichuan.MediaPacket{Kind: baichuan.MediaVideoP, Codec: "H264"}); err == nil {
		t.Fatal("accepted video codec change")
	}
}

func TestValidateVideoPayloadReasons(t *testing.T) {
	tests := []struct {
		codec string
		data  []byte
		want  string
	}{
		{"H264", []byte{0, 0, 0, 1, 0}, "NAL 0 has type 0"},
		{"H264", []byte{0, 0, 0, 1, 0x81}, "NAL 0 has forbidden bit"},
		{"H265", []byte{0, 0, 0, 2, 2, 0}, "NAL 0 has temporal ID zero"},
		{"H265", []byte{0, 0, 0, 2, 48 << 1, 1}, "NAL 0 has type 48"},
	}
	for _, test := range tests {
		if err := validateVideoPayload(test.codec, test.data); err == nil || err.Error() != test.want {
			t.Errorf("%s validation error = %v, want %q", test.codec, err, test.want)
		}
	}
}

func TestWritePacketAcceptsDuplicateH264ParameterSets(t *testing.T) {
	data := h264Keyframe(t, "Z2QAFqwa0BQF/yzcBAQFAAADAAEAAAMAHo8UIqA=", 0x80, 1)
	pipeline := &mediaPipeline{}
	if err := pipeline.configureVideo(baichuan.MediaPacket{Kind: baichuan.MediaVideoI, Codec: "H264", Data: data}); err != nil {
		t.Fatal(err)
	}
	config := pipeline.videoConfig
	changed := h264Keyframe(t, "Z2QAFqwa0BQF/yzcBAQFAAADAAEAAAMAHo8UIqA=", 0x80, 2)
	if frame, err := pipeline.frame(baichuan.MediaPacket{
		Kind: baichuan.MediaVideoI, Codec: "H264", Data: changed,
	}); err != nil || !frame.valid() {
		t.Fatalf("duplicate parameter sets failed: frame=%v err=%v", frame, err)
	}
	if pipeline.videoConfig != config {
		t.Fatalf("parameter variant changed configuration: %q", pipeline.videoConfig)
	}
	if _, err := pipeline.frame(baichuan.MediaPacket{
		Kind: baichuan.MediaVideoI, Codec: "H264",
		Data: h264Keyframe(t, "Z2QAFqwa0BQF/yzcBAQFAAADAAEAAAMAHo8UIqA=", 0x80, 2),
	}); err != nil {
		t.Fatalf("repeated parameter variant failed: %v", err)
	}
}

func TestWritePacketAcceptsReorderedH264ParameterSets(t *testing.T) {
	sps, err := base64.StdEncoding.DecodeString("Z2QAFqwa0BQF/yzcBAQFAAADAAEAAAMAHo8UIqA=")
	if err != nil {
		t.Fatal(err)
	}
	pps := []byte{0x68, 0xee, 0x3c, 0x80}
	idr := []byte{0x65, 0}
	pipeline := &mediaPipeline{}
	if err = pipeline.configureVideo(baichuan.MediaPacket{
		Kind: baichuan.MediaVideoI, Codec: "H264", Data: h264AccessUnit(sps, pps, idr),
	}); err != nil {
		t.Fatal(err)
	}
	frame, err := pipeline.frame(baichuan.MediaPacket{
		Kind: baichuan.MediaVideoI, Codec: "H264", Data: h264AccessUnit(pps, sps, idr),
	})
	if err != nil || !frame.valid() {
		t.Fatalf("reordered parameter sets failed: frame=%v err=%v", frame, err)
	}
}

func TestWritePacketRejectsH264ParameterSetChange(t *testing.T) {
	for _, name := range []string{"sps", "pps"} {
		t.Run(name, func(t *testing.T) {
			pipeline := &mediaPipeline{}
			data := h264Keyframe(t, "Z2QAFqwa0BQF/yzcBAQFAAADAAEAAAMAHo8UIqA=", 0x80, 1)
			if err := pipeline.configureVideo(baichuan.MediaPacket{Kind: baichuan.MediaVideoI, Codec: "H264", Data: data}); err != nil {
				t.Fatal(err)
			}
			changed := h264Keyframe(t, "Z2QAFqwa0BQF/yzcBAQFAAADAAEAAAMAHo8UIqA=", 0x80, 1)
			if name == "sps" {
				changed[10] ^= 1
			} else {
				changed[len(changed)-7] ^= 1
			}
			packet := baichuan.MediaPacket{
				Kind: baichuan.MediaVideoI, Codec: "H264", Data: changed,
			}
			if frame, err := pipeline.frame(packet); err != nil || frame.valid() {
				t.Fatalf("first parameter-set candidate was not suppressed: frame=%v err=%v", frame, err)
			}
			packet.Data = h264Keyframe(t, "Z2QAFqwa0BQF/yzcBAQFAAADAAEAAAMAHo8UIqA=", 0x80, 1)
			if name == "sps" {
				packet.Data[10] ^= 1
			} else {
				packet.Data[len(packet.Data)-7] ^= 1
			}
			if _, err := pipeline.frame(packet); err == nil || !strings.Contains(err.Error(), "parameter sets changed") {
				t.Fatalf("unexpected parameter-set error: %v", err)
			}
		})
	}
}

func TestWritePacketRejectsH265ParameterSetChange(t *testing.T) {
	pipeline := &mediaPipeline{}
	if err := pipeline.configureVideo(testVideoKeyframe("H265", 0)); err != nil {
		t.Fatal(err)
	}
	changed := testVideoKeyframe("H265", 1_000_000)
	changed.Data[5] ^= 8 // change nuh_layer_id without invalidating the NAL header
	if frame, err := pipeline.videoFrame(changed); err != nil || frame.valid() {
		t.Fatalf("first parameter-set candidate was not suppressed: frame=%v err=%v", frame, err)
	}
	changed = testVideoKeyframe("H265", 1_050_000)
	changed.Data[5] ^= 8
	if _, err := pipeline.videoFrame(changed); err == nil || !strings.Contains(err.Error(), "parameter sets changed") {
		t.Fatalf("unexpected parameter-set error: %v", err)
	}
}

func TestWritePacketRecoversFromSingleH264ParameterCandidate(t *testing.T) {
	pipeline := &mediaPipeline{}
	base := h264Keyframe(t, "Z2QAFqwa0BQF/yzcBAQFAAADAAEAAAMAHo8UIqA=", 0x80, 1)
	if err := pipeline.configureVideo(baichuan.MediaPacket{Kind: baichuan.MediaVideoI, Codec: "H264", Data: base}); err != nil {
		t.Fatal(err)
	}
	changed := h264Keyframe(t, "Z2QAFqwa0BQF/yzcBAQFAAADAAEAAAMAHo8UIqA=", 0x80, 1)
	changed[10] ^= 1
	if frame, err := pipeline.frame(baichuan.MediaPacket{
		Kind: baichuan.MediaVideoI, Codec: "H264", Timestamp: 1_000_000, Data: changed,
	}); err != nil || frame.valid() {
		t.Fatalf("parameter candidate was not suppressed: frame=%v err=%v", frame, err)
	}
	if frame, err := pipeline.frame(baichuan.MediaPacket{
		Kind: baichuan.MediaVideoI, Codec: "H264", Timestamp: 1_050_000,
		Data: h264Keyframe(t, "Z2QAFqwa0BQF/yzcBAQFAAADAAEAAAMAHo8UIqA=", 0x80, 1),
	}); err != nil || !frame.valid() {
		t.Fatalf("original parameters did not recover: frame=%v err=%v", frame, err)
	}
}

func TestWritePacketResyncsAfterExcessH264ParameterSets(t *testing.T) {
	pipeline := &mediaPipeline{}
	base := h264Keyframe(t, "Z2QAFqwa0BQF/yzcBAQFAAADAAEAAAMAHo8UIqA=", 0x80, 1)
	if err := pipeline.configureVideo(baichuan.MediaPacket{Kind: baichuan.MediaVideoI, Codec: "H264", Data: base}); err != nil {
		t.Fatal(err)
	}
	if frame, err := pipeline.videoFrame(baichuan.MediaPacket{
		Kind: baichuan.MediaVideoI, Codec: "H264", Timestamp: 1_000_000,
		Data: h264Keyframe(t, "Z2QAFqwa0BQF/yzcBAQFAAADAAEAAAMAHo8UIqA=", 0x80, 64),
	}); err != nil || frame.valid() {
		t.Fatalf("excess parameter sets were not suppressed: frame=%v err=%v", frame, err)
	}
	if frame, err := pipeline.videoFrame(baichuan.MediaPacket{
		Kind: baichuan.MediaVideoI, Codec: "H264", Timestamp: 1_050_000, Data: base,
	}); err != nil || !frame.valid() {
		t.Fatalf("valid keyframe did not resync: frame=%v err=%v", frame, err)
	}
}

func TestWritePacketInvalidVideoDoesNotPoisonClock(t *testing.T) {
	pipeline := &mediaPipeline{}
	base := testH264Keyframe(1_000_000)
	if err := pipeline.configureVideo(base); err != nil {
		t.Fatal(err)
	}
	if frame, err := pipeline.videoFrame(base); err != nil || !frame.valid() || frame.packet.Timestamp != 0 {
		t.Fatalf("unexpected initial frame: frame=%v err=%v", frame, err)
	}
	if frame, err := pipeline.videoFrame(baichuan.MediaPacket{
		Kind: baichuan.MediaVideoP, Codec: "H264", Timestamp: 21_000_000,
		Data: []byte{0, 0, 0, 1, 0xc1, 1},
	}); err != nil || frame.valid() {
		t.Fatalf("invalid frame was not suppressed: frame=%v err=%v", frame, err)
	}
	recovery := testH264Keyframe(1_100_000)
	frame, err := pipeline.videoFrame(recovery)
	if err != nil || !frame.valid() {
		t.Fatalf("invalid timestamp poisoned recovery: frame=%v err=%v", frame, err)
	}
	if frame.packet.Timestamp != 9000 {
		t.Fatalf("recovery timestamp = %d, want 9000", frame.packet.Timestamp)
	}
}

func TestConfigureVideoBoundsH264ParameterSets(t *testing.T) {
	data := h264Keyframe(t, "Z2QAFqwa0BQF/yzcBAQFAAADAAEAAAMAHo8UIqA=", 0x80, 64)
	err := (&mediaPipeline{}).configureVideo(baichuan.MediaPacket{
		Kind: baichuan.MediaVideoI, Codec: "H264", Data: data,
	})
	if err == nil || !strings.Contains(err.Error(), "parameter sets exceed 64") {
		t.Fatalf("unexpected parameter-set limit error: %v", err)
	}
}

func h264Keyframe(t *testing.T, encodedSPS string, ppsTail byte, spsCopies int) []byte {
	t.Helper()
	sps, err := base64.StdEncoding.DecodeString(encodedSPS)
	if err != nil {
		t.Fatal(err)
	}
	nalus := make([][]byte, 0, spsCopies+2)
	for range spsCopies {
		nalus = append(nalus, sps)
	}
	nalus = append(nalus, []byte{0x68, 0xee, 0x3c, ppsTail}, []byte{0x65, 0})
	return h264AccessUnit(nalus...)
}

func h264AccessUnit(nalus ...[]byte) []byte {
	var data []byte
	for _, nalu := range nalus {
		data = append(data, 0, 0, 0, 1)
		data = append(data, nalu...)
	}
	return data
}

func FuzzH264ParameterSignature(f *testing.F) {
	f.Add([]byte{0x67, 0x64, 0, 0x29})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 4 || len(data) > 4096 {
			return
		}
		sps := append([]byte(nil), data...)
		sps[0] = 0x67
		avcc := make([]byte, 4, len(sps)+10)
		binary.BigEndian.PutUint32(avcc, uint32(len(sps)))
		avcc = append(avcc, sps...)
		avcc = binary.BigEndian.AppendUint32(avcc, 2)
		avcc = append(avcc, 0x68, 0)
		_, _ = h264ParameterSignature(avcc)
	})
}

func TestEncodeOwnedToAVCC(t *testing.T) {
	for _, input := range [][]byte{
		{0, 0, 0, 1, 0x41, 1, 2, 0, 0, 0, 1, 0x41, 3},
		{0, 0, 1, 0x41, 1, 2, 0, 0, 1, 0x41, 3},
		{0, 0, 0, 1, 0x09, 0xf0, 0, 0, 0, 1, 0x41, 3},
		{0, 0, 0, 1, 0x41, 1, 2, 0, 0, 1, 0x41, 3},
		{0, 0, 0, 1, 0, 0, 0, 1, 0x41, 3},
	} {
		original := append([]byte(nil), input...)
		want := annexb.EncodeToAVCC(original)
		if got := encodeOwnedToAVCC(input); !bytes.Equal(got, want) {
			t.Fatalf("unexpected conversion for %x: %x != %x", original, got, want)
		}
	}
	input := []byte{0, 0, 0, 1, 0x41, 1, 2}
	if got := encodeOwnedToAVCC(input); &got[0] != &input[0] {
		t.Fatal("four-byte access unit was copied")
	}
}

func FuzzEncodeOwnedToAVCC(f *testing.F) {
	for _, input := range [][]byte{
		{0, 0, 0, 1, 0x41, 1, 2, 0, 0, 0, 1, 0x41, 3},
		{0, 0, 1, 0x41, 1, 2},
		{0, 0, 0, 1, 0x09, 0xf0, 0, 0, 0, 1, 0x41, 3},
		{0, 0, 0, 1, 0, 0, 0, 1, 0x41, 3},
	} {
		f.Add(input)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 1<<20 {
			t.Skip()
		}
		original := append([]byte(nil), input...)
		want := annexb.EncodeToAVCC(original)
		if got := encodeOwnedToAVCC(input); !bytes.Equal(got, want) {
			t.Fatalf("unexpected conversion for %x: %x != %x", original, got, want)
		}
	})
}

func TestAddVideoRejectsMalformedKeyframe(t *testing.T) {
	for _, test := range []struct {
		codec string
		data  []byte
	}{
		{codec: "H264"},
		{codec: "H264", data: []byte{0, 0, 0, 1, 0x67, 0x64}},
		{codec: "H264", data: []byte{
			0, 0, 0, 1, 0x67, 0x64,
			0, 0, 0, 1, 0x67, 0x64, 0, 0x29,
			0, 0, 0, 1, 0x68, 0,
			0, 0, 0, 1, 0x65, 0,
		}},
		{codec: "H264", data: []byte{0, 0, 0, 1, 0x65, 0}},
		{codec: "H265", data: []byte{0, 0, 0, 1, 0x40}},
	} {
		pipeline := &mediaPipeline{}
		if err := pipeline.configureVideo(baichuan.MediaPacket{Codec: test.codec, Data: test.data}); err == nil {
			t.Fatalf("accepted malformed %s keyframe: %x", test.codec, test.data)
		}
	}
}

func TestWritePacketResyncsAfterInvalidNALHeaders(t *testing.T) {
	for _, test := range []struct {
		codec string
		nalu  []byte
	}{
		{codec: "H264", nalu: []byte{0, 1}},
		{codec: "H264", nalu: []byte{0x78, 1}},
		{codec: "H264", nalu: []byte{0xc1, 1}},
		{codec: "H265", nalu: []byte{48 << 1, 1}},
		{codec: "H265", nalu: []byte{0x80, 1}},
		{codec: "H265", nalu: []byte{1 << 1, 0}},
	} {
		t.Run(fmt.Sprintf("%s_%x", test.codec, test.nalu), func(t *testing.T) {
			pipeline := &mediaPipeline{}
			keyframe := testVideoKeyframe(test.codec, 1_000_000)
			if err := pipeline.configureVideo(keyframe); err != nil {
				t.Fatal(err)
			}
			data := append([]byte{0, 0, 0, 1}, test.nalu...)
			if frame, err := pipeline.videoFrame(baichuan.MediaPacket{
				Kind: baichuan.MediaVideoP, Codec: test.codec, Timestamp: 1_050_000, Data: data,
			}); err != nil || frame.valid() {
				t.Fatalf("invalid NAL was not suppressed: frame=%v err=%v", frame, err)
			}
			if frame, err := pipeline.videoFrame(testVideoPFrame(test.codec, 1_100_000)); err != nil || frame.valid() {
				t.Fatalf("intervening frame was not suppressed: frame=%v err=%v", frame, err)
			}
			if frame, err := pipeline.videoFrame(testVideoKeyframe(test.codec, 1_150_000)); err != nil || !frame.valid() {
				t.Fatalf("valid keyframe did not resync: frame=%v err=%v", frame, err)
			}
		})
	}
}

func testVideoKeyframe(codec string, timestamp uint32) baichuan.MediaPacket {
	if codec == "H264" {
		return testH264Keyframe(timestamp)
	}
	return baichuan.MediaPacket{
		Kind: baichuan.MediaVideoI, Codec: "H265", Timestamp: timestamp,
		Data: []byte{
			0, 0, 0, 1, 0x40, 1,
			0, 0, 0, 1, 0x42, 1,
			0, 0, 0, 1, 0x44, 1,
			0, 0, 0, 1, 0x26, 1,
		},
	}
}

func testVideoPFrame(codec string, timestamp uint32) baichuan.MediaPacket {
	if codec == "H264" {
		return testH264PFrame(timestamp)
	}
	return baichuan.MediaPacket{
		Kind: baichuan.MediaVideoP, Codec: "H265", Timestamp: timestamp,
		Data: []byte{0, 0, 0, 1, 2, 1, 0},
	}
}

func TestAddVideoAcceptsParameterSetsAndKeyframe(t *testing.T) {
	for _, test := range []struct {
		codec string
		data  []byte
	}{
		{codec: "H264", data: []byte{
			0, 0, 0, 1, 0x67, 0x64, 0, 0x29,
			0, 0, 0, 1, 0x68, 0,
			0, 0, 0, 1, 0x65, 0,
		}},
		{codec: "H265", data: []byte{
			0, 0, 0, 1, 0x40, 1,
			0, 0, 0, 1, 0x42, 1,
			0, 0, 0, 1, 0x44, 1,
			0, 0, 0, 1, 0x26, 1,
		}},
	} {
		pipeline := &mediaPipeline{}
		if err := pipeline.configureVideo(baichuan.MediaPacket{Codec: test.codec, Data: test.data}); err != nil {
			t.Fatalf("rejected valid %s keyframe: %v", test.codec, err)
		}
	}
}

func FuzzConfigureH264(f *testing.F) {
	f.Add([]byte{
		0, 0, 0, 1, 0x67, 0x64, 0, 0x29,
		0, 0, 0, 1, 0x68, 0,
		0, 0, 0, 1, 0x65, 0,
	})
	f.Add([]byte{
		0, 0, 0, 1, 0x67, 0x64,
		0, 0, 0, 1, 0x67, 0x64, 0, 0x29,
		0, 0, 0, 1, 0x68, 0,
		0, 0, 0, 1, 0x65, 0,
	})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		_ = (&mediaPipeline{}).configureVideo(baichuan.MediaPacket{
			Kind: baichuan.MediaVideoI, Codec: "H264", Data: append([]byte(nil), data...),
		})
	})
}

func FuzzVideoFrameRepair(f *testing.F) {
	f.Add(false, false, []byte{0, 0, 0, 1, 0xc1, 1})
	f.Add(true, false, []byte{0, 0, 0, 1, 0x80, 1})
	f.Add(false, true, testH264Keyframe(0).Data)
	f.Fuzz(func(t *testing.T, h265Codec, keyframe bool, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		codec := "H264"
		if h265Codec {
			codec = "H265"
		}
		pipeline := &mediaPipeline{}
		base := testVideoKeyframe(codec, 1_000_000)
		if err := pipeline.configureVideo(base); err != nil {
			t.Fatal(err)
		}
		kind := baichuan.MediaVideoP
		if keyframe {
			kind = baichuan.MediaVideoI
		}
		_, _ = pipeline.videoFrame(baichuan.MediaPacket{
			Kind: kind, Codec: codec, Timestamp: 1_050_000, Data: data,
		})
		if _, err := pipeline.videoFrame(testVideoKeyframe(codec, 1_100_000)); err != nil {
			t.Fatal(err)
		}
	})
}
