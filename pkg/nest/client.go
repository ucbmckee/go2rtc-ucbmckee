package nest

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/rtsp"
	"github.com/AlexxIT/go2rtc/pkg/webrtc"
	"github.com/pion/rtcp"
	pion "github.com/pion/webrtc/v4"
)

type WebRTCClient struct {
	conn       *webrtc.Conn
	pc         *pion.PeerConnection
	api        *API
	h264       atomic.Pointer[h264Probe]
	pliOnce    sync.Once
	requestPLI func(*core.Media) error
	done       chan struct{}
	stopOnce   sync.Once
	stopErr    error
}

type RTSPClient struct {
	conn *rtsp.Conn
	api  *API
}

func Dial(rawURL string) (core.Producer, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, errors.New("nest: invalid source")
	}

	query := u.Query()
	clientID := query.Get("client_id")
	clientSecret := query.Get("client_secret")
	refreshToken := query.Get("refresh_token")
	projectID := query.Get("project_id")
	deviceID := query.Get("device_id")

	if clientID == "" || clientSecret == "" || refreshToken == "" || projectID == "" || deviceID == "" {
		return nil, errors.New("nest: wrong query")
	}

	nestAPI, err := NewAPI(clientID, clientSecret, refreshToken)
	if err != nil {
		return nil, err
	}

	protocols := strings.Split(query.Get("protocols"), ",")
	if len(protocols) > 0 && protocols[0] == "RTSP" {
		return rtspConn(nestAPI, rawURL, projectID, deviceID)
	}

	// Default to WEB_RTC for backwards compatibility.
	return rtcConn(nestAPI, rawURL, projectID, deviceID)
}

func (c *WebRTCClient) GetMedias() []*core.Media {
	return c.conn.GetMedias()
}

func (c *WebRTCClient) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	if probe := c.h264.Load(); probe != nil && probe.media == media && codec != nil && probe.track.Codec.Match(codec) {
		// WebRTC Conn calls GetTrack while binding the remote track, before a
		// downstream consumer exists. The bounded PLI sequence is intentional:
		// Nest may otherwise withhold SPS/PPS/IDR until explicitly requested.
		probe.handoffStarted.Store(true)
		c.pliOnce.Do(func() { go c.requestKeyframes(probe) })
		return probe.track, nil
	}
	return c.conn.GetTrack(media, codec)
}

func (c *WebRTCClient) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	return c.conn.AddTrack(media, codec, track)
}

func (c *WebRTCClient) Start() error {
	c.api.StartExtendStreamTimer()
	return c.conn.Start()
}

func (c *WebRTCClient) Stop() error {
	c.stopOnce.Do(func() {
		close(c.done)
		c.api.StopExtendStreamTimer()
		c.stopErr = c.conn.Stop()
		// Release Google's media session after the local peer is closed. A
		// failed remote cleanup must not turn a successful local stop into an
		// error; the session will still expire server-side.
		_ = c.api.StopWebRTCStream()
	})
	return c.stopErr
}

func (c *WebRTCClient) MarshalJSON() ([]byte, error) {
	raw, err := c.conn.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if probe := c.h264.Load(); probe != nil {
		fields["nest"], err = json.Marshal(probe.diagnostics())
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(fields)
}

func rtcConn(nestAPI *API, rawURL, projectID, deviceID string) (*WebRTCClient, error) {
	rtcAPI, err := webrtc.NewAPI()
	if err != nil {
		return nil, err
	}

	pc, err := rtcAPI.NewPeerConnection(pion.Configuration{})
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = pc.Close()
		}
	}()

	conn := webrtc.NewConn(pc)
	conn.FormatName = "nest/webrtc"
	conn.Mode = core.ModeActiveProducer
	conn.Protocol = "http"
	conn.URL = redactedSource(rawURL)
	client := &WebRTCClient{conn: conn, pc: pc, api: nestAPI, done: make(chan struct{})}

	// https://developers.google.com/nest/device-access/traits/device/camera-live-stream#generatewebrtcstream-request-fields
	medias := []*core.Media{
		{Kind: core.KindAudio, Direction: core.DirectionRecvonly},
		{Kind: core.KindVideo, Direction: core.DirectionRecvonly},
		{Kind: "app"}, // important for Nest
	}

	offer, err := conn.CreateCompleteOffer(medias)
	if err != nil {
		return nil, err
	}
	answer, err := nestAPI.ExchangeSDP(projectID, deviceID, offer)
	if err != nil {
		return nil, err
	}
	if err = conn.SetAnswer(answer); err != nil {
		return nil, err
	}
	if err = client.prepareH264Probe(); err != nil {
		return nil, err
	}

	ok = true
	return client, nil
}

func (c *WebRTCClient) prepareH264Probe() error {
	for _, media := range c.conn.Medias {
		if media.Kind != core.KindVideo || media.Direction != core.DirectionRecvonly {
			continue
		}
		for _, codec := range media.Codecs {
			if codec.Name != core.CodecH264 {
				continue
			}
			source, err := c.conn.GetTrack(media, codec)
			if err != nil {
				return err
			}
			c.h264.Store(newH264Probe(media, codec, source))
			return nil
		}
	}
	return nil
}

func (c *WebRTCClient) requestKeyframes(probe *h264Probe) {
	c.requestKeyframesAfter(probe, []time.Duration{
		100 * time.Millisecond, 500 * time.Millisecond, 1500 * time.Millisecond,
	})
}

func (c *WebRTCClient) requestKeyframesAfter(probe *h264Probe, delays []time.Duration) {
	for _, delay := range delays {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
			select {
			case <-c.done:
				return
			default:
			}
			if err := c.requestKeyframe(probe.media); err != nil {
				probe.pliErrors.Add(1)
				continue
			}
			probe.plis.Add(1)
		case <-c.done:
			timer.Stop()
			return
		}
	}
}

func (c *WebRTCClient) requestKeyframe(media *core.Media) error {
	if c.requestPLI != nil {
		return c.requestPLI(media)
	}
	if c.pc == nil {
		return errors.New("nest: WebRTC peer unavailable")
	}
	if media.Kind != core.KindVideo {
		return errors.New("nest: PLI requires video")
	}
	for _, transceiver := range c.pc.GetTransceivers() {
		if transceiver.Mid() != media.ID || transceiver.Receiver() == nil {
			continue
		}
		remote := transceiver.Receiver().Track()
		if remote != nil {
			return c.pc.WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{MediaSSRC: uint32(remote.SSRC())},
			})
		}
	}
	return errors.New("nest: WebRTC video receiver unavailable")
}

func rtspConn(nestAPI *API, rawURL, projectID, deviceID string) (*RTSPClient, error) {
	rtspURL, err := nestAPI.GenerateRtspStream(projectID, deviceID)
	if err != nil {
		return nil, err
	}

	rtspClient := rtsp.NewClient(rtspURL)
	if err := rtspClient.Dial(); err != nil {
		return nil, err
	}
	if err := rtspClient.Describe(); err != nil {
		return nil, err
	}
	rtspClient.Connection.URL = redactedSource(rawURL)

	return &RTSPClient{conn: rtspClient, api: nestAPI}, nil
}

func redactedSource(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" {
		return "nest:"
	}
	return u.Scheme + ":"
}

func (c *RTSPClient) GetMedias() []*core.Media {
	result := c.conn.GetMedias()
	return result
}

func (c *RTSPClient) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	return c.conn.GetTrack(media, codec)
}

func (c *RTSPClient) Start() error {
	c.api.StartExtendStreamTimer()
	return c.conn.Start()
}

func (c *RTSPClient) Stop() error {
	c.api.StopRTSPStream()
	c.api.StopExtendStreamTimer()
	return c.conn.Stop()
}

func (c *RTSPClient) MarshalJSON() ([]byte, error) {
	return c.conn.MarshalJSON()
}
