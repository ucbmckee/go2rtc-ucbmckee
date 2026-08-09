package reolink

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/baichuan"
	"github.com/rs/zerolog"
)

type inputResult struct {
	packet baichuan.MediaPacket
	err    error
}

type fakeProfileInput struct {
	results   chan inputResult
	done      chan struct{}
	once      sync.Once
	talk      baichuan.TalkFormat
	talkErr   error
	talks     atomic.Uint32
	closed    atomic.Bool
	closeErr  error
	discovery discoverySnapshot
}

func newFakeProfileInput() *fakeProfileInput {
	i := &fakeProfileInput{
		results: make(chan inputResult, 16), done: make(chan struct{}),
		talkErr: &baichuan.UnsupportedTalkError{Reason: "test camera"},
	}
	i.results <- inputResult{packet: testH264Keyframe(1_000_000)}
	i.results <- inputResult{packet: testAAC(1_000_000)}
	return i
}

func (i *fakeProfileInput) Read(ctx context.Context) (baichuan.MediaPacket, error) {
	select {
	case result := <-i.results:
		return result.packet, result.err
	case <-i.done:
		return baichuan.MediaPacket{}, context.Canceled
	case <-ctx.Done():
		return baichuan.MediaPacket{}, ctx.Err()
	}
}

func (i *fakeProfileInput) ProbeTalk(context.Context, uint8) (baichuan.TalkFormat, error) {
	i.talks.Add(1)
	return i.talk, i.talkErr
}

func (i *fakeProfileInput) Discovery() discoverySnapshot {
	return i.discovery
}

func (i *fakeProfileInput) Close() error {
	i.once.Do(func() {
		i.closed.Store(true)
		close(i.done)
	})
	return i.closeErr
}

type fakeProfileOpener struct {
	mu        sync.Mutex
	inputs    []*fakeProfileInput
	talk      *baichuan.TalkFormat
	discovery discoverySnapshot
}

func (o *fakeProfileOpener) open(context.Context, *Camera, source) (profileInput, error) {
	input := newFakeProfileInput()
	if o.talk != nil {
		input.talk = *o.talk
		input.talkErr = nil
	}
	input.discovery = o.discovery
	o.mu.Lock()
	o.inputs = append(o.inputs, input)
	o.mu.Unlock()
	return input, nil
}

func (o *fakeProfileOpener) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.inputs)
}

func (o *fakeProfileOpener) input(index int) *fakeProfileInput {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.inputs[index]
}

func testRegistry(open profileOpener, budget sessionBudget) *Registry {
	secret := sha256.Sum256([]byte("test registry secret"))
	return newRegistry(zerolog.Nop(), secret, open, budget)
}

func testH264Keyframe(timestamp uint32) baichuan.MediaPacket {
	return baichuan.MediaPacket{
		Kind: baichuan.MediaVideoI, Codec: "H264", Timestamp: timestamp,
		Data: []byte{
			0, 0, 0, 1, 0x67, 0x64, 0, 0x29,
			0, 0, 0, 1, 0x68, 0,
			0, 0, 0, 1, 0x65, 0,
		},
	}
}

func testAAC(timestamp uint32) baichuan.MediaPacket {
	data := []byte{0xff, 0xf1, 0x60, 0x40, 0, 0, 0xfc, 1, 2, 3}
	aac.WriteADTSSize(data, uint16(len(data)))
	return baichuan.MediaPacket{Kind: baichuan.MediaAAC, Timestamp: timestamp, Data: data}
}

func testH264PFrame(timestamp uint32) baichuan.MediaPacket {
	return baichuan.MediaPacket{
		Kind: baichuan.MediaVideoP, Codec: "H264", Timestamp: timestamp,
		Data: []byte{0, 0, 0, 1, 0x41, 1, 2, 3},
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRegistryKeepsConfiguredSourcesIndependent(t *testing.T) {
	opener := &fakeProfileOpener{}
	registry := testRegistry(opener.open, sessionBudget{main: 2, other: 10})
	p1, err := registry.Dial("reolink://admin:secret@CAMERA.EXAMPLE./main?backchannel=0")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := registry.Dial("reolink://admin:secret@camera.example:9000?stream=main&backchannel=0")
	if err != nil {
		t.Fatal(err)
	}
	if opener.count() != 2 || p1.profile == p2.profile || p1.profile.camera != p2.profile.camera {
		t.Fatalf("sources not isolated within one camera: opens=%d same_profile=%t same_camera=%t",
			opener.count(), p1.profile == p2.profile, p1.profile.camera == p2.profile.camera)
	}
	if p1.Receivers[0] == p2.Receivers[0] {
		t.Fatal("configured sources shared a core receiver")
	}
	if err = p1.Stop(); err != nil {
		t.Fatal(err)
	}
	if !opener.input(0).closed.Load() {
		t.Fatal("producer stop returned before its input closed")
	}
	if opener.input(1).closed.Load() {
		t.Fatal("one source closed another source")
	}
	if err = p2.Stop(); err != nil {
		t.Fatal(err)
	}
	if !opener.input(1).closed.Load() {
		t.Fatal("second producer stop returned before its input closed")
	}
}

func TestRegistryIsolatesCredentials(t *testing.T) {
	opener := &fakeProfileOpener{}
	registry := testRegistry(opener.open, sessionBudget{main: 2, other: 10})
	p1, err := registry.Dial("reolink://admin:first@camera/main?backchannel=0")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := registry.Dial("reolink://admin:second@camera/main?backchannel=0")
	if err != nil {
		t.Fatal(err)
	}
	if opener.count() != 2 || p1.profile == p2.profile {
		t.Fatal("different credentials shared camera state")
	}
	_ = p1.Stop()
	_ = p2.Stop()
	waitFor(t, func() bool { return opener.input(0).closed.Load() && opener.input(1).closed.Load() })
}

func TestRegistryEnforcesCameraSessionBudget(t *testing.T) {
	opener := &fakeProfileOpener{}
	registry := testRegistry(opener.open, sessionBudget{main: 2, other: 1})
	p1, err := registry.Dial("reolink://admin:secret@camera/main?channel=0&backchannel=0")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := registry.Dial("reolink://admin:secret@camera/main?channel=1&backchannel=0")
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.Dial("reolink://admin:secret@camera/main?channel=2&backchannel=0")
	var limit *SessionLimitError
	if !errors.As(err, &limit) || limit.Limit != 2 {
		t.Fatalf("unexpected main-session result: %v", err)
	}
	sub, err := registry.Dial("reolink://admin:secret@camera/sub?channel=0&backchannel=0")
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.Dial("reolink://admin:secret@camera/extern?channel=0&backchannel=0")
	if !errors.As(err, &limit) || limit.Limit != 1 {
		t.Fatalf("unexpected non-main-session result: %v", err)
	}
	_ = p1.Stop()
	_ = p2.Stop()
	_ = sub.Stop()
	waitFor(t, func() bool {
		return opener.input(0).closed.Load() && opener.input(1).closed.Load() && opener.input(2).closed.Load()
	})
}

func TestRegistryFailureCreatesNewGeneration(t *testing.T) {
	opener := &fakeProfileOpener{}
	registry := testRegistry(opener.open, sessionBudget{main: 1, other: 10})
	p1, err := registry.Dial("reolink://admin:secret@camera/main?backchannel=0")
	if err != nil {
		t.Fatal(err)
	}
	startErr := make(chan error, 1)
	go func() { startErr <- p1.Start() }()
	opener.input(0).results <- inputResult{err: io.ErrUnexpectedEOF}
	if err = <-startErr; !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("unexpected terminal error: %v", err)
	}
	registry.mu.Lock()
	mainSessions := p1.profile.camera.mainSessions
	registry.mu.Unlock()
	if mainSessions != 0 {
		t.Fatalf("producer start returned with %d main sessions active", mainSessions)
	}
	p2, err := registry.Dial("reolink://admin:secret@camera/main?backchannel=0")
	if err != nil {
		t.Fatal(err)
	}
	if p2.profile.generation != p1.profile.generation+1 || opener.count() != 2 {
		t.Fatalf("replacement generation=%d opens=%d", p2.profile.generation, opener.count())
	}
	if err = p1.Stop(); err != nil {
		t.Fatal(err)
	}
	if opener.input(1).closed.Load() {
		t.Fatal("old generation release closed replacement")
	}
	if err = p2.Stop(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, opener.input(1).closed.Load)
}

func TestRegistryConcurrentDialHonorsSessionBudget(t *testing.T) {
	opener := &fakeProfileOpener{}
	registry := testRegistry(opener.open, sessionBudget{main: 2, other: 10})
	const count = 32
	producers := make(chan *Producer, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			producer, err := registry.Dial("reolink://admin:secret@camera/main?backchannel=0")
			if err != nil {
				errs <- err
				return
			}
			producers <- producer
		}()
	}
	wg.Wait()
	close(producers)
	close(errs)
	accepted := 0
	for producer := range producers {
		accepted++
		if err := producer.Stop(); err != nil {
			t.Error(err)
		}
	}
	rejected := 0
	for err := range errs {
		var limit *SessionLimitError
		if !errors.As(err, &limit) {
			t.Errorf("unexpected dial error: %v", err)
		}
		rejected++
	}
	if accepted != 2 || rejected != count-2 || opener.count() != 2 {
		t.Fatalf("accepted=%d rejected=%d opens=%d", accepted, rejected, opener.count())
	}
	waitFor(t, func() bool { return opener.input(0).closed.Load() && opener.input(1).closed.Load() })
}

func TestRegistryScopesTalkProbeToConfiguredSource(t *testing.T) {
	opener := &fakeProfileOpener{}
	registry := testRegistry(opener.open, sessionBudget{main: 3, other: 10})
	const base = "reolink://admin:secret@camera/main"
	withoutTalk, err := registry.Dial(base + "?backchannel=0")
	if err != nil {
		t.Fatal(err)
	}
	first, err := registry.Dial(base)
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.Dial(base)
	if err != nil {
		t.Fatal(err)
	}
	if opener.input(0).talks.Load() != 0 || opener.input(1).talks.Load() != 1 ||
		opener.input(2).talks.Load() != 1 || withoutTalk.talk != nil || first.talk != nil || second.talk != nil {
		t.Fatalf("unexpected talk probes: disabled=%d first=%d second=%d",
			opener.input(0).talks.Load(), opener.input(1).talks.Load(), opener.input(2).talks.Load())
	}
	_ = withoutTalk.Stop()
	_ = first.Stop()
	_ = second.Stop()
	waitFor(t, func() bool {
		return opener.input(0).closed.Load() && opener.input(1).closed.Load() && opener.input(2).closed.Load()
	})
}

func TestSourceFormattingRedactsCredentials(t *testing.T) {
	s, err := parseURL("reolink://sentinel-user:sentinel-pass@camera/main")
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		text := fmt.Sprintf(format, s)
		if stringsContainsAny(text, "sentinel-user", "sentinel-pass") {
			t.Fatalf("credentials exposed by %s: %s", format, text)
		}
	}
}

func TestUIDCameraIdentityIsSecretAndRouteIndependent(t *testing.T) {
	secret := [sha256.Size]byte{1}
	registry := newRegistry(zerolog.Nop(), secret, nil, sessionBudget{})
	first, err := parseURL("reolink://admin:pass@ABC1234567890001/main?transport=uid")
	if err != nil {
		t.Fatal(err)
	}
	second, err := parseURL("reolink://admin:pass@ABC1234567890002/main?transport=uid")
	if err != nil {
		t.Fatal(err)
	}
	firstKey, secondKey := registry.cameraKey(first), registry.cameraKey(second)
	if firstKey == secondKey || firstKey.endpoint != "uid" || secondKey.endpoint != "uid" {
		t.Fatal("UID camera identities were not isolated")
	}
	selected, err := parseURL("reolink://admin:pass@ABC1234567890001/main?transport=uid&local=192.0.2.28")
	if err != nil {
		t.Fatal(err)
	}
	if firstKey != registry.cameraKey(selected) {
		t.Fatal("UID interface selection changed camera identity")
	}
	routed, err := parseURL("reolink://admin:pass@ABC1234567890001/main?transport=uid&broadcast=198.51.100.255")
	if err != nil {
		t.Fatal(err)
	}
	if firstKey != registry.cameraKey(routed) {
		t.Fatal("UID broadcast selection changed camera identity")
	}
}

func TestUIDRoutingSharesSessionBudget(t *testing.T) {
	opener := &fakeProfileOpener{}
	registry := testRegistry(opener.open, sessionBudget{main: 1, other: 10})
	producer, err := registry.Dial("reolink://admin:pass@ABC1234567890001/main?transport=uid&backchannel=0")
	if err != nil {
		t.Fatal(err)
	}
	defer producer.Stop()

	_, err = registry.Dial("reolink://admin:pass@ABC1234567890001/main?transport=uid&local=192.0.2.28&backchannel=0")
	var limit *SessionLimitError
	if !errors.As(err, &limit) || limit.Stream != baichuan.StreamMain || limit.Limit != 1 {
		t.Fatalf("UID routing bypassed camera budget: %v", err)
	}
}

func TestAggregateFormattingRedactsCredentials(t *testing.T) {
	opener := &fakeProfileOpener{}
	registry := testRegistry(opener.open, sessionBudget{main: 2, other: 10})
	producer, err := registry.Dial("reolink://sentinel-user:sentinel-pass@camera/main?backchannel=0")
	if err != nil {
		t.Fatal(err)
	}
	defer producer.Stop()

	for _, value := range []any{registry, producer.profile.camera, producer.profile, producer} {
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
			text := fmt.Sprintf(format, value)
			if stringsContainsAny(text, "sentinel-user", "sentinel-pass") {
				t.Fatalf("credentials exposed by %T with %s: %s", value, format, text)
			}
		}
	}
}

func stringsContainsAny(value string, items ...string) bool {
	for _, item := range items {
		if strings.Contains(value, item) {
			return true
		}
	}
	return false
}
