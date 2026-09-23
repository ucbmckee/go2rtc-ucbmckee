package baichuan

import (
	"encoding/binary"
	"fmt"
)

const mediaDiagnosticScan = 192 << 10

type mediaDiagnosticHeader struct {
	magic, codec, payload, extra uint32
	plain, padded                uint64
	adts                         uint32
}

// mediaFailureDetail is called only after parsing has already failed. It never changes parser state.
func mediaFailureDetail(b []byte, limits Limits) string {
	if len(b) < 4 {
		return fmt.Sprintf("media bytes %d", len(b))
	}
	magic := binary.LittleEndian.Uint32(b)
	declared := uint64(0)
	header := ""
	if len(b) >= 16 && (magic >= mediaIFrameMin && magic <= mediaIFrameMax ||
		magic >= mediaPFrameMin && magic <= mediaPFrameMax) {
		payload := binary.LittleEndian.Uint32(b[8:12])
		extra := binary.LittleEndian.Uint32(b[12:16])
		declared = 24 + uint64(extra) + uint64(payload) + uint64(padding(payload))
		header = fmt.Sprintf(" codec %#x payload %d extra %d", binary.LittleEndian.Uint32(b[4:8]), payload, extra)
	} else if len(b) >= 8 && (magic == mediaAAC || magic == mediaAACV2 || magic == mediaADPCM) {
		first, second := binary.LittleEndian.Uint16(b[4:6]), binary.LittleEndian.Uint16(b[6:8])
		payload := uint32(first)
		declared = 8 + uint64(payload) + uint64(padding(payload))
		header = fmt.Sprintf(" audio_sizes %d,%d adts %d", first, second, adtsLength(b[8:]))
	} else if len(b) >= 8 && (magic == mediaInfoV1 || magic == mediaInfoV2) {
		header = fmt.Sprintf(" info_size %d", binary.LittleEndian.Uint32(b[4:8]))
	}
	start := 4
	if declared > 4 && declared <= uint64(len(b)) && declared > 64<<10 {
		start = int(declared) - (64 << 10)
	}
	end := start + mediaDiagnosticScan
	if end > len(b) {
		end = len(b)
	}
	var best [2]mediaDiagnosticCandidate
	for offset := start; offset+4 <= end; offset++ {
		if !knownMediaMagic(binary.LittleEndian.Uint32(b[offset:])) {
			continue
		}
		h, ok := diagnosticHeader(b[offset:], limits.MaxMediaFrame)
		if !ok {
			continue
		}
		candidate := mediaDiagnosticCandidate{offset: offset, header: h, following: diagnosticFollowing(b[offset:], h, limits.MaxMediaFrame)}
		candidate.rank = 1
		if candidate.following == "coherent" {
			candidate.rank = 2
		}
		if candidate.rank > best[0].rank {
			best[1], best[0] = best[0], candidate
		} else if candidate.rank > best[1].rank {
			best[1] = candidate
		}
	}
	detail := fmt.Sprintf("media magic %#x%s declared %d scan [%d,%d) candidates", magic, header, declared, start, end)
	for _, candidate := range best {
		if candidate.rank == 0 {
			continue
		}
		h := candidate.header
		detail += fmt.Sprintf(" {offset %d magic %#x codec %#x payload %d extra %d plain %d padded %d adts %d aligned %t following_header %s}",
			candidate.offset, h.magic, h.codec, h.payload, h.extra, h.plain, h.padded, h.adts,
			candidate.offset%mediaAlignment == 0, candidate.following)
	}
	return detail
}

type mediaDiagnosticCandidate struct {
	offset    int
	header    mediaDiagnosticHeader
	following string
	rank      int
}

func diagnosticHeader(b []byte, maxFrame uint32) (mediaDiagnosticHeader, bool) {
	if len(b) < 8 {
		return mediaDiagnosticHeader{}, false
	}
	h := mediaDiagnosticHeader{magic: binary.LittleEndian.Uint32(b)}
	switch {
	case h.magic == mediaInfoV1 || h.magic == mediaInfoV2:
		if len(b) < 32 || binary.LittleEndian.Uint32(b[4:8]) != 32 {
			return h, false
		}
		h.plain, h.padded = 32, 32
	case h.magic >= mediaIFrameMin && h.magic <= mediaIFrameMax ||
		h.magic >= mediaPFrameMin && h.magic <= mediaPFrameMax:
		if len(b) < mediaVideoHeaderSize {
			return h, false
		}
		h.codec = binary.LittleEndian.Uint32(b[4:8])
		if h.codec != codecH264 && h.codec != codecH265 {
			return h, false
		}
		h.payload = binary.LittleEndian.Uint32(b[8:12])
		h.extra = binary.LittleEndian.Uint32(b[12:16])
		h.plain = 24 + uint64(h.extra) + uint64(h.payload)
		h.padded = h.plain + uint64(padding(h.payload))
	case h.magic == mediaAAC || h.magic == mediaAACV2 || h.magic == mediaADPCM:
		if binary.LittleEndian.Uint16(b[4:6]) != binary.LittleEndian.Uint16(b[6:8]) {
			return h, false
		}
		h.payload = uint32(binary.LittleEndian.Uint16(b[4:6]))
		h.plain = 8 + uint64(h.payload)
		h.padded = h.plain + uint64(padding(h.payload))
		if h.magic == mediaADPCM && h.payload < 4 {
			return h, false
		}
		if (h.magic == mediaAAC || h.magic == mediaAACV2) && h.payload >= 7 {
			h.adts = adtsLength(b[8:])
		}
	default:
		return h, false
	}
	return h, h.padded <= uint64(maxFrame)
}

func adtsLength(b []byte) uint32 {
	if len(b) < 7 || b[0] != 0xff || b[1]&0xf6 != 0xf0 || b[2]&0x3c == 0x3c {
		return 0
	}
	size := uint32(b[3]&3)<<11 | uint32(b[4])<<3 | uint32(b[5])>>5
	minimum := uint32(7)
	if b[1]&1 == 0 {
		minimum = 9
	}
	if size < minimum {
		return 0
	}
	return size
}

func diagnosticFollowing(b []byte, h mediaDiagnosticHeader, maxFrame uint32) string {
	for _, boundary := range [2]uint64{h.plain, h.padded} {
		if boundary >= uint64(len(b)) || uint64(len(b))-boundary < 8 {
			continue
		}
		if _, ok := diagnosticHeader(b[boundary:], maxFrame); ok {
			return "coherent"
		}
	}
	if h.plain >= uint64(len(b)) || uint64(len(b))-h.plain < 32 {
		return "unavailable"
	}
	return "incoherent"
}
