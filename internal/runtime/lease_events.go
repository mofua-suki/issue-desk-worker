package runtime

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

const leaseEventCapacity = 64

type leaseEvent struct {
	job, epoch    int64
	event, reason string
	at            time.Time
}

// Logging is observational: a blocked Writer must never own the renewal slot
// or the lease mutex. One consumer and a bounded queue are shared by a Runner.
// On overflow retain the newest events and make
// the missing history visible when the sink becomes writable again.
type leaseEvents struct {
	mu            sync.Mutex
	queue         chan leaseEvent
	closed        bool
	dropped       atomic.Bool
	closedDropped atomic.Bool
	done          chan struct{}
	now           func() time.Time
}

func newLeaseEvents(sink func(int64, int64, string, string), now func() time.Time) *leaseEvents {
	e := &leaseEvents{queue: make(chan leaseEvent, leaseEventCapacity), done: make(chan struct{}), now: now}
	go func() {
		defer close(e.done)
		for item := range e.queue {
			if e.dropped.Swap(false) {
				// The shared queue may have lost events from multiple jobs.
				writeLeaseEvent(sink, leaseEvent{event: "events_dropped", reason: "buffer_full", at: e.now().UTC()})
			}
			writeLeaseEvent(sink, item)
		}
	}()
	return e
}

func (e *leaseEvents) record(job, epoch int64, event, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		// Retain evidence even if the consumer has already exited. Run keeps
		// the queue open until every producer finishes, including late cleanup.
		e.closedDropped.Store(true)
		return
	}
	item := leaseEvent{job: job, epoch: epoch, event: event, reason: reason, at: e.now().UTC()}
	select {
	case e.queue <- item:
	default:
		select {
		case <-e.queue:
			e.dropped.Store(true)
		default:
		}
		// All producers hold mu, and the consumer can only free space.
		e.queue <- item
	}
}

func writeLeaseEvent(sink func(int64, int64, string, string), item leaseEvent) {
	if sink != nil {
		sink(item.job, item.epoch, item.event, item.reason)
		return
	}
	log.Printf("lease job=%d epoch=%d event=%s reason=%s at=%s", item.job, item.epoch, item.event, item.reason, item.at.Format(time.RFC3339Nano))
}

func (e *leaseEvents) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.closed {
		e.closed = true
		close(e.queue)
	}
	// Do not join a possibly blocked sink. Its single consumer drains and exits
	// when the Writer returns; cancellation/expiry never waits for logging.
}
