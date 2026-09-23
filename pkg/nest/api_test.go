package nest

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	go2rtcwebrtc "github.com/AlexxIT/go2rtc/pkg/webrtc"
	pion "github.com/pion/webrtc/v4"
)

func TestNewAPIIsolatesStreamState(t *testing.T) {
	key := authKey{clientID: "test-client", clientSecret: "test-secret", refreshToken: "test-refresh"}
	state := &authState{
		clientID: "test-client", clientSecret: "test-secret", refreshToken: "test-refresh",
		token: "cached-token", expiresAt: time.Now().Add(time.Hour),
	}

	cacheMu.Lock()
	previous := cache[key]
	cache[key] = state
	cacheMu.Unlock()
	t.Cleanup(func() {
		cacheMu.Lock()
		if previous == nil {
			delete(cache, key)
		} else {
			cache[key] = previous
		}
		cacheMu.Unlock()
	})

	first, err := NewAPI("test-client", "test-secret", "test-refresh")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewAPI("test-client", "test-secret", "test-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first.auth != second.auth {
		t.Fatal("same credentials did not produce independent stream APIs with shared auth")
	}

	first.StreamProjectID = "project-a"
	first.StreamDeviceID = "device-a"
	first.StreamSessionID = "session-a"
	first.StreamExpiresAt = time.Now().Add(time.Hour)
	first.StartExtendStreamTimer()
	second.StreamProjectID = "project-b"
	second.StreamDeviceID = "device-b"
	second.StreamSessionID = "session-b"
	second.StreamExpiresAt = time.Now().Add(time.Hour)
	second.StartExtendStreamTimer()
	first.StopExtendStreamTimer()
	defer second.StopExtendStreamTimer()

	if second.StreamProjectID != "project-b" || second.StreamDeviceID != "device-b" ||
		second.StreamSessionID != "session-b" || second.extendTimer == nil {
		t.Fatal("stream state or timer was shared between API instances")
	}
}

func TestExtendStreamTimerConcurrentStop(t *testing.T) {
	api := &API{StreamExpiresAt: time.Now().Add(time.Hour)}
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			api.StartExtendStreamTimer()
			api.StopExtendStreamTimer()
			done <- struct{}{}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
	api.StopExtendStreamTimer()
}

func TestExtendStreamTimerRepeats(t *testing.T) {
	api := &API{StreamExpiresAt: time.Now().Add(-time.Minute)}
	calls := 0
	api.extendRequest = func(context.Context) error {
		calls++
		if calls == 1 {
			api.streamMu.Lock()
			api.StreamExpiresAt = time.Now().Add(-time.Second)
			api.streamMu.Unlock()
		}
		return nil
	}
	api.StartExtendStreamTimer()
	t.Cleanup(api.StopExtendStreamTimer)

	api.extendMu.Lock()
	finished := api.extendFinished
	api.extendMu.Unlock()
	if finished != nil {
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("stream extension didn't stop on unchanged expiry")
		}
	}
	if calls != 2 {
		t.Fatalf("wrong stream extension count: %d", calls)
	}
}

func TestExtendStreamTimerJoinsCanceledRequest(t *testing.T) {
	started := make(chan struct{})
	api := &API{
		StreamExpiresAt: time.Now().Add(-time.Minute),
		extendRequest: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
	}
	api.StartExtendStreamTimer()
	<-started

	stopped := make(chan struct{})
	go func() {
		api.StopExtendStreamTimer()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("timer stop did not join canceled request")
	}
}

func TestStopWebRTCStreamClearsSessionAfterRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	useHTTPTransport(t, func(*http.Request) (*http.Response, error) {
		close(started)
		<-release
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       http.NoBody,
		}, nil
	})

	api := testStreamAPI()
	done := make(chan error, 1)
	go func() { done <- api.StopWebRTCStream() }()
	<-started
	api.streamMu.RLock()
	sessionID := api.StreamSessionID
	api.streamMu.RUnlock()
	if sessionID != "session" {
		t.Fatal("session was cleared before the stop request completed")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	api.streamMu.RLock()
	defer api.streamMu.RUnlock()
	if api.StreamProjectID != "" || api.StreamDeviceID != "" || api.StreamSessionID != "" || api.streamGeneration != 2 {
		t.Fatal("completed stop did not clear the session")
	}
}

func TestStopWebRTCStreamRetainsSessionOnFailure(t *testing.T) {
	useHTTPTransport(t, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("test request failure")
	})
	api := testStreamAPI()
	if err := api.StopWebRTCStream(); err == nil {
		t.Fatal("request failure was not returned")
	}
	if api.StreamSessionID != "session" || api.streamGeneration != 1 {
		t.Fatal("failed stop discarded the session identity")
	}
}

func TestStopWebRTCStreamDoesNotClearReplacement(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	useHTTPTransport(t, func(*http.Request) (*http.Response, error) {
		close(started)
		<-release
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Status:     "404 Not Found",
			Body:       http.NoBody,
		}, nil
	})

	api := testStreamAPI()
	done := make(chan error, 1)
	go func() { done <- api.StopWebRTCStream() }()
	<-started
	api.streamMu.Lock()
	api.StreamSessionID = "replacement"
	api.streamGeneration++
	api.streamMu.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if api.StreamSessionID != "replacement" || api.streamGeneration != 2 {
		t.Fatal("old stop request cleared the replacement session")
	}
}

func TestWebRTCClientStartWaitsForRemoteCleanup(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	useHTTPTransport(t, func(*http.Request) (*http.Response, error) {
		close(started)
		<-release
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       http.NoBody,
		}, nil
	})

	rtcAPI, err := go2rtcwebrtc.NewAPI()
	if err != nil {
		t.Fatal(err)
	}
	pc, err := rtcAPI.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	conn := go2rtcwebrtc.NewConn(pc)
	client := &WebRTCClient{conn: conn, api: testStreamAPI(), done: make(chan struct{})}
	client.watchPeer()
	done := make(chan error, 1)
	go func() { done <- client.Start() }()
	if err = conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("remote cleanup did not start")
	}
	select {
	case <-done:
		t.Fatal("Start returned before remote cleanup completed")
	default:
	}
	close(release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func testStreamAPI() *API {
	return &API{
		Token: "token", StreamProjectID: "project", StreamDeviceID: "device",
		StreamSessionID: "session", StreamExpiresAt: time.Now().Add(time.Hour), streamGeneration: 1,
	}
}

func useHTTPTransport(t *testing.T, fn roundTripFunc) {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = fn
	t.Cleanup(func() { http.DefaultTransport = previous })
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}
