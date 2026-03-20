package batcher

import (
	"sync"
	"time"
)

// BytesBatcher consructs a byte slice on every Push call and calls BatchFunc
// on every MaxBatchSize Push calls or MaxDelay interval.
//
// See also Batcher.
type BytesBatcher struct {
	// BatchFunc is called when either MaxBatchSize or MaxDelay is reached.
	//
	//   * b contains a byte slice constructed when Push is called.
	//   * items contains the number of Push calls used for constructing b.
	//
	// BytesBatcher prevents calling BatchFunc from concurrently running
	// goroutines.
	//
	// b mustn't be accessed after returning from BatchFunc.
	BatchFunc func(b []byte, items int)

	// HeaderFunc is called before starting new batch.
	//
	// HeaderFunc must append header data to dst and return the resulting
	// byte slice.
	//
	// dst mustn't be accessed after returning from HeaderFunc.
	//
	// HeaderFunc may be nil.
	HeaderFunc func(dst []byte) []byte

	// FooterFunc is called before the batch is passed to BatchFunc.
	//
	// FooterFunc must append footer data to dst and return the resulting
	// byte slice.
	//
	// dst mustn't be accessed after returning from FooterFunc.
	//
	// FooterFunc may be nil.
	FooterFunc func(dst []byte) []byte

	// MaxBatchSize the the maximum batch size.
	MaxBatchSize int

	// MaxDelay is the maximum duration before BatchFunc is called
	// unless MaxBatchSize is reached.
	MaxDelay time.Duration

	stopped      bool
	once         sync.Once
	lock         sync.Mutex
	b            []byte
	pendingB     []byte
	items        int
	lastExecTime time.Time
}

func (b *BytesBatcher) Stop() {
	b.lock.Lock()
	b.stopped = true
	b.execNolock(false)
	b.lock.Unlock()
}

// Push calls appendFunc on a byte slice.
//
// appendFunc must append data to dst and return the resulting byte slice.
// dst mustn't be accessed after returning from appendFunc.
//
// The function returns false if the batch reached MaxBatchSize and BatchFunc
// isn't returned yet.
func (b *BytesBatcher) Push(appendFunc func(dst []byte, rows int) []byte) bool {
	b.once.Do(b.init)
	b.lock.Lock()
	if b.stopped {
		b.lock.Unlock()
		return false
	}
	if b.items >= b.MaxBatchSize && !b.execNolock(true) {
		b.lock.Unlock()
		return false
	}
	if b.items == 0 {
		if b.HeaderFunc != nil {
			b.b = b.HeaderFunc(b.b)
		}
	}
	b.b = appendFunc(b.b, b.items)
	b.items++
	if b.items >= b.MaxBatchSize {
		b.execNolockNocheck()
	}
	b.lock.Unlock()
	return true
}

// Flush triggers immediate processing of all accumulated items in the current batch.
func (b *BytesBatcher) Flush() (ok bool) {
	b.lock.Lock()
	defer b.lock.Unlock()

	if b.items <= 0 {
		return true
	}

	for i := 0; i < 10; i++ {
		if b.execNolockBlock() {
			return true
		}
		time.Sleep(time.Second)
	}

	return false
}

func (b *BytesBatcher) init() {
	go func() {
		maxDelay := b.MaxDelay
		delay := maxDelay
		for {
			time.Sleep(delay)
			b.lock.Lock()
			d := time.Since(b.lastExecTime)
			if float64(d) > 0.9*float64(maxDelay) {
				if b.items > 0 {
					b.execNolockNocheck()
				}
				delay = maxDelay
			} else {
				delay = maxDelay - d
			}
			b.lock.Unlock()
		}
	}()
}

func (b *BytesBatcher) execNolockNocheck() {
	// Do not check the returned value, since the previous batch
	// may be still pending in BatchFunc.
	// The error will be discovered on the next Push.
	b.execNolock(true)
}

func (b *BytesBatcher) execNolockBlock() bool {
	if len(b.pendingB) > 0 {
		return false
	}
	if b.FooterFunc != nil {
		b.b = b.FooterFunc(b.b)
	}
	b.pendingB = append(b.pendingB[:0], b.b...)
	b.b = b.b[:0]

	b.lastExecTime = time.Now()

	b.BatchFunc(b.pendingB, b.items)

	b.pendingB = b.pendingB[:0]
	b.items = 0

	return true
}

func (b *BytesBatcher) execNolock(parallel bool) bool {
	if len(b.pendingB) > 0 {
		return false
	}
	if b.FooterFunc != nil {
		b.b = b.FooterFunc(b.b)
	}
	b.pendingB = append(b.pendingB[:0], b.b...)
	b.b = b.b[:0]
	items := b.items
	b.items = 0
	b.lastExecTime = time.Now()

	if parallel {
		go func(data []byte, items int) {
			b.BatchFunc(data, items)
			b.lock.Lock()
			b.pendingB = b.pendingB[:0]
			if cap(b.pendingB) > 64*1024 {
				// A hack: throw big pendingB slice to GC in order
				// to reduce memory usage between BatchFunc calls.
				//
				// Keep small pendingB slices in order to reduce
				// load on GC.
				b.pendingB = nil
			}
			b.lock.Unlock()
		}(b.pendingB, items)
	} else {
		b.BatchFunc(b.pendingB, items)
		b.pendingB = nil
	}

	return true
}
