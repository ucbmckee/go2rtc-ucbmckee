package streams

import (
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

type reconnectTestProducer struct {
	core.Connection
	start chan struct{}
}

func (p *reconnectTestProducer) Start() error {
	<-p.start
	return nil
}

func (p *reconnectTestProducer) Stop() error {
	return p.Connection.Stop()
}

func TestProducerReconnectMovesCompatibleReceivers(t *testing.T) {
	oldCodec := &core.Codec{Name: core.CodecOpus, ClockRate: 48000, Channels: 2}
	oldA := core.NewReceiver(nil, oldCodec)
	oldB := core.NewReceiver(nil, oldCodec.Clone())
	senderA := core.NewSender(nil, oldCodec.Clone()).WithParent(oldA)
	senderB := core.NewSender(nil, oldCodec.Clone()).WithParent(oldB)
	old := &reconnectTestProducer{Connection: core.Connection{Receivers: []*core.Receiver{oldA, oldB}}}

	newCodec := oldCodec.Clone()
	media := &core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{newCodec}}
	target := core.NewReceiver(media, newCodec)
	existing := core.NewSender(nil, newCodec.Clone()).WithParent(target)
	next := &reconnectTestProducer{
		Connection: core.Connection{Medias: []*core.Media{media}, Receivers: []*core.Receiver{target}},
		start:      make(chan struct{}),
	}

	const scheme = "reconnect-test"
	handlers[scheme] = func(string) (core.Producer, error) { return next, nil }
	t.Cleanup(func() { delete(handlers, scheme) })

	p := &Producer{
		url:       scheme + ":source",
		conn:      old,
		receivers: []*core.Receiver{oldA, oldA, oldB},
		state:     stateStart,
		workerID:  1,
	}
	p.reconnect(1, 0)

	require.Same(t, target, p.receivers[0])
	require.Same(t, target, p.receivers[1])
	require.Same(t, target, p.receivers[2])
	target.Input(&core.Packet{Payload: []byte{1}})
	require.Equal(t, 1, senderA.Packets)
	require.Equal(t, 1, senderB.Packets)
	require.Equal(t, 1, existing.Packets)

	p.mu.Lock()
	p.workerID++
	p.mu.Unlock()
	close(next.start)
}
