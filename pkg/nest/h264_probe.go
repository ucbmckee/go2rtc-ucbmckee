package nest

import (
	"sync/atomic"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/pion/rtp"
)

type h264Probe struct {
	media *core.Media
	track *core.Receiver
	start time.Time

	handoffStarted     atomic.Bool
	readyBeforeHandoff atomic.Bool
	packets            atomic.Uint64
	idrs               atomic.Uint64
	sps                atomic.Uint64
	pps                atomic.Uint64
	psAfterHandoff     atomic.Uint64
	idrsAfterHandoff   atomic.Uint64
	malformed          atomic.Uint64
	plis               atomic.Uint64
	pliErrors          atomic.Uint64
	firstReady         atomic.Int64

	haveSPS bool
	havePPS bool
}

func newH264Probe(media *core.Media, codec *core.Codec, source *core.Receiver) *h264Probe {
	track := core.NewReceiver(media, codec)
	probe := &h264Probe{media: media, track: track, start: time.Now()}
	forward := track.Input
	track.Input = func(packet *rtp.Packet) {
		probe.inspect(packet.Payload)
		forward(packet)
	}
	track.WithParent(&source.Node)
	return probe
}

func (p *h264Probe) inspect(payload []byte) {
	p.packets.Add(1)
	ok := eachRTPNALU(payload, func(typ byte) {
		switch typ {
		case h264.NALUTypeSPS:
			p.haveSPS = true
			p.sps.Add(1)
			if p.handoffStarted.Load() {
				p.psAfterHandoff.Add(1)
			}
		case h264.NALUTypePPS:
			p.havePPS = true
			p.pps.Add(1)
			if p.handoffStarted.Load() {
				p.psAfterHandoff.Add(1)
			}
		case h264.NALUTypeIFrame:
			p.idrs.Add(1)
			if p.handoffStarted.Load() {
				p.idrsAfterHandoff.Add(1)
			}
			if p.haveSPS && p.havePPS {
				p.markReady()
			}
		}
	})
	if !ok {
		p.malformed.Add(1)
	}
}

func (p *h264Probe) markReady() {
	elapsed := time.Since(p.start)
	if elapsed <= 0 {
		elapsed = 1
	}
	if p.firstReady.CompareAndSwap(0, int64(elapsed)) && !p.handoffStarted.Load() {
		p.readyBeforeHandoff.Store(true)
	}
}

type h264Diagnostics struct {
	Packets          uint64 `json:"video_packets"`
	IDRs             uint64 `json:"video_idrs"`
	SPS              uint64 `json:"video_sps"`
	PPS              uint64 `json:"video_pps"`
	PSAfterHandoff   uint64 `json:"video_ps_after_handoff"`
	IDRsAfterHandoff uint64 `json:"video_idrs_after_handoff"`
	PLIs             uint64 `json:"video_plis"`
	PLIErrors        uint64 `json:"video_pli_errors"`
	Malformed        uint64 `json:"video_malformed"`
	ReadyAfter       string `json:"video_ready_after,omitempty"`
	ReadyBefore      bool   `json:"video_ready_before_handoff"`
}

func (p *h264Probe) diagnostics() h264Diagnostics {
	ready := "none"
	if elapsed := time.Duration(p.firstReady.Load()); elapsed > 0 {
		ready = elapsed.Round(time.Millisecond).String()
	}
	return h264Diagnostics{
		Packets: p.packets.Load(), IDRs: p.idrs.Load(), SPS: p.sps.Load(), PPS: p.pps.Load(),
		PSAfterHandoff: p.psAfterHandoff.Load(), IDRsAfterHandoff: p.idrsAfterHandoff.Load(),
		PLIs: p.plis.Load(), PLIErrors: p.pliErrors.Load(), Malformed: p.malformed.Load(),
		ReadyAfter: ready, ReadyBefore: p.readyBeforeHandoff.Load(),
	}
}

func eachRTPNALU(payload []byte, fn func(byte)) bool {
	if len(payload) == 0 {
		return false
	}
	switch typ := payload[0] & 0x1F; typ {
	case 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23:
		fn(typ)
		return true
	case 24: // STAP-A
		payload = payload[1:]
		for len(payload) > 0 {
			if len(payload) < 3 {
				return false
			}
			size := int(payload[0])<<8 | int(payload[1])
			if size == 0 || size > len(payload)-2 {
				return false
			}
			fn(payload[2] & 0x1F)
			payload = payload[size+2:]
		}
		return true
	case 28: // FU-A
		if len(payload) < 2 {
			return false
		}
		if payload[1]&0x80 != 0 {
			fn(payload[1] & 0x1F)
		}
		return true
	default:
		return true
	}
}
