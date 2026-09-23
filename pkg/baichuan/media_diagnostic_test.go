package baichuan

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

func TestMediaFailureDetailFindsCandidateBeyondResync(t *testing.T) {
	const declared = 1000
	const offset = declared + 70<<10
	b := make([]byte, offset+64)
	binary.LittleEndian.PutUint32(b, mediaIFrameMin)
	copy(b[4:8], "H264")
	binary.LittleEndian.PutUint32(b[8:12], declared-24)
	binary.LittleEndian.PutUint32(b[declared+8:], mediaPFrameMin)
	copy(b[declared+12:declared+16], "bogus")
	copy(b[offset:], infoFixture())
	copy(b[offset+32:], infoFixture())
	limits, err := (Limits{MaxMediaFrame: 128 << 10, MaxMediaBuffer: 256 << 10, MaxResync: 64 << 10}).normalized()
	if err != nil {
		t.Fatal(err)
	}
	p, _ := NewMediaParser(limits)
	if _, err = p.Append(b); err == nil || !strings.Contains(err.Error(), "no keyframe boundary") {
		t.Fatalf("parser accepted out-of-window boundary: %v", err)
	}
	detail := mediaFailureDetail(p.buf, limits)
	if !strings.Contains(detail, fmt.Sprintf("offset %d magic", offset)) ||
		!strings.Contains(detail, "following_header coherent") ||
		strings.Contains(detail, fmt.Sprintf("offset %d magic", declared+8)) {
		t.Fatalf("wrong diagnostic candidates: %s", detail)
	}
}

func TestMediaFailureDetailBoundsAndRedacts(t *testing.T) {
	const secret = "sentinel-private-XML-and-credentials"
	b := make([]byte, 1<<20)
	copy(b[100:], secret)
	binary.LittleEndian.PutUint32(b, mediaIFrameMin)
	copy(b[4:8], "H264")
	binary.LittleEndian.PutUint32(b[8:12], 1<<19-24)
	detail := mediaFailureDetail(b, Limits{MaxMediaFrame: 1 << 20})
	if !strings.Contains(detail, "scan [458752,655360)") || strings.Contains(detail, secret) || len(detail) > 700 {
		t.Fatalf("unbounded or unsafe diagnostic: %s", detail)
	}
	truncated := make([]byte, 19)
	binary.LittleEndian.PutUint32(truncated, mediaPFrameMin)
	copy(truncated[4:8], "H264")
	if detail = mediaFailureDetail(truncated, Limits{MaxMediaFrame: 1 << 20}); strings.Contains(detail, "offset ") {
		t.Fatalf("truncated header became candidate: %s", detail)
	}
}

func TestMediaFailureDetailReportsCurrentAACLengths(t *testing.T) {
	b := make([]byte, 72)
	binary.LittleEndian.PutUint32(b, mediaAAC)
	binary.LittleEndian.PutUint16(b[4:6], 13)
	binary.LittleEndian.PutUint16(b[6:8], 13)
	copy(b[8:], []byte{0xff, 0xf1, 0x50, 0x80, 0x03, 0xc0, 0}) // ADTS frame length 30.
	copy(b[40:], infoFixture())
	limits, _ := (Limits{MaxMediaFrame: 128, MaxMediaBuffer: 256, MaxResync: 64}).normalized()
	p, _ := NewMediaParser(limits)
	if _, err := p.Append(b); err == nil || !strings.Contains(err.Error(), "boundary differs") {
		t.Fatalf("parser accepted inconsistent AAC boundary: %v", err)
	}
	detail := mediaFailureDetail(p.buf, limits)
	if !strings.Contains(detail, "audio_sizes 13,13 adts 30 declared 24") {
		t.Fatalf("missing current AAC lengths: %s", detail)
	}
	copy(b[8:], []byte{0xff, 0xf7, 0x50, 0x80, 0x03, 0xc0, 0}) // Invalid ADTS layer.
	if detail = mediaFailureDetail(b, limits); !strings.Contains(detail, "adts 0") {
		t.Fatalf("invalid ADTS header accepted: %s", detail)
	}
}

func TestPreviewParserFailureIncludesRedactedOuterMetadata(t *testing.T) {
	limits, _ := (Limits{}).normalized()
	parser, _ := NewMediaParser(limits)
	client := &Client{previews: make(map[previewKey]*Preview)}
	preview := &Preview{
		client: client, messages: make(chan message, 1),
		queueLimit: int64(limits.MaxMediaBuffer), done: make(chan struct{}), parser: parser,
	}
	client.previews[preview.key] = preview
	const secret = "sentinel-private-XML-and-credentials"
	preview.deliver(message{
		header:    header{Command: commandPreview, Sequence: 42, ResponseCode: 200},
		extension: []byte(secret), payload: []byte{9, 8, 7, 6},
		binary: true, binaryFlag: 1, hasBinaryFlag: true,
		encrypt: 2, hasEncryptLen: true, checkPos: 11, hasCheckPos: true,
	})
	_, err := preview.Read(context.Background())
	if err == nil || !strings.Contains(err.Error(), "command 3 sequence 42 status 200") ||
		!strings.Contains(err.Error(), "binaryData 1 (present true)") ||
		!strings.Contains(err.Error(), "encrypted prefix 2 (present true), check position 11 (present true)") ||
		strings.Contains(err.Error(), secret) {
		t.Fatalf("incorrect parser failure context: %v", err)
	}
}

func TestMediaParserCodecFailureUsesNumericDetail(t *testing.T) {
	p, _ := NewMediaParser(Limits{})
	b := make([]byte, mediaVideoHeaderSize)
	binary.LittleEndian.PutUint32(b, mediaPFrameMin)
	copy(b[4:8], "pw!@")
	_, err := p.Append(b)
	if err == nil || !strings.Contains(err.Error(), "unsupported video codec 0x") || strings.Contains(err.Error(), "pw!@") {
		t.Fatalf("raw codec bytes in parser error: %v", err)
	}
}
