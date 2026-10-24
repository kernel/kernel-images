package api

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

func TestPTYOutputRetainsLatestBytes(t *testing.T) {
	var output ptyOutput
	output.write(bytes.Repeat([]byte("a"), ptyReplayBytes-2))
	output.write([]byte("tail"))

	var replay []byte
	output.attach(func(data []byte) error {
		replay = append(replay, data...)
		return nil
	})
	output.flush()

	if len(replay) != ptyReplayBytes {
		t.Fatalf("replay length = %d, want %d", len(replay), ptyReplayBytes)
	}
	if !bytes.Equal(replay[len(replay)-4:], []byte("tail")) {
		t.Fatalf("replay tail = %q, want %q", replay[len(replay)-4:], "tail")
	}
}

func TestPTYOutputReplayAndLiveOutputPrecedeExit(t *testing.T) {
	var output ptyOutput
	if err := output.write([]byte("replay")); err != nil {
		t.Fatal(err)
	}

	var received bytes.Buffer
	output.attach(func(data []byte) error {
		_, _ = received.Write(data)
		return nil
	})
	if err := output.write([]byte("live")); err != nil {
		t.Fatal(err)
	}
	output.flush()
	_, _ = received.WriteString("exit")

	if got, want := received.String(), "replayliveexit"; got != want {
		t.Fatalf("received %q, want %q", got, want)
	}
}

func TestPTYOutputContinuesDrainingDuringSinkBackpressure(t *testing.T) {
	var output ptyOutput
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var received bytes.Buffer
	output.attach(func(data []byte) error {
		once.Do(func() {
			close(entered)
			<-release
		})
		_, _ = received.Write(data)
		return nil
	})
	if err := output.write([]byte("blocked")); err != nil {
		t.Fatal(err)
	}
	<-entered

	writesDone := make(chan struct{})
	go func() {
		for i := 0; i < 4; i++ {
			_ = output.write(bytes.Repeat([]byte("x"), ptyReplayBytes/2))
		}
		_ = output.write([]byte("tail"))
		close(writesDone)
	}()
	select {
	case <-writesDone:
	case <-time.After(time.Second):
		t.Fatal("PTY writes blocked behind the WebSocket sink")
	}

	close(release)
	output.flush()
	if !bytes.Contains(received.Bytes(), []byte("tail")) {
		t.Fatal("buffered PTY tail was not forwarded after sink recovered")
	}
}

func TestPTYOutputFullBufferDoesNotReallocateOnEachWrite(t *testing.T) {
	var output ptyOutput
	if err := output.write(bytes.Repeat([]byte("a"), ptyReplayBytes)); err != nil {
		t.Fatal(err)
	}
	if len(output.buf) != ptyReplayBytes {
		t.Fatalf("buffer length = %d, want %d", len(output.buf), ptyReplayBytes)
	}
	firstByte := &output.buf[0]

	for i := 0; i < 10; i++ {
		if err := output.write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if &output.buf[0] != firstByte {
			t.Fatalf("buffer reallocated on write %d", i)
		}
	}
	if got := output.buf[(output.end-1)%ptyReplayBytes]; got != 'x' {
		t.Fatalf("last buffered byte = %q, want %q", got, 'x')
	}
}

func TestPTYOutputDeliversWritesAfterAttachBeforeFirstOutput(t *testing.T) {
	var output ptyOutput
	var mu sync.Mutex
	var received bytes.Buffer
	output.attach(func(data []byte) error {
		mu.Lock()
		defer mu.Unlock()
		_, _ = received.Write(data)
		return nil
	})
	// Let the forwarder reach its wait before the first write.
	time.Sleep(10 * time.Millisecond)
	if err := output.write([]byte("first")); err != nil {
		t.Fatal(err)
	}

	flushed := make(chan struct{})
	go func() {
		output.flush()
		close(flushed)
	}()
	select {
	case <-flushed:
	case <-time.After(time.Second):
		t.Fatal("output written after an early attach was never delivered")
	}
	mu.Lock()
	defer mu.Unlock()
	if got := received.String(); got != "first" {
		t.Fatalf("received %q, want %q", got, "first")
	}
}

func TestPTYOutputDetachWaitsForInFlightSink(t *testing.T) {
	var output ptyOutput
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	output.attach(func(data []byte) error {
		once.Do(func() {
			close(entered)
			<-release
		})
		return nil
	})
	if err := output.write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	<-entered

	detached := make(chan struct{})
	go func() {
		output.detach()
		close(detached)
	}()
	select {
	case <-detached:
		t.Fatal("detach returned while the previous attachment's sink was still running")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-detached:
	case <-time.After(time.Second):
		t.Fatal("detach did not return after the sink finished")
	}

	var received bytes.Buffer
	output.attach(func(data []byte) error {
		_, _ = received.Write(data)
		return nil
	})
	if err := output.write([]byte("b")); err != nil {
		t.Fatal(err)
	}
	output.flush()
	if got := received.String(); got != "ab" {
		t.Fatalf("reattached sink received %q, want %q", got, "ab")
	}
}
