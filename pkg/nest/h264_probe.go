package nest

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/pion/rtp"
)

type h264Probe struct {
	media   *core.Media
	track   *core.Receiver
	sources []*core.Receiver

	handoffOnce    sync.Once
	handoffStarted atomic.Bool
	handoffAt      time.Time

	packets            atomic.Uint64
	idrs               atomic.Uint64
	sps                atomic.Uint64
	pps                atomic.Uint64
	psAfterHandoff     atomic.Uint64
	idrsAfterHandoff   atomic.Uint64
	malformed          atomic.Uint64
	plis               atomic.Uint64
	pliErrors          atomic.Uint64
	watchdogPLIs       atomic.Uint64
	watchdogRestarts   atomic.Uint64
	readyBeforeHandoff atomic.Bool
	readyAfterHandoff  atomic.Int64

	haveSPS  bool
	havePPS  bool
	afterSPS bool
	afterPPS bool
	parser   h264RTPParser
}

func newH264Probe(media *core.Media, codec *core.Codec, source *core.Receiver) *h264Probe {
	track := core.NewReceiver(media, codec)
	probe := &h264Probe{media: media, track: track, sources: []*core.Receiver{source}}
	forward := track.Input
	track.Input = func(packet *rtp.Packet) {
		probe.inspect(packet)
		forward(packet)
	}
	track.WithParent(&source.Node)
	return probe
}

func (p *h264Probe) addSource(source *core.Receiver) {
	for _, current := range p.sources {
		if current == source {
			return
		}
	}
	p.sources = append(p.sources, source)
	source.AppendChild(&core.Node{Input: p.track.Input})
}

func (p *h264Probe) beginHandoff() {
	p.handoffOnce.Do(func() {
		p.handoffAt = time.Now()
		p.handoffStarted.Store(true)
	})
}

func (p *h264Probe) inspect(packet *rtp.Packet) {
	p.packets.Add(1)
	if !p.parser.eachNALU(packet, p.inspectNALU) {
		p.malformed.Add(1)
	}
}

func (p *h264Probe) inspectNALU(typ byte) {
	afterHandoff := p.handoffStarted.Load()
	switch typ {
	case h264.NALUTypeSPS:
		p.haveSPS = true
		p.sps.Add(1)
		if afterHandoff {
			p.afterSPS = true
			p.psAfterHandoff.Add(1)
		}
	case h264.NALUTypePPS:
		p.havePPS = true
		p.pps.Add(1)
		if afterHandoff {
			p.afterPPS = true
			p.psAfterHandoff.Add(1)
		}
	case h264.NALUTypeIFrame:
		p.idrs.Add(1)
		if afterHandoff {
			p.idrsAfterHandoff.Add(1)
			if p.afterSPS && p.afterPPS {
				p.markHandoffReady()
			}
		} else if p.haveSPS && p.havePPS {
			p.readyBeforeHandoff.Store(true)
		}
	}
}

func (p *h264Probe) markHandoffReady() {
	elapsed := time.Since(p.handoffAt)
	if elapsed <= 0 {
		elapsed = 1
	}
	p.readyAfterHandoff.CompareAndSwap(0, int64(elapsed))
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
	WatchdogPLIs     uint64 `json:"video_watchdog_plis"`
	WatchdogRestarts uint64 `json:"video_watchdog_restarts"`
	Malformed        uint64 `json:"video_malformed"`
	ReadyAfter       string `json:"video_ready_after,omitempty"`
	ReadyBefore      bool   `json:"video_ready_before_handoff"`
}

func (p *h264Probe) diagnostics() h264Diagnostics {
	ready := "none"
	if elapsed := time.Duration(p.readyAfterHandoff.Load()); elapsed > 0 {
		ready = elapsed.Round(time.Millisecond).String()
	}
	return h264Diagnostics{
		Packets: p.packets.Load(), IDRs: p.idrs.Load(), SPS: p.sps.Load(), PPS: p.pps.Load(),
		PSAfterHandoff: p.psAfterHandoff.Load(), IDRsAfterHandoff: p.idrsAfterHandoff.Load(),
		PLIs: p.plis.Load(), PLIErrors: p.pliErrors.Load(), Malformed: p.malformed.Load(),
		WatchdogPLIs: p.watchdogPLIs.Load(), WatchdogRestarts: p.watchdogRestarts.Load(),
		ReadyAfter: ready, ReadyBefore: p.readyBeforeHandoff.Load(),
	}
}

type h264RTPParser struct {
	fragmentType byte
	timestamp    uint32
	sequence     uint16
	fragmentData bool
	fragmented   bool
}

func (p *h264RTPParser) eachNALU(packet *rtp.Packet, fn func(byte)) bool {
	if packet == nil || len(packet.Payload) == 0 {
		p.fragmented = false
		return false
	}

	typ := packet.Payload[0] & 0x1F
	if typ == 28 { // FU-A
		return p.fuA(packet, fn)
	}

	complete := !p.fragmented
	p.fragmented = false
	switch {
	case typ >= 1 && typ <= 23:
		fn(typ)
		return complete
	case typ == 24: // STAP-A
		valid := eachSTAPA(packet.Payload, fn)
		return complete && valid
	default:
		return false
	}
}

func (p *h264RTPParser) fuA(packet *rtp.Packet, fn func(byte)) bool {
	payload := packet.Payload
	if len(payload) < 2 {
		p.fragmented = false
		return false
	}

	header := payload[1]
	typ := header & 0x1F
	start, end := header&0x80 != 0, header&0x40 != 0
	if header&0x20 != 0 || typ == 0 || typ >= 24 || start && end {
		p.fragmented = false
		return false
	}

	if start {
		complete := !p.fragmented
		p.fragmented = true
		p.fragmentType = typ
		p.timestamp = packet.Timestamp
		p.sequence = packet.SequenceNumber
		p.fragmentData = len(payload) > 2
		return complete
	}

	if !p.fragmented || p.fragmentType != typ || p.timestamp != packet.Timestamp ||
		packet.SequenceNumber != p.sequence+1 {
		p.fragmented = false
		return false
	}

	p.sequence = packet.SequenceNumber
	p.fragmentData = p.fragmentData || len(payload) > 2
	if end {
		p.fragmented = false
		if p.fragmentData {
			fn(typ)
		}
	}
	return true
}

func eachSTAPA(payload []byte, fn func(byte)) bool {
	payload = payload[1:]
	if len(payload) == 0 {
		return false
	}
	for rest := payload; len(rest) > 0; {
		if len(rest) < 3 {
			return false
		}
		size := int(rest[0])<<8 | int(rest[1])
		if size == 0 || size > len(rest)-2 {
			return false
		}
		typ := rest[2] & 0x1F
		if typ == 0 || typ >= 24 {
			return false
		}
		rest = rest[size+2:]
	}
	for len(payload) > 0 {
		size := int(payload[0])<<8 | int(payload[1])
		fn(payload[2] & 0x1F)
		payload = payload[size+2:]
	}
	return true
}
