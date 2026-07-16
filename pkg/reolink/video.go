package reolink

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"

	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
	"github.com/AlexxIT/go2rtc/pkg/h265"
)

// encodeOwnedToAVCC reuses access units with nonempty NALs, four-byte start codes, and no AUD.
// Preview owns packet data; every other layout uses the parent converter.
func encodeOwnedToAVCC(data []byte) []byte {
	if !canEncodeToAVCCInPlace(data) {
		return annexb.EncodeToAVCC(data)
	}
	header, nalu := 0, 4
	for {
		offset := bytes.Index(data[nalu:], []byte{0, 0, 1})
		if offset < 0 {
			binary.BigEndian.PutUint32(data[header:], uint32(len(data)-nalu))
			return data
		}
		next := nalu + offset - 1
		binary.BigEndian.PutUint32(data[header:], uint32(next-nalu))
		header, nalu = next, next+4
	}
}

func canEncodeToAVCCInPlace(data []byte) bool {
	if len(data) < 5 || binary.BigEndian.Uint32(data) != 1 {
		return false
	}
	for nalu := 4; ; {
		if nalu >= len(data) || data[nalu]&0x1f == 9 || data[nalu]&0x7e == 35<<1 {
			return false
		}
		offset := bytes.Index(data[nalu:], []byte{0, 0, 1})
		if offset < 0 {
			return true
		}
		marker := nalu + offset
		if marker <= nalu+1 || data[marker-1] != 0 {
			return false
		}
		nalu = marker + 3
	}
}

func validAVCC(data []byte, minNALU int) bool {
	if len(data) < 4+minNALU {
		return false
	}
	for len(data) >= 4 {
		size := binary.BigEndian.Uint32(data)
		if size < uint32(minNALU) || uint64(size) > uint64(len(data)-4) {
			return false
		}
		data = data[4+int(size):]
	}
	return len(data) == 0
}

func validVideoConfig(codec string, data []byte) bool {
	if validateVideoPayload(codec, data) != nil {
		return false
	}
	var first, second, third, keyframe bool
	for len(data) != 0 {
		size := int(binary.BigEndian.Uint32(data))
		nalu := data[4 : 4+size]
		switch codec {
		case "H264":
			switch nalu[0] & 0x1f {
			case h264.NALUTypeSPS:
				if len(nalu) < 4 {
					return false
				}
				first = true
			case h264.NALUTypePPS:
				second = true
			case h264.NALUTypeIFrame:
				keyframe = true
			}
		case "H265":
			if len(nalu) < 2 {
				return false
			}
			switch (nalu[0] >> 1) & 0x3f {
			case h265.NALUTypeVPS:
				first = true
			case h265.NALUTypeSPS:
				second = true
			case h265.NALUTypePPS:
				third = true
			case h265.NALUTypeIFrame, h265.NALUTypeIFrame2, h265.NALUTypeIFrame3:
				keyframe = true
			}
		default:
			return false
		}
		data = data[4+size:]
	}
	if codec == "H264" {
		return first && second && keyframe
	}
	return first && second && third && keyframe
}

func validateVideoPayload(codec string, data []byte) error {
	minNALU := 1
	if codec == "H265" {
		minNALU = 2
	} else if codec != "H264" {
		return fmt.Errorf("unsupported codec %q", codec)
	}
	if !validAVCC(data, minNALU) {
		return fmt.Errorf("invalid AVCC framing")
	}
	index := 0
	for len(data) != 0 {
		size := int(binary.BigEndian.Uint32(data))
		nalu := data[4 : 4+size]
		if codec == "H264" {
			typeID := nalu[0] & 0x1f
			if nalu[0]&0x80 != 0 {
				return fmt.Errorf("NAL %d has forbidden bit", index)
			}
			if typeID == 0 || typeID >= 24 {
				return fmt.Errorf("NAL %d has type %d", index, typeID)
			}
		} else {
			typeID := (nalu[0] >> 1) & 0x3f
			if nalu[0]&0x80 != 0 {
				return fmt.Errorf("NAL %d has forbidden bit", index)
			}
			if typeID >= 48 {
				return fmt.Errorf("NAL %d has type %d", index, typeID)
			}
			if nalu[1]&7 == 0 {
				return fmt.Errorf("NAL %d has temporal ID zero", index)
			}
		}
		data = data[4+size:]
		index++
	}
	return nil
}

func h264ParameterSignature(data []byte) (string, error) {
	const maxSets = 64
	sets := make([]string, 0, 2)
	count := 0
	for len(data) >= 4 {
		size := int(binary.BigEndian.Uint32(data))
		if size <= 0 || size > len(data)-4 {
			return "", mediaErrorf("reolink: invalid H264 parameter sets")
		}
		nalu := data[4 : 4+size]
		typeID := nalu[0] & 0x1f
		if typeID == h264.NALUTypeSPS || typeID == h264.NALUTypePPS {
			if count == maxSets {
				return "", mediaErrorf("reolink: H264 parameter sets exceed %d", maxSets)
			}
			count++
			digest := sha256.Sum256(nalu)
			var raw [sha256.Size + 1]byte
			raw[0] = typeID
			copy(raw[1:], digest[:])
			key := string(raw[:])
			duplicate := false
			for _, existing := range sets {
				if existing == key {
					duplicate = true
					break
				}
			}
			if !duplicate {
				sets = append(sets, key)
			}
		}
		data = data[4+size:]
	}
	if len(data) != 0 || len(sets) < 2 {
		return "", mediaErrorf("reolink: invalid H264 parameter sets")
	}
	sort.Strings(sets)
	return strings.Join(sets, ""), nil
}
