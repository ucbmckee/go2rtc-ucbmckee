package baichuan

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type shortTransport struct {
	bytes.Buffer
	step int
}

type failingTransport struct {
	closed chan struct{}
	once   sync.Once
}

func (t *failingTransport) Read([]byte) (int, error) {
	<-t.closed
	return 0, io.EOF
}

func (t *failingTransport) Write([]byte) (int, error)        { return 1, io.ErrUnexpectedEOF }
func (t *failingTransport) SetWriteDeadline(time.Time) error { return nil }
func (t *failingTransport) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}

func (t *shortTransport) Write(b []byte) (int, error) {
	if len(b) > t.step {
		b = b[:t.step]
	}
	return t.Buffer.Write(b)
}

func (t *shortTransport) SetWriteDeadline(time.Time) error { return nil }
func (t *shortTransport) Close() error                     { return nil }

func TestWriteFullHandlesShortWrites(t *testing.T) {
	conn := &shortTransport{step: 2}
	if err := writeFull(context.Background(), conn, time.Second, []byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	if actual := conn.String(); actual != "abcdef" {
		t.Fatalf("unexpected payload: %s", actual)
	}
}

func TestInterruptDeadlineWaitsForCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	cleanup := interruptDeadline(ctx, func(deadline time.Time) error {
		if !deadline.IsZero() {
			close(started)
			<-release
		}
		return nil
	})
	cancel()
	<-started
	done := make(chan struct{})
	go func() {
		cleanup()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("cleanup returned while deadline callback was active")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-done
}

func BenchmarkWriteFull(b *testing.B) {
	conn := &shortTransport{step: 1024}
	payload := make([]byte, 1024)
	b.ReportAllocs()
	for b.Loop() {
		conn.Reset()
		if err := writeFull(context.Background(), conn, time.Second, payload); err != nil {
			b.Fatal(err)
		}
	}
}

func TestClientCloseInterruptsFrameRead(t *testing.T) {
	conn, peer := net.Pipe()
	defer peer.Close()
	client := newClient(context.Background(), Config{}, conn)
	done := make(chan error, 1)
	go func() { done <- client.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("client close did not interrupt frame read")
	}
}

func TestClientPartialWriteShutsDown(t *testing.T) {
	cfg, err := NewConfig("camera.local", "admin", "password").normalized()
	if err != nil {
		t.Fatal(err)
	}
	client := newClient(context.Background(), cfg, &failingTransport{closed: make(chan struct{})})
	_, err = client.roundTrip(context.Background(), request{command: commandPing, class: classOffset})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("unexpected write error: %v", err)
	}
	select {
	case <-client.Done():
	default:
		t.Fatal("client remained active after partial write")
	}
	client.wg.Wait()
}
