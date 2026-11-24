package cyclebatch

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
)

type cycleBatch struct {
	logger     *zap.Logger
	deliveries int
	maxWait    time.Duration
	next       consumer.Metrics

	// mu guards the pending group. sendMu serialises sends so groups leave in
	// the order they were completed; it is taken before mu is released, so a
	// group handed over first is also sent first, while new deliveries can
	// already start the next group.
	mu       sync.Mutex
	sendMu   sync.Mutex
	pending  pmetric.Metrics
	count    int
	gen      uint64 // identifies the current group; a stale timer sees another value
	timer    *time.Timer
	shutdown bool
	// inflight counts groups handed over and not yet returned from next;
	// Shutdown waits for it. Add happens under mu, before Shutdown can see
	// the group gone.
	inflight sync.WaitGroup
}

func newCycleBatch(logger *zap.Logger, deliveries int, maxWait time.Duration, next consumer.Metrics) *cycleBatch {
	return &cycleBatch{logger: logger, deliveries: deliveries, maxWait: maxWait, next: next}
}

func (p *cycleBatch) Start(context.Context, component.Host) error { return nil }

func (p *cycleBatch) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: true}
}

// ConsumeMetrics moves the delivery into the pending group and returns at once.
// The group is sent when it holds `deliveries` deliveries or when the timer
// armed by its first delivery fires.
func (p *cycleBatch) ConsumeMetrics(_ context.Context, md pmetric.Metrics) error {
	p.mu.Lock()
	if p.shutdown {
		// Nothing should deliver after Shutdown; pass it through rather than hold it.
		p.mu.Unlock()
		p.send(md)
		return nil
	}
	if p.count == 0 {
		p.pending = pmetric.NewMetrics()
		p.gen++
		gen := p.gen
		p.timer = time.AfterFunc(p.maxWait, func() { p.flushOnTimer(gen) })
	}
	md.ResourceMetrics().MoveAndAppendTo(p.pending.ResourceMetrics())
	p.count++
	if p.count < p.deliveries {
		p.mu.Unlock()
		return nil
	}
	p.handOverLocked()
	return nil
}

// flushOnTimer sends group gen if it is still pending. A timer whose group
// already left on the count finds another generation (or none) and does nothing.
func (p *cycleBatch) flushOnTimer(gen uint64) {
	p.mu.Lock()
	if p.count == 0 || p.gen != gen {
		p.mu.Unlock()
		return
	}
	p.handOverLocked()
}

// Shutdown hands a pending group downstream before it returns, so nothing
// that was collected is lost at a graceful stop.
func (p *cycleBatch) Shutdown(context.Context) error {
	p.mu.Lock()
	p.shutdown = true
	if p.count == 0 {
		p.mu.Unlock()
		// A timer may have handed its group over just before: wait until
		// that send has returned, or the next processor could be shut down
		// before the group reaches it.
		p.inflight.Wait()
		return nil
	}
	p.handOverLocked()
	return nil
}

// handOverLocked takes the pending group and sends it. Called with mu held;
// it releases mu once it holds sendMu.
func (p *cycleBatch) handOverLocked() {
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	out := p.pending
	p.pending = pmetric.Metrics{}
	p.count = 0
	p.inflight.Add(1)
	defer p.inflight.Done()
	p.sendMu.Lock()
	p.mu.Unlock()
	defer p.sendMu.Unlock()
	p.sendLocked(out)
}

func (p *cycleBatch) send(md pmetric.Metrics) {
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	p.sendLocked(md)
}

func (p *cycleBatch) sendLocked(md pmetric.Metrics) {
	// Counted before the call: downstream owns md once it has it.
	n := md.DataPointCount()
	// The delivering receiver's context may be cancelled by now; the group
	// belongs to the pipeline, not to that call.
	if err := p.next.ConsumeMetrics(context.Background(), md); err != nil {
		p.logger.Warn("cycle_batch: sending a collection cycle downstream failed",
			zap.Int("datapoints", n), zap.Error(err))
	}
}
