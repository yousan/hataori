package discord

import (
	"sync"

	"github.com/yousan/hataori/internal/surface"
)

// queue is a surface.Sink that hands what it gets to another sink, in
// order, on a goroutine of its own. The gateway's receive loop then never
// waits for the sink: a slow sink cannot delay the next event, such as the
// acknowledgement a button press needs within 3 seconds.
type queue struct {
	mu      sync.Mutex
	pending []func(surface.Sink)
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
}

func newQueue(sink surface.Sink) *queue {
	q := &queue{wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	go q.run(sink)
	return q
}

func (q *queue) push(f func(surface.Sink)) {
	q.mu.Lock()
	q.pending = append(q.pending, f)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *queue) run(sink surface.Sink) {
	defer close(q.done)
	for {
		q.mu.Lock()
		batch := q.pending
		q.pending = nil
		q.mu.Unlock()
		for _, f := range batch {
			select {
			case <-q.stop:
				return
			default:
			}
			f(sink)
		}
		select {
		case <-q.wake:
		case <-q.stop:
			return
		}
	}
}

// close stops the queue; what is still pending is not delivered.
func (q *queue) close() {
	close(q.stop)
	<-q.done
}

// connected tells the sink, in order with the messages, that a session is
// ready, when the sink wants to know (surface.Reconnected).
func (q *queue) connected() {
	q.push(func(s surface.Sink) {
		if r, ok := s.(surface.Reconnected); ok {
			r.Connected()
		}
	})
}

// disconnected tells the sink, in order with the messages, that the
// connection was lost and is being made again, when the sink wants to know
// (surface.Disconnected).
func (q *queue) disconnected() {
	q.push(func(s surface.Sink) {
		if d, ok := s.(surface.Disconnected); ok {
			d.Disconnected()
		}
	})
}

func (q *queue) Message(in surface.Inbound) { q.push(func(s surface.Sink) { s.Message(in) }) }
func (q *queue) Choice(c surface.Selection) { q.push(func(s surface.Sink) { s.Choice(c) }) }
func (q *queue) Command(c surface.Command)  { q.push(func(s surface.Sink) { s.Command(c) }) }
