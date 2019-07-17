// Copyright 2014 Canonical Ltd.
// Licensed under the LGPLv3 with static-linking exception.
// See LICENCE file for details.

// Package ratelimit provides an efficient token bucket implementation
// that can be used to limit the rate of arbitrary things.
// See http://en.wikipedia.org/wiki/Token_bucket.
package ratelimit

import (
	"math"
	"sync"
	"time"
	_ "unsafe" // for go:linkname
)

// The algorithm that this implementation uses does computational work
// only when tokens are removed from the bucket, and that work completes
// in short, bounded-constant time (Bucket.Wait benchmarks at 175ns on
// my laptop).
//
// Time is measured in equal measured ticks, a given interval
// (fillInterval) apart. On each tick a number of tokens (quantum) are
// added to the bucket.
//
// When any of the methods are called the bucket updates the number of
// tokens that are in the bucket, and it records the current tick
// number too. Note that it doesn't record the current time - by
// keeping things in units of whole ticks, it's easy to dish out tokens
// at exactly the right intervals as measured from the start time.
//
// This allows us to calculate the number of tokens that will be
// available at some time in the future with a few simple arithmetic
// operations.
//
// The main reason for being able to transfer multiple tokens on each tick
// is so that we can represent rates greater than 1e9 (the resolution of the Go
// time package) tokens per second, but it's also useful because
// it means we can easily represent situations like "a person gets
// five tokens an hour, replenished on the hour".

// Bucket represents a token bucket that fills at a predetermined rate.
// Methods on Bucket may be called concurrently.
type Bucket struct {
	// startTime holds the moment when the bucket was
	// first created and ticks began.
	startTime int64

	// capacity holds the overall capacity of the bucket.
	// This represents the max rate spike/burst.
	capacity int64

	// quantum holds how many tokens are added on
	// each tick.
	quantum int64

	// fillInterval holds the interval between each tick.
	fillInterval time.Duration

	// mu guards the fields below it.
	mu sync.Mutex

	// availableTokens holds the number of available
	// tokens as of the associated latestTick.
	// It will be negative when there are consumers
	// waiting for tokens.
	availableTokens int64

	// latestTick holds the latest tick for which
	// we know the number of tokens in the bucket.
	latestTick int64
}

// NewBucket returns a new token bucket that fills at the
// rate of one token every fillInterval, up to the given
// maximum capacity. Both arguments must be
// positive. The bucket is initially full.
func NewBucket(fillInterval time.Duration, capacity int64) *Bucket {
	return NewBucketWithQuantum(fillInterval, capacity, 1)
}

// rateMargin specifes the allowed variance of actual
// rate from specified rate. 1% seems reasonable.
const rateMargin = 0.01

// NewBucketWithRate returns a token bucket that fills the bucket
// at the rate of rate tokens per second up to the given
// maximum capacity. Because of limited clock resolution,
// at high rates, the actual rate may be up to 1% different from the
// specified rate.
func NewBucketWithRate(rate float64, capacity int64) *Bucket {
	// Use the same bucket each time through the loop
	// to save allocations.
	tb := NewBucketWithQuantum(1, capacity, 1)
	tb.fillInterval, tb.quantum = calcQuantum(rate)
	if tb.capacity < tb.quantum {
		tb.capacity = tb.quantum
	}
	return tb
}

func calcQuantum(rate float64) (fillInterval time.Duration, quantum int64) {
	margin := 1.1
	for {
		for quantum = int64(1); quantum < 1<<50; quantum = nextQuantum(quantum, margin) {
			fillInterval = time.Duration(1e9 * float64(quantum) / rate)
			if fillInterval <= 0 {
				continue
			}
			r0 := 1e9 * float64(quantum) / float64(fillInterval)
			if diff := math.Abs(r0 - rate); diff/rate <= rateMargin {
				return
			}
		}
		margin = 1.0 + margin*0.9
	}
}

// nextQuantum returns the next quantum to try after q.
// We grow the quantum exponentially, but slowly, so we
// get a good fit in the lower numbers.
func nextQuantum(q int64, m float64) int64 {
	q1 := int64(float64(q) * m)
	if q1 <= q {
		q1 = q + 1
	}
	return q1
}

// NewBucketWithQuantum is similar to NewBucket, but allows
// the specification of the quantum size - quantum tokens
// are added every fillInterval.
func NewBucketWithQuantum(fillInterval time.Duration, capacity, quantum int64) *Bucket {
	if fillInterval <= 0 {
		panic("token bucket fill interval is not > 0")
	}
	if capacity <= 0 {
		panic("token bucket capacity is not > 0")
	}
	if quantum <= 0 {
		panic("token bucket quantum is not > 0")
	}
	if capacity < quantum {
		panic("token capacity need large than quantum")
	}
	return &Bucket{
		startTime:       nanotime(),
		latestTick:      0,
		fillInterval:    fillInterval,
		capacity:        capacity,
		quantum:         quantum,
		availableTokens: capacity,
	}
}

// ResetRate reset current rate limit and capacity.
func (tb *Bucket) ResetRate(rate float64, capacity int64) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.latestTick = 0
	tb.capacity = capacity
	tb.fillInterval, tb.quantum = calcQuantum(rate)
	if tb.capacity < tb.quantum {
		tb.capacity = tb.quantum
	}
}

// Wait takes count tokens from the bucket, waiting until they are
// available.
func (tb *Bucket) Wait(count int64) {
	if d := tb.Take(count); d > 0 {
		time.Sleep(d)
	}
}

// WaitMaxDuration is like Wait except that it will
// only take tokens from the bucket if it needs to wait
// for no greater than maxWait. It reports whether
// any tokens have been removed from the bucket
// If no tokens have been removed, it returns immediately.
func (tb *Bucket) WaitMaxDuration(count int64, maxWait time.Duration) bool {
	d, ok := tb.TakeMaxDuration(count, maxWait)
	if d > 0 {
		time.Sleep(d)
	}
	return ok
}

const infinityDuration time.Duration = 0x7fffffffffffffff

// Take takes count tokens from the bucket without blocking. It returns
// the time that the caller should wait until the tokens are actually
// available.
//
// Note that if the request is irrevocable - there is no way to return
// tokens to the bucket once this method commits us to taking them.
func (tb *Bucket) Take(count int64) time.Duration {
	tb.mu.Lock()
	d, _ := tb.take(nanotime(), count, infinityDuration)
	tb.mu.Unlock()
	return d
}

// TakeMaxDuration is like Take, except that
// it will only take tokens from the bucket if the wait
// time for the tokens is no greater than maxWait.
//
// If it would take longer than maxWait for the tokens
// to become available, it does nothing and reports false,
// otherwise it returns the time that the caller should
// wait until the tokens are actually available, and reports
// true.
func (tb *Bucket) TakeMaxDuration(count int64, maxWait time.Duration) (time.Duration, bool) {
	tb.mu.Lock()
	d, ok := tb.take(nanotime(), count, maxWait)
	tb.mu.Unlock()
	return d, ok
}

// TakeAvailable takes up to count immediately available tokens from the
// bucket. It returns the number of tokens removed, or zero if there are
// no available tokens. It does not block.
func (tb *Bucket) TakeAvailable(count int64) int64 {
	tb.mu.Lock()
	n := tb.takeAvailable(nanotime(), count)
	tb.mu.Unlock()
	return n
}

// takeAvailable is the internal version of TakeAvailable - it takes the
// current time as an argument to enable easy testing.
func (tb *Bucket) takeAvailable(now, count int64) int64 {
	if count <= 0 {
		return 0
	}
	tb.adjustavailableTokens(tb.currentTick(now))
	if tb.availableTokens <= 0 {
		return 0
	}
	if count > tb.availableTokens {
		count = tb.availableTokens
	}
	tb.availableTokens -= count
	return count
}

// Available returns the number of available tokens. It will be negative
// when there are consumers waiting for tokens. Note that if this
// returns greater than zero, it does not guarantee that calls that take
// tokens from the buffer will succeed, as the number of available
// tokens could have changed in the meantime. This method is intended
// primarily for metrics reporting and debugging.
func (tb *Bucket) Available() int64 {
	return tb.available(nanotime())
}

// available is the internal version of available - it takes the current time as
// an argument to enable easy testing.
func (tb *Bucket) available(now int64) int64 {
	tb.mu.Lock()
	tb.adjustavailableTokens(tb.currentTick(now))
	avail := tb.availableTokens
	tb.mu.Unlock()
	return avail
}

// Capacity returns the capacity that the bucket was created with.
func (tb *Bucket) Capacity() int64 {
	return tb.capacity
}

// Rate returns the fill rate of the bucket, in tokens per second.
func (tb *Bucket) Rate() float64 {
	return 1e9 * float64(tb.quantum) / float64(tb.fillInterval)
}

// take is the internal version of Take - it takes the current time as
// an argument to enable easy testing.
func (tb *Bucket) take(now, count int64, maxWait time.Duration) (time.Duration, bool) {
	if count <= 0 {
		return 0, true
	}

	tick := tb.currentTick(now)
	tb.adjustavailableTokens(tick)
	avail := tb.availableTokens - count
	if avail >= 0 {
		tb.availableTokens = avail
		return 0, true
	}
	// Round up the missing tokens to the nearest multiple
	// of quantum - the tokens won't be available until
	// that tick.

	// endTick holds the tick when all the requested tokens will
	// become available.
	endTick := tick + (-avail+tb.quantum-1)/tb.quantum
	endTime := tb.startTime + (endTick * int64(tb.fillInterval))
	waitTime := time.Duration(endTime - now)
	if waitTime > maxWait {
		return 0, false
	}
	tb.availableTokens = avail
	return waitTime, true
}

// currentTick returns the current time tick, measured
// from tb.startTime.
func (tb *Bucket) currentTick(now int64) int64 {
	return (now - tb.startTime) / int64(tb.fillInterval)
}

// adjustavailableTokens adjusts the current number of tokens
// available in the bucket at the given time, which must
// be in the future (positive) with respect to tb.latestTick.
func (tb *Bucket) adjustavailableTokens(tick int64) {
	if tb.availableTokens >= tb.capacity {
		return
	}
	tb.availableTokens += (tick - tb.latestTick) * tb.quantum
	if tb.availableTokens > tb.capacity {
		tb.availableTokens = tb.capacity
	}
	tb.latestTick = tick
	return
}

//go:linkname nanotime runtime.nanotime
func nanotime() int64
