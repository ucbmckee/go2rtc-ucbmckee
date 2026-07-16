package reolink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/AlexxIT/go2rtc/pkg/baichuan"
	"github.com/AlexxIT/go2rtc/pkg/core"
)

type Producer struct {
	core.Connection

	profile *Profile
	config  baichuan.Config
	channel uint8
	ctx     context.Context
	cancel  context.CancelFunc
	stop    sync.Once
	stopErr error
	send    atomic.Uint64

	failureMu sync.Mutex
	failure   error
	talkMu    sync.Mutex
	talk      *backchannel
}

func (p *Producer) Format(state fmt.State, _ rune) {
	profile := p.profile
	if profile == nil {
		_, _ = fmt.Fprintf(state, "reolink.Producer{ID:%d}", p.ID)
		return
	}
	_, _ = fmt.Fprintf(state, "reolink.Producer{ID:%d, Remote:%q, Channel:%d, Stream:%q, Generation:%d}",
		p.ID, p.RemoteAddr, profile.key.Channel, profile.key.Stream, profile.generation)
}

var _ core.Producer = (*Producer)(nil)
var _ core.Consumer = (*Producer)(nil)

func newProducer(s source, profile *Profile, talkFormat *baichuan.TalkFormat) *Producer {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Producer{
		Connection: core.Connection{
			ID: core.NewID(), FormatName: "baichuan", Protocol: s.protocol, RemoteAddr: s.remote,
		},
		profile: profile, config: s.config, channel: s.channel,
		ctx: ctx, cancel: cancel,
	}
	p.Medias = append(p.Medias, profile.pipeline.medias...)
	p.Receivers = append(p.Receivers, profile.receivers...)
	if talkFormat != nil {
		p.talk = &backchannel{producer: p, format: *talkFormat}
		p.Medias = append(p.Medias, &core.Media{
			Kind: core.KindAudio, Direction: core.DirectionSendonly,
			Codecs: talkCodecs(),
		})
	}
	return p
}

func (p *Producer) Start() error {
	p.profile.startProfile()
	select {
	case <-p.profile.done:
		if err := p.ctx.Err(); err != nil {
			return p.terminalError(err)
		}
		return p.terminalError(p.profile.err())
	case <-p.ctx.Done():
		<-p.profile.done
		return p.terminalError(p.ctx.Err())
	}
}

func (p *Producer) fail(err error) {
	if err == nil {
		return
	}
	p.failureMu.Lock()
	if p.failure == nil {
		p.failure = err
		p.cancel()
		if p.profile != nil {
			p.profile.cancel()
		}
	}
	p.failureMu.Unlock()
}

func (p *Producer) terminalError(fallback error) error {
	p.failureMu.Lock()
	defer p.failureMu.Unlock()
	if p.failure != nil {
		return p.failure
	}
	if fallback != nil {
		return fallback
	}
	return errors.New("reolink: profile stopped")
}

func (p *Producer) Stop() error {
	p.stop.Do(func() {
		p.cancel()
		p.profile.camera.registry.release(p.profile)
		p.talkMu.Lock()
		if p.talk != nil {
			p.stopErr = errors.Join(p.stopErr, p.talk.Close())
		}
		p.talkMu.Unlock()
		<-p.profile.done
		p.stopErr = errors.Join(p.stopErr, p.profile.closeError())
		p.profile.closeReceivers()
	})
	return p.stopErr
}

func (p *Producer) addSend(size int) {
	p.send.Add(uint64(size))
}

type producerDiagnostics struct {
	ID         uint32             `json:"id,omitempty"`
	FormatName string             `json:"format_name,omitempty"`
	Protocol   string             `json:"protocol,omitempty"`
	RemoteAddr string             `json:"remote_addr,omitempty"`
	Medias     []*core.Media      `json:"medias,omitempty"`
	Recv       uint64             `json:"bytes_recv,omitempty"`
	Send       uint64             `json:"bytes_send,omitempty"`
	Reolink    profileDiagnostics `json:"reolink"`
}

type profileDiagnostics struct {
	Endpoint        string                  `json:"endpoint"`
	Channel         uint8                   `json:"channel"`
	Profile         string                  `json:"profile"`
	State           string                  `json:"state"`
	Generation      uint64                  `json:"generation"`
	Profiles        int                     `json:"profiles"`
	MainSessions    int                     `json:"main_sessions"`
	OtherSessions   int                     `json:"other_sessions"`
	VideoCodec      string                  `json:"video_codec,omitempty"`
	AudioCodec      string                  `json:"audio_codec,omitempty"`
	CameraType      string                  `json:"camera_type,omitempty"`
	CameraModel     string                  `json:"camera_model,omitempty"`
	HardwareVersion string                  `json:"hardware_version,omitempty"`
	FirmwareVersion string                  `json:"firmware_version,omitempty"`
	DeviceInfo      string                  `json:"device_info_status,omitempty"`
	CapabilityInfo  string                  `json:"capability_status,omitempty"`
	Channels        []uint16                `json:"observed_channels,omitempty"`
	ReadOnly        []string                `json:"read_only_capabilities,omitempty"`
	ReadWrite       []string                `json:"read_write_capabilities,omitempty"`
	ReceivedBytes   uint64                  `json:"received_bytes"`
	ReceivedPacket  uint64                  `json:"received_packets"`
	Frames          uint64                  `json:"frames"`
	VideoFrames     uint64                  `json:"video_frames"`
	AudioFrames     uint64                  `json:"audio_frames,omitempty"`
	AudioSamples    uint64                  `json:"audio_samples,omitempty"`
	QueueHighWater  uint64                  `json:"preview_queue_high_water_bytes,omitempty"`
	QueueOverflows  uint64                  `json:"preview_queue_overflows,omitempty"`
	BoundaryAdjust  uint64                  `json:"boundary_adjustments,omitempty"`
	ParamVariants   uint64                  `json:"parameter_set_variants,omitempty"`
	ParamCandidates uint64                  `json:"parameter_set_candidates,omitempty"`
	VideoInvalid    uint64                  `json:"invalid_video_units,omitempty"`
	VideoDrops      uint64                  `json:"video_resync_drops,omitempty"`
	VideoRecoveries uint64                  `json:"video_resyncs,omitempty"`
	Transport       baichuan.TransportStats `json:"transport"`
	LastError       string                  `json:"last_error_class,omitempty"`
}

func (p *Producer) diagnostics() profileDiagnostics {
	profile := p.profile
	registry := profile.camera.registry
	p.failureMu.Lock()
	failureClass := errorClass(p.failure)
	p.failureMu.Unlock()
	registry.mu.Lock()
	profile.mu.Lock()
	var stats baichuan.PreviewStats
	if profile.input != nil {
		stats = profile.input.Stats()
	}
	videoFrames, audioFrames := profile.videoFrames.Load(), profile.audioFrames.Load()
	channels := make([]uint16, len(profile.discovery.capabilities.ObservedChannels))
	for i, channel := range profile.discovery.capabilities.ObservedChannels {
		channels[i] = uint16(channel)
	}
	d := profileDiagnostics{
		Endpoint: profile.camera.remote, Channel: profile.key.Channel, Profile: string(profile.key.Stream),
		State: profile.state.String(), Generation: profile.generation, Profiles: len(profile.camera.profiles),
		MainSessions: profile.camera.mainSessions, OtherSessions: profile.camera.otherSessions,
		VideoCodec: profile.pipeline.videoCodec, ReceivedBytes: profile.recvBytes.Load(),
		CameraType: profile.discovery.device.Type, CameraModel: profile.discovery.device.Model,
		HardwareVersion: profile.discovery.device.Hardware, FirmwareVersion: profile.discovery.device.Firmware,
		DeviceInfo: profile.discovery.deviceStatus, CapabilityInfo: profile.discovery.capabilityStatus,
		Channels:       channels,
		ReadOnly:       append([]string(nil), profile.discovery.capabilities.ReadOnly...),
		ReadWrite:      append([]string(nil), profile.discovery.capabilities.ReadWrite...),
		ReceivedPacket: profile.recvPackets.Load(), Frames: videoFrames + audioFrames,
		VideoFrames: videoFrames, AudioFrames: audioFrames, AudioSamples: profile.audioSamples.Load(),
		QueueHighWater: stats.QueueHighWater, QueueOverflows: stats.QueueOverflows,
		BoundaryAdjust:  stats.BoundaryAdjustments,
		ParamVariants:   profile.pipeline.paramVariants.Load(),
		ParamCandidates: profile.pipeline.paramCandidates.Load(),
		VideoInvalid:    profile.pipeline.videoInvalid.Load(),
		VideoDrops:      profile.pipeline.videoDrops.Load(),
		VideoRecoveries: profile.pipeline.videoRecoveries.Load(),
		Transport:       stats.Transport,
		LastError:       profile.errClass,
	}
	if profile.pipeline.audioMedia != nil {
		d.AudioCodec = profile.pipeline.audioMedia.Codecs[0].Name
	}
	if failureClass != "" {
		d.LastError = failureClass
	}
	profile.mu.Unlock()
	registry.mu.Unlock()
	return d
}

func (p *Producer) MarshalJSON() ([]byte, error) {
	return json.Marshal(&producerDiagnostics{
		ID: p.ID, FormatName: p.FormatName, Protocol: p.Protocol, RemoteAddr: p.RemoteAddr,
		Medias: p.Medias, Recv: p.profile.recvBytes.Load(), Send: p.send.Load(), Reolink: p.diagnostics(),
	})
}
