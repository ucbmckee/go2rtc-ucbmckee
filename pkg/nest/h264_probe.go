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
	forward core.HandlerFunc
	sources []*h264ProbeSource

	active atomic.Uint64
	closed atomic.Bool

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
	readyIDRs          atomic.Uint64
}

type h264ProbeSource struct {
	id           uint32
	track        *core.Receiver
	parser       h264RTPParser
	params       uint8
	after        uint8
	payload      uint16
	handoffReset bool
	ready        bool
	opening      bool
}

const (
	h264SPSSeen = 1 << iota
	h264PPSSeen
)

func newH264Probe(media *core.Media, codec *core.Codec, source *core.Receiver) *h264Probe {
	track := core.NewReceiver(media, codec)
	input := &h264ProbeSource{id: 1, track: source}
	probe := &h264Probe{media: media, track: track, forward: track.Input, sources: []*h264ProbeSource{input}}
	track.Input = probe.input(input)
	track.WithParent(&source.Node)
	return probe
}

func (p *h264Probe) addSource(source *core.Receiver) {
	for _, current := range p.sources {
		if current.track == source {
			return
		}
	}
	input := &h264ProbeSource{id: uint32(len(p.sources) + 1), track: source}
	p.sources = append(p.sources, input)
	source.AppendChild(&core.Node{Input: p.input(input)})
}

func (p *h264Probe) input(source *h264ProbeSource) core.HandlerFunc {
	return func(packet *rtp.Packet) {
		if packet == nil {
			p.malformed.Add(1)
			return
		}
		if p.closed.Load() {
			return
		}
		stream := uint64(source.id)<<32 | uint64(packet.SSRC)
		active := p.active.Load()
		if active == 0 {
			p.active.CompareAndSwap(0, stream)
			active = p.active.Load()
		}
		if active != stream {
			return
		}
		forward := p.inspectSource(source, packet)
		if forward && !p.closed.Load() {
			p.forward(packet)
		}
	}
}

func (p *h264Probe) close() {
	p.closed.Store(true)
}

func (p *h264Probe) beginHandoff() {
	p.handoffOnce.Do(func() {
		p.handoffAt = time.Now()
		p.handoffStarted.Store(true)
	})
}

func (p *h264Probe) inspectSource(source *h264ProbeSource, packet *rtp.Packet) bool {
	p.packets.Add(1)
	payload := uint16(packet.PayloadType) + 1
	if source.payload != payload {
		source.parser = h264RTPParser{}
		source.params, source.after = 0, 0
		source.ready, source.opening = false, false
		source.payload = payload
	}
	afterHandoff := p.handoffStarted.Load()
	if afterHandoff && !source.handoffReset {
		source.parser = h264RTPParser{}
		source.after = 0
		source.ready, source.opening = false, false
		source.handoffReset = true
	}
	before := source.after
	valid := source.parser.eachNALU(packet, func(typ byte) { p.inspectNALU(source, typ, afterHandoff) })
	if !valid {
		p.malformed.Add(1)
		if source.opening {
			source.ready, source.opening = false, false
		}
	}
	if source.opening && !source.parser.fragmented {
		source.ready, source.opening = false, false
	}
	if afterHandoff && !source.ready && source.after == h264SPSSeen|h264PPSSeen &&
		source.parser.fragmented && source.parser.fragmentType == h264.NALUTypeIFrame {
		source.ready, source.opening = true, true
	}
	return !afterHandoff || source.ready || source.after != before ||
		(valid || source.parser.fragmented) && h264ParameterFragment(packet)
}

func (p *h264Probe) inspectNALU(source *h264ProbeSource, typ byte, afterHandoff bool) {
	var param uint8
	switch typ {
	case h264.NALUTypeSPS:
		param = h264SPSSeen
		p.sps.Add(1)
	case h264.NALUTypePPS:
		param = h264PPSSeen
		p.pps.Add(1)
	case h264.NALUTypeIFrame:
		p.idrs.Add(1)
		if afterHandoff {
			p.idrsAfterHandoff.Add(1)
			if source.after == h264SPSSeen|h264PPSSeen {
				source.ready, source.opening = true, false
				p.readyIDRs.Add(1)
				p.markHandoffReady()
			}
		} else if source.params == h264SPSSeen|h264PPSSeen {
			p.readyBeforeHandoff.Store(true)
		}
		return
	default:
		return
	}
	source.params |= param
	if afterHandoff {
		source.after |= param
		p.psAfterHandoff.Add(1)
	}
}

func h264ParameterFragment(packet *rtp.Packet) bool {
	if len(packet.Payload) == 0 {
		return false
	}
	typ := packet.Payload[0] & 0x1F
	if typ == h264.NALUTypeSPS || typ == h264.NALUTypePPS {
		return len(packet.Payload) > 1
	}
	if typ != 28 || len(packet.Payload) < 2 {
		return false
	}
	typ = packet.Payload[1] & 0x1F
	header := packet.Payload[1]
	return header&0x20 == 0 && header&0xC0 != 0xC0 &&
		(typ == h264.NALUTypeSPS || typ == h264.NALUTypePPS)
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
		if len(packet.Payload) < 2 {
			return false
		}
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
		if size < 2 || size > len(rest)-2 {
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
