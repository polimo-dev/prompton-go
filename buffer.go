package prompton

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// The monitoring-log buffer. Never block the provider call on a log flush:
// enqueueing is a map copy and a mutex, sending happens on the buffer's own
// goroutine.
//
// Batching rules the endpoint imposes: at most 200 records and 5 MB per
// request, and the environment query parameter is forced onto the whole batch,
// so records are queued per environment and never share a request across them.

const (
	maxBatchRecords = 200
	// The endpoint accepts 5 MB; leaving headroom keeps the envelope and the
	// query string from pushing a full batch over the edge.
	maxBatchBytes = 4_500_000
	logBackoffMin = time.Second
	logBackoffMax = 5 * time.Minute
)

// BufferStats reports what the monitoring-log buffer has been doing. Every
// counter is a record count, and every drop is a record PromptOn will never
// see.
type BufferStats struct {
	// Queued is how many records are waiting to be sent.
	Queued int
	// Sent is how many records the server accepted or counted as duplicates.
	Sent int
	// DroppedOverflow is how many were dropped because the queue was full.
	DroppedOverflow int
	// DroppedTooLarge is how many single records exceeded the request limit.
	DroppedTooLarge int
	// DroppedRejected is how many the server rejected per-record.
	DroppedRejected int
	// DroppedClientError is how many were dropped after a 4xx that is not
	// retried.
	DroppedClientError int
	// DroppedRetriesExhausted is how many were dropped after the attempt bound.
	DroppedRetriesExhausted int
	// DroppedAfterClose is how many were rejected because the client was
	// already closed. Log reports that one as an error too.
	DroppedAfterClose int
	// Failures is the highest number of consecutive send failures any one
	// environment is carrying; zero when every environment's last send
	// succeeded.
	Failures int
}

type queuedRecord struct {
	record map[string]interface{}
	bytes  int
	// seq orders records across environments, so the queue bound drops the
	// genuinely oldest record rather than whichever environment sorts first.
	seq uint64
}

type logBatch struct {
	records  []queuedRecord
	attempts int
}

func (b logBatch) byteSize() int {
	total := 0
	for _, r := range b.records {
		total += r.bytes
	}
	return total
}

// lane is one environment's queue. Batches are per environment because the
// endpoint forces the environment onto everything in a request.
type lane struct {
	// ready holds batches that must be sent as they are: a batch being retried
	// with the same ids, or the halves of one split after a 413.
	ready []logBatch
	// readyBytes and pendingBytes are tracked apart so a batch that goes back
	// onto ready after a failed send restores its bytes exactly, and the flush
	// trigger keeps measuring what is still accumulating.
	readyBytes   int
	pending      []queuedRecord
	pendingBytes int
	// blockedUntil is when this lane may contact the server again. Until then
	// the previous batch stays queued and later records queue behind it.
	blockedUntil time.Time
	failures     int
}

type logBuffer struct {
	client *Client

	mu     sync.Mutex
	lanes  map[string]*lane
	stats  BufferStats
	seq    uint64
	closed bool

	wake     chan struct{}
	flushReq chan chan error
	done     chan struct{}
	stopped  chan struct{}
}

func newLogBuffer(c *Client) *logBuffer {
	b := &logBuffer{
		client:   c,
		lanes:    map[string]*lane{},
		wake:     make(chan struct{}, 1),
		flushReq: make(chan chan error),
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	go b.run()
	return b
}

func (b *logBuffer) laneFor(env string) *lane {
	l, ok := b.lanes[env]
	if !ok {
		l = &lane{}
		b.lanes[env] = l
	}
	return l
}

// enqueue adds one record and returns immediately. A record the buffer will
// never send is counted, and only the closed case is reported back: it is the
// one where the caller can still do something about it.
func (b *logBuffer) enqueue(env string, record map[string]interface{}) error {
	size := jsonSize(record)
	b.mu.Lock()
	if b.closed {
		b.stats.DroppedAfterClose++
		b.mu.Unlock()
		b.client.warnOnce("log-closed", "dropping a monitoring log: the client is closed")
		return ErrClosed
	}
	if size > maxBatchBytes {
		b.stats.DroppedTooLarge++
		b.mu.Unlock()
		b.client.warnOnce("log-too-large", "dropping a %d-byte monitoring log: one record cannot exceed %d bytes", size, maxBatchBytes)
		return nil
	}
	b.seq++
	l := b.laneFor(env)
	l.pending = append(l.pending, queuedRecord{record: record, bytes: size, seq: b.seq})
	l.pendingBytes += size
	overflow := b.trimToBound()
	trigger := len(l.pending) >= b.client.cfg.LogFlushSize || l.pendingBytes >= b.client.cfg.LogFlushBytes
	b.mu.Unlock()

	if overflow > 0 {
		b.client.warnOnce("log-overflow", "monitoring-log queue full: dropped %d oldest record(s)", overflow)
	}
	if trigger {
		b.nudge()
	}
	return nil
}

// trimToBound enforces the queue bound by dropping the oldest record in the
// buffer, whichever environment holds it. Callers hold the lock.
func (b *logBuffer) trimToBound() int {
	limit := b.client.cfg.LogMaxBuffer
	dropped := 0
	for b.totalQueuedLocked() > limit {
		oldest := b.oldestLaneLocked()
		if oldest == nil {
			break
		}
		if len(oldest.ready) > 0 {
			batch := oldest.ready[0]
			if len(batch.records) > 0 {
				oldest.readyBytes -= batch.records[0].bytes
				batch.records = batch.records[1:]
				dropped++
				if len(batch.records) == 0 {
					oldest.ready = oldest.ready[1:]
				} else {
					oldest.ready[0] = batch
				}
				continue
			}
			oldest.ready = oldest.ready[1:]
			continue
		}
		if len(oldest.pending) == 0 {
			break
		}
		oldest.pendingBytes -= oldest.pending[0].bytes
		oldest.pending = oldest.pending[1:]
		dropped++
	}
	b.stats.DroppedOverflow += dropped
	return dropped
}

func (b *logBuffer) totalQueuedLocked() int {
	total := 0
	for _, l := range b.lanes {
		total += l.count()
	}
	return total
}

// oldestLaneLocked is the lane whose head record was enqueued first. Records
// carry an enqueue sequence precisely so that drop-oldest stays drop-oldest
// once a process logs to two environments.
func (b *logBuffer) oldestLaneLocked() *lane {
	var pick *lane
	var pickSeq uint64
	for _, name := range b.laneNamesLocked() {
		l := b.lanes[name]
		seq, ok := l.headSeq()
		if !ok {
			continue
		}
		if pick == nil || seq < pickSeq {
			pick = l
			pickSeq = seq
		}
	}
	return pick
}

// headSeq is the enqueue sequence of the lane's oldest queued record. Ready
// batches always hold records taken from pending earlier, so they come first.
func (l *lane) headSeq() (uint64, bool) {
	for _, batch := range l.ready {
		if len(batch.records) > 0 {
			return batch.records[0].seq, true
		}
	}
	if len(l.pending) > 0 {
		return l.pending[0].seq, true
	}
	return 0, false
}

func (b *logBuffer) laneNamesLocked() []string {
	names := make([]string, 0, len(b.lanes))
	for name := range b.lanes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (l *lane) count() int {
	n := len(l.pending)
	for _, batch := range l.ready {
		n += len(batch.records)
	}
	return n
}

func (b *logBuffer) nudge() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *logBuffer) snapshotStats() BufferStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.stats
	out.Queued = b.totalQueuedLocked()
	// Failures belongs to a lane, not to the buffer: a success on one
	// environment must not report health while another is still backing off.
	out.Failures = 0
	for _, l := range b.lanes {
		if l.failures > out.Failures {
			out.Failures = l.failures
		}
	}
	return out
}

// flush sends everything queued and waits for the result.
func (b *logBuffer) flush(ctx context.Context) error {
	reply := make(chan error, 1)
	select {
	case b.flushReq <- reply:
	case <-b.stopped:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *logBuffer) close(timeout time.Duration) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		<-b.stopped
		return nil
	}
	b.closed = true
	b.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// Best effort: a drain that cannot finish in time leaves records unsent
	// rather than holding the process open.
	err := b.flush(ctx)
	close(b.done)
	<-b.stopped
	return err
}

func (b *logBuffer) run() {
	defer close(b.stopped)
	timer := time.NewTimer(b.client.cfg.LogFlushInterval)
	defer timer.Stop()
	for {
		select {
		case <-b.done:
			return
		case reply := <-b.flushReq:
			reply <- b.drain(context.Background())
		case <-b.wake:
			b.sendReady(context.Background(), false)
		case <-timer.C:
			b.sendReady(context.Background(), true)
		}
		resetTimer(timer, b.nextWakeup())
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// nextWakeup is when the loop should look again: the flush interval normally,
// or the moment a blocked lane is allowed to retry.
func (b *logBuffer) nextWakeup() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.client.cfg.now()
	next := b.client.cfg.LogFlushInterval
	for _, l := range b.lanes {
		if l.count() == 0 || l.blockedUntil.IsZero() {
			continue
		}
		if d := l.blockedUntil.Sub(now); d > 0 && d < next {
			next = d
		}
	}
	if next < time.Millisecond {
		next = time.Millisecond
	}
	return next
}

// sendReady sends one batch per lane that is due. timerFired means the interval
// elapsed, which flushes whatever is queued however small.
func (b *logBuffer) sendReady(ctx context.Context, timerFired bool) {
	for _, env := range b.laneNames() {
		batch, ok := b.takeBatch(env, timerFired)
		if !ok {
			continue
		}
		b.send(ctx, env, batch)
	}
}

func (b *logBuffer) laneNames() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.laneNamesLocked()
}

// takeBatch removes the next batch for an environment, honouring the lane's
// block window.
func (b *logBuffer) takeBatch(env string, force bool) (logBatch, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.lanes[env]
	if !ok || l.count() == 0 {
		return logBatch{}, false
	}
	if now := b.client.cfg.now(); l.blockedUntil.After(now) {
		return logBatch{}, false
	}
	if len(l.ready) > 0 {
		batch := l.ready[0]
		l.ready = l.ready[1:]
		l.readyBytes -= batch.byteSize()
		return batch, true
	}
	if !force && len(l.pending) < b.client.cfg.LogFlushSize && l.pendingBytes < b.client.cfg.LogFlushBytes {
		return logBatch{}, false
	}
	var batch logBatch
	total := 0
	for len(l.pending) > 0 && len(batch.records) < maxBatchRecords {
		next := l.pending[0]
		if len(batch.records) > 0 && total+next.bytes > maxBatchBytes {
			break
		}
		batch.records = append(batch.records, next)
		total += next.bytes
		l.pendingBytes -= next.bytes
		l.pending = l.pending[1:]
	}
	if len(batch.records) == 0 {
		return logBatch{}, false
	}
	return batch, true
}

func (b *logBuffer) send(ctx context.Context, env string, batch logBatch) {
	records := make([]map[string]interface{}, len(batch.records))
	for i, r := range batch.records {
		records[i] = r.record
	}
	sendCtx, cancel := context.WithTimeout(ctx, b.client.cfg.Timeout+time.Second)
	result, err := b.client.postLogs(sendCtx, env, records)
	cancel()

	if err == nil {
		b.mu.Lock()
		b.stats.Sent += result.Accepted + result.Duplicates
		b.stats.DroppedRejected += len(result.Rejected)
		if l, ok := b.lanes[env]; ok {
			l.failures = 0
			l.blockedUntil = time.Time{}
		}
		b.mu.Unlock()
		if len(result.Rejected) > 0 {
			// Partial acceptance: one bad record never fails the batch, and an
			// accepted record must never be resent.
			b.client.cfg.Logger("%d monitoring log(s) rejected, first: %s (%s)",
				len(result.Rejected), result.Rejected[0].Message, result.Rejected[0].Code)
		}
		return
	}
	b.handleSendError(env, batch, err)
}

func (b *logBuffer) handleSendError(env string, batch logBatch, err error) {
	var apiErr *APIError
	status := 0
	retryAfter := time.Duration(0)
	if errors.As(err, &apiErr) {
		status = apiErr.Status
		if apiErr.RetryAfter > 0 {
			retryAfter = time.Duration(apiErr.RetryAfter * float64(time.Second))
		}
	}

	switch {
	case status == 413 && len(batch.records) > 1:
		// Split in half and resend both halves as they are.
		half := len(batch.records) / 2
		b.mu.Lock()
		l := b.laneFor(env)
		l.ready = append([]logBatch{
			{records: batch.records[:half], attempts: batch.attempts},
			{records: batch.records[half:], attempts: batch.attempts},
		}, l.ready...)
		l.readyBytes += batch.byteSize()
		b.mu.Unlock()
		b.nudge()
		return

	case status == 413:
		b.mu.Lock()
		b.stats.DroppedTooLarge += len(batch.records)
		b.mu.Unlock()
		b.client.cfg.Logger("dropping a monitoring log the server refused with 413")
		return

	case status >= 400 && status < 500 && status != 429:
		// Never retry another 4xx: the record or the key is wrong, and resending
		// it forever only hides that.
		b.mu.Lock()
		b.stats.DroppedClientError += len(batch.records)
		b.mu.Unlock()
		b.client.warnOnce("log-4xx", "dropping %d monitoring log(s): %v", len(batch.records), err)
		return
	}

	batch.attempts++
	if batch.attempts >= b.client.cfg.LogMaxAttempts {
		b.mu.Lock()
		b.stats.DroppedRetriesExhausted += len(batch.records)
		b.mu.Unlock()
		b.client.cfg.Logger("dropping %d monitoring log(s) after %d attempts: %v", len(batch.records), batch.attempts, err)
		return
	}

	b.mu.Lock()
	l := b.laneFor(env)
	l.failures++
	delay := retryAfter
	if delay <= 0 {
		delay = backoffDelay(logBackoffMin, logBackoffMax, batch.attempts)
	}
	l.blockedUntil = b.client.cfg.now().Add(delay)
	l.ready = append([]logBatch{batch}, l.ready...)
	l.readyBytes += batch.byteSize()
	b.trimToBound()
	b.mu.Unlock()
	b.client.warnOnce("log-retry", "monitoring log send failed (%v); retrying the same batch in %s", err, delay.Round(time.Millisecond))
}

// drain sends everything queued and reports whether the queue emptied. A lane
// that is waiting out a Retry-After stops the drain rather than hammering the
// server, and its records stay queued for the next attempt.
func (b *logBuffer) drain(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		sent := false
		for _, env := range b.laneNames() {
			batch, ok := b.takeBatch(env, true)
			if !ok {
				continue
			}
			sent = true
			b.send(ctx, env, batch)
		}
		if !sent {
			break
		}
	}
	if n := b.snapshotStats().Queued; n > 0 {
		return fmt.Errorf("prompton: %d monitoring log(s) are still queued", n)
	}
	return nil
}

// backoffDelay is the exponential ×2 schedule shared by the poller and the log
// buffer, capped so a long outage does not turn into a long silence.
func backoffDelay(base, max time.Duration, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}
