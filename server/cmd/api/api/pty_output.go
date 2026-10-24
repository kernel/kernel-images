package api

import (
	"sync"
	"time"
)

const (
	ptyReplayBytes = 64 * 1024
	ptyDrainWait   = 2 * time.Second
)

type ptyOutput struct {
	mu        sync.Mutex
	buf       []byte
	start     uint64
	end       uint64
	sink      func([]byte) error
	stop      chan struct{}
	exited    chan struct{}
	changed   chan struct{}
	delivered uint64
}

func (o *ptyOutput) write(data []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.buf) == 0 {
		o.buf = make([]byte, ptyReplayBytes)
	}

	if len(data) > ptyReplayBytes {
		o.start = o.end + uint64(len(data)-ptyReplayBytes)
		data = data[len(data)-ptyReplayBytes:]
	}
	for len(data) > 0 {
		index := int(o.end % ptyReplayBytes)
		n := min(len(data), ptyReplayBytes-index)
		copy(o.buf[index:index+n], data[:n])
		o.end += uint64(n)
		data = data[n:]
		if o.end-o.start > ptyReplayBytes {
			o.start = o.end - ptyReplayBytes
		}
	}
	o.signalLocked()
	return nil
}

func (o *ptyOutput) attach(sink func([]byte) error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.changed == nil {
		o.changed = make(chan struct{})
	}
	o.sink = sink
	o.stop = make(chan struct{})
	o.exited = make(chan struct{})
	o.delivered = o.start
	go o.forward(o.stop, o.exited)
}

func (o *ptyOutput) forward(stop <-chan struct{}, exited chan<- struct{}) {
	defer close(exited)
	for {
		o.mu.Lock()
		if o.delivered < o.start {
			o.delivered = o.start
		}
		if o.sink == nil {
			o.mu.Unlock()
			return
		}
		if o.delivered < o.end {
			count := min(o.end-o.delivered, uint64(ptyReplayBytes))
			data := o.copyLocked(o.delivered, count)
			sink := o.sink
			o.mu.Unlock()
			if err := sink(data); err != nil {
				o.mu.Lock()
				o.sink = nil
				o.signalLocked()
				o.mu.Unlock()
				return
			}
			o.mu.Lock()
			if o.sink == nil {
				o.signalLocked()
				o.mu.Unlock()
				return
			}
			o.delivered += uint64(len(data))
			o.signalLocked()
			o.mu.Unlock()
			continue
		}
		changed := o.changed
		o.mu.Unlock()
		select {
		case <-changed:
		case <-stop:
			return
		}
	}
}

func (o *ptyOutput) copyLocked(cursor, count uint64) []byte {
	data := make([]byte, count)
	index := int(cursor % ptyReplayBytes)
	first := min(len(data), ptyReplayBytes-index)
	copy(data, o.buf[index:index+first])
	copy(data[first:], o.buf[:len(data)-first])
	return data
}

func (o *ptyOutput) flush() {
	o.mu.Lock()
	target := o.end
	for o.sink != nil && o.delivered < target {
		changed := o.changed
		o.mu.Unlock()
		<-changed
		o.mu.Lock()
		target = o.end
	}
	o.mu.Unlock()
}

func (o *ptyOutput) detach() {
	o.mu.Lock()
	var exited chan struct{}
	if o.stop != nil {
		close(o.stop)
		o.stop = nil
		exited = o.exited
	}
	o.sink = nil
	o.signalLocked()
	o.mu.Unlock()
	// Wait for the forwarder to leave any in-flight sink call so it cannot move
	// the cursor of the next attachment.
	if exited != nil {
		<-exited
	}
}

func (o *ptyOutput) signalLocked() {
	if o.changed != nil {
		close(o.changed)
	}
	o.changed = make(chan struct{})
}
