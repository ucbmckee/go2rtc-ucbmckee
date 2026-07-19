package nest

import (
	"fmt"
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

	requested          atomic.Bool
	readyBeforeRequest atomic.Bool
	packets            atomic.Uint64
	idrs               atomic.Uint64
	sps                atomic.Uint64
	pps                atomic.Uint64
	psAfterRequest     atomic.Uint64
	idrsAfterRequest   atomic.Uint64
	malformed          atomic.Uint64
	plis               atomic.Uint64
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
			if p.requested.Load() {
				p.psAfterRequest.Add(1)
			}
		case h264.NALUTypePPS:
			p.havePPS = true
			p.pps.Add(1)
			if p.requested.Load() {
				p.psAfterRequest.Add(1)
			}
		case h264.NALUTypeIFrame:
			p.idrs.Add(1)
			if p.requested.Load() {
				p.idrsAfterRequest.Add(1)
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
	if p.firstReady.CompareAndSwap(0, int64(elapsed)) && !p.requested.Load() {
		p.readyBeforeRequest.Store(true)
	}
}

func (p *h264Probe) diagnostics() string {
	ready := "none"
	if elapsed := time.Duration(p.firstReady.Load()); elapsed > 0 {
		ready = elapsed.Round(time.Millisecond).String()
	}
	return fmt.Sprintf(
		"video_packets=%d video_idrs=%d video_sps=%d video_pps=%d video_ps_after_track=%d video_idrs_after_track=%d video_plis=%d video_malformed=%d video_ready_after=%s video_ready_before_track=%t",
		p.packets.Load(), p.idrs.Load(), p.sps.Load(), p.pps.Load(), p.psAfterRequest.Load(),
		p.idrsAfterRequest.Load(), p.plis.Load(), p.malformed.Load(), ready, p.readyBeforeRequest.Load(),
	)
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
