package metrics

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// Meters count events to produce exponentially-weighted moving average rates
// at one-, five-, and fifteen-minutes and a mean rate.
type Meter interface {
	Count() int64
	Mark(int64)
	Rate1() float64
	Rate5() float64
	Rate15() float64
	RateMean() float64
	RateStep() float64
	Snapshot() Meter
	Stop()
}

// GetOrRegisterMeter returns an existing Meter or constructs and registers a
// new StandardMeter.
// Be sure to unregister the meter from the registry once it is of no use to
// allow for garbage collection.
func GetOrRegisterMeter(name string, r Registry) Meter {
	if nil == r {
		r = DefaultRegistry
	}
	return r.GetOrRegister(name, NewMeter).(Meter)
}

// NewMeter constructs a new StandardMeter and launches a goroutine.
// Be sure to call Stop() once the meter is of no use to allow for garbage collection.
func NewMeter() Meter {
	if UseNilMetrics {
		return NilMeter{}
	}
	m := newStandardMeter()
	arbiter.Lock()
	defer arbiter.Unlock()
	arbiter.meters[m] = struct{}{}
	if !arbiter.started {
		arbiter.started = true
		go arbiter.tick()
	}
	return m
}

// NewMeter constructs and registers a new StandardMeter and launches a
// goroutine.
// Be sure to unregister the meter from the registry once it is of no use to
// allow for garbage collection.
func NewRegisteredMeter(name string, r Registry) Meter {
	c := NewMeter()
	if nil == r {
		r = DefaultRegistry
	}
	r.Register(name, c)
	return c
}

// MeterSnapshot is a read-only copy of another Meter.
type MeterSnapshot struct {
	count, _lastCount                        int64
	rate1, rate5, rate15, rateMean, rateStep uint64
	_lastTime                                time.Time
}

// Count returns the count of events at the time the snapshot was taken.
func (m *MeterSnapshot) Count() int64 { return m.count }

// Mark panics.
func (*MeterSnapshot) Mark(n int64) {
	panic("Mark called on a MeterSnapshot")
}

// Rate1 returns the one-minute moving average rate of events per second at the
// time the snapshot was taken.
func (m *MeterSnapshot) Rate1() float64 { return math.Float64frombits(m.rate1) }

// Rate5 returns the five-minute moving average rate of events per second at
// the time the snapshot was taken.
func (m *MeterSnapshot) Rate5() float64 { return math.Float64frombits(m.rate5) }

// Rate15 returns the fifteen-minute moving average rate of events per second
// at the time the snapshot was taken.
func (m *MeterSnapshot) Rate15() float64 { return math.Float64frombits(m.rate15) }

// RateMean returns the meter's mean rate of events per second at the time the
// snapshot was taken.
func (m *MeterSnapshot) RateMean() float64 { return math.Float64frombits(m.rateMean) }

func (m *MeterSnapshot) RateStep() float64 { return math.Float64frombits(m.rateStep) }

// Snapshot returns the snapshot.
func (m *MeterSnapshot) Snapshot() Meter { return m }

// Stop is a no-op.
func (m *MeterSnapshot) Stop() {}

// NilMeter is a no-op Meter.
type NilMeter struct{}

// Count is a no-op.
func (NilMeter) Count() int64 { return 0 }

// Mark is a no-op.
func (NilMeter) Mark(n int64) {}

// Rate1 is a no-op.
func (NilMeter) Rate1() float64 { return 0.0 }

// Rate5 is a no-op.
func (NilMeter) Rate5() float64 { return 0.0 }

// Rate15is a no-op.
func (NilMeter) Rate15() float64 { return 0.0 }

// RateMean is a no-op.
func (NilMeter) RateMean() float64 { return 0.0 }

func (NilMeter) RateStep() float64 { return 0.0 }

// Snapshot is a no-op.
func (NilMeter) Snapshot() Meter { return NilMeter{} }

// Stop is a no-op.
func (NilMeter) Stop() {}

// StandardMeter is the standard implementation of a Meter.
type StandardMeter struct {
	// Only used on stop.
	lock        sync.Mutex
	snapshot    *MeterSnapshot
	a1, a5, a15 EWMA
	startTime   time.Time
	stopped     uint32
}

func newStandardMeter() *StandardMeter {
	nw := time.Now()
	return &StandardMeter{
		snapshot:  &MeterSnapshot{_lastTime: nw},
		a1:        NewEWMA1(),
		a5:        NewEWMA5(),
		a15:       NewEWMA15(),
		startTime: nw,
	}
}

// Stop stops the meter, Mark() will be a no-op if you use it after being stopped.
func (m *StandardMeter) Stop() {
	m.lock.Lock()
	stopped := m.stopped
	m.stopped = 1
	m.lock.Unlock()
	if stopped != 1 {
		arbiter.Lock()
		delete(arbiter.meters, m)
		arbiter.Unlock()
	}
}

// Count returns the number of events recorded.
func (m *StandardMeter) Count() int64 {
	return atomic.LoadInt64(&m.snapshot.count)
}

// Mark records the occurance of n events.
func (m *StandardMeter) Mark(n int64) {
	if atomic.LoadUint32(&m.stopped) == 1 {
		return
	}

	atomic.AddInt64(&m.snapshot.count, n)

	m.a1.Update(n)
	m.a5.Update(n)
	m.a15.Update(n)
	m.updateSnapshot()
}

// Rate1 returns the one-minute moving average rate of events per second.
func (m *StandardMeter) Rate1() float64 {
	return math.Float64frombits(atomic.LoadUint64(&m.snapshot.rate1))
}

// Rate5 returns the five-minute moving average rate of events per second.
func (m *StandardMeter) Rate5() float64 {
	return math.Float64frombits(atomic.LoadUint64(&m.snapshot.rate5))
}

// Rate15 returns the fifteen-minute moving average rate of events per second.
func (m *StandardMeter) Rate15() float64 {
	return math.Float64frombits(atomic.LoadUint64(&m.snapshot.rate15))
}

// RateMean returns the meter's mean rate of events per second.
func (m *StandardMeter) RateMean() float64 {
	return math.Float64frombits(atomic.LoadUint64(&m.snapshot.rateMean))
}

// RateStep returns the meter's step rate of events per second
func (m *StandardMeter) RateStep() float64 {
	m.lock.Lock()
	defer m.lock.Unlock()
	m.updateSnapshotOnStep()
	return math.Float64frombits(atomic.LoadUint64(&m.snapshot.rateStep))
}

// Snapshot updates rate.step and returns a read-only copy of the meter.
func (m *StandardMeter) Snapshot() Meter {
	m.lock.Lock()
	defer m.lock.Unlock()
	m.updateSnapshotOnStep()

	copiedSnapshot := MeterSnapshot{
		count:    atomic.LoadInt64(&m.snapshot.count),
		rate1:    atomic.LoadUint64(&m.snapshot.rate1),
		rate5:    atomic.LoadUint64(&m.snapshot.rate5),
		rate15:   atomic.LoadUint64(&m.snapshot.rate15),
		rateMean: atomic.LoadUint64(&m.snapshot.rateMean),
	}
	return &copiedSnapshot
}

func (m *StandardMeter) updateSnapshot() {
	rate1 := math.Float64bits(m.a1.Rate())
	rate5 := math.Float64bits(m.a5.Rate())
	rate15 := math.Float64bits(m.a15.Rate())
	rateMean := math.Float64bits(float64(m.Count()) / time.Since(m.startTime).Seconds())

	atomic.StoreUint64(&m.snapshot.rate1, rate1)
	atomic.StoreUint64(&m.snapshot.rate5, rate5)
	atomic.StoreUint64(&m.snapshot.rate15, rate15)
	atomic.StoreUint64(&m.snapshot.rateMean, rateMean)
}

func (m *StandardMeter) updateSnapshotOnStep() {
	// should run with write lock held on m.lock
	nw := time.Now()
	sub := nw.Sub(m.startTime).Seconds()
	step := nw.Sub(m.snapshot._lastTime).Seconds()
	rate1 := math.Float64bits(m.a1.Rate())
	rate5 := math.Float64bits(m.a5.Rate())
	rate15 := math.Float64bits(m.a15.Rate())
	if sub > 0 {
		rateMean := math.Float64bits(float64(m.snapshot.count) / sub)
		atomic.StoreUint64(&m.snapshot.rateMean, rateMean)
	}
	if step > 0 {
		rateStep := math.Float64bits(float64(m.snapshot.count-m.snapshot._lastCount) / step)
		atomic.StoreUint64(&m.snapshot.rateStep, rateStep)
	}

	atomic.StoreUint64(&m.snapshot.rate1, rate1)
	atomic.StoreUint64(&m.snapshot.rate5, rate5)
	atomic.StoreUint64(&m.snapshot.rate15, rate15)
	m.snapshot._lastCount = m.snapshot.count
	m.snapshot._lastTime = nw
}

func (m *StandardMeter) tick(now time.Time) {
	m.a1.Tick()
	m.a5.Tick()
	m.a15.Tick()
	m.updateSnapshot()
}

// meterArbiter ticks meters every 5s from a single goroutine.
// meters are references in a set for future stopping.
type meterArbiter struct {
	sync.RWMutex
	started bool
	meters  map[*StandardMeter]struct{}
	ticker  *time.Ticker
}

var arbiter = meterArbiter{ticker: time.NewTicker(5e9), meters: make(map[*StandardMeter]struct{})}

// Ticks meters on the scheduled interval
func (ma *meterArbiter) tick() {
	for {
		select {
		case nw := <-ma.ticker.C:
			ma.tickMeters(nw)
		}
	}
}

func (ma *meterArbiter) tickMeters(nw time.Time) {
	ma.RLock()
	defer ma.RUnlock()
	for meter := range ma.meters {
		meter.tick(nw)
	}
}
