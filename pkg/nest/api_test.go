package nest

import (
	"context"
	"testing"
	"time"
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
