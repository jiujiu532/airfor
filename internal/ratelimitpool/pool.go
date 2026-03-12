package ratelimitpool

import (
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// KeyState represents which pool a key belongs to.
type KeyState int

const (
	KeyStateActive  KeyState = iota // In the active pool, serving requests
	KeyStateCooling                 // In the cooling pool, waiting for cooldown to expire
	KeyStateStandby                 // In the standby pool, waiting to enter active
	KeyStateInvalid                 // Permanently invalidated (lifetime limit reached)
)

// KeySlot holds all runtime state for a single API key.
type KeySlot struct {
	KeyID         uint      // Database primary key ID
	KeyValue      string    // The actual API key string (decrypted)
	GroupID       uint      // Group this key belongs to
	TotalRequests int64     // Lifetime total request count
	State         KeyState  // Current pool membership
	CooldownUntil time.Time // When this key can leave cooling (only valid if State == KeyStateCooling)

	// requestTimes stores timestamps of recent requests for sliding window calculation.
	// Only timestamps within the current sliding window are kept.
	requestTimes []time.Time
}

// windowCount returns how many requests occurred in the last `window` duration.
func (k *KeySlot) windowCount(window time.Duration) int {
	cutoff := time.Now().Add(-window)
	count := 0
	for _, t := range k.requestTimes {
		if t.After(cutoff) {
			count++
		}
	}
	return count
}

// pruneOldTimestamps removes timestamps older than the sliding window.
func (k *KeySlot) pruneOldTimestamps(window time.Duration) {
	cutoff := time.Now().Add(-window)
	n := 0
	for _, t := range k.requestTimes {
		if t.After(cutoff) {
			k.requestTimes[n] = t
			n++
		}
	}
	k.requestTimes = k.requestTimes[:n]
}

// recordRequest adds a timestamp and increments the total request count.
func (k *KeySlot) recordRequest() {
	k.requestTimes = append(k.requestTimes, time.Now())
	k.TotalRequests++
}

// PoolStats holds real-time statistics about the pool state.
type PoolStats struct {
	ActiveCount  int   `json:"active_count"`
	CoolingCount int   `json:"cooling_count"`
	StandbyCount int   `json:"standby_count"`
	InvalidCount int   `json:"invalid_count"`
	TotalCount   int   `json:"total_count"`
	WaitingQueue int   `json:"waiting_queue"`
	ActiveCap    int   `json:"active_cap"` // configured active pool size
	TotalServed  int64 `json:"total_served"`
}

// OnStandbyLowFunc is called when the standby pool drops below the refill threshold.
// currentCount is the current number of standby keys.
type OnStandbyLowFunc func(currentCount int)

// Pool is the central three-pool key rotation engine.
type Pool struct {
	mu        sync.Mutex
	available *sync.Cond // broadcast when a key becomes available in active pool

	// The three pools
	activeKeys  []*KeySlot // ring buffer of active keys
	coolingKeys []*KeySlot // keys waiting for cooldown expiry
	standbyKeys []*KeySlot // keys waiting to enter active pool
	invalidKeys []*KeySlot // permanently invalidated keys

	cursor int // current position in activeKeys ring buffer

	config PoolConfig

	// Callback when standby pool is low
	onStandbyLow OnStandbyLowFunc

	// Counters
	totalServed  int64
	waitingCount int

	// Lifecycle
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewPool creates a new rate-limited key pool with the given configuration.
func NewPool(config PoolConfig) *Pool {
	p := &Pool{
		config:      config,
		activeKeys:  make([]*KeySlot, 0, config.ActivePoolSize),
		coolingKeys: make([]*KeySlot, 0),
		standbyKeys: make([]*KeySlot, 0),
		invalidKeys: make([]*KeySlot, 0),
		stopCh:      make(chan struct{}),
	}
	p.available = sync.NewCond(&p.mu)
	return p
}

// Start begins the background maintenance goroutines.
func (p *Pool) Start() {
	p.wg.Add(1)
	go p.maintenanceLoop()
	logrus.Info("[RateLimitPool] Pool started with maintenance loop")
}

// Stop gracefully stops the pool and waits for goroutines to finish.
func (p *Pool) Stop() {
	close(p.stopCh)
	// Wake up any waiting AcquireKey goroutines
	p.available.Broadcast()
	p.wg.Wait()
	logrus.Info("[RateLimitPool] Pool stopped")
}

// SetOnStandbyLow sets the callback for when standby pool drops below threshold.
func (p *Pool) SetOnStandbyLow(fn OnStandbyLowFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onStandbyLow = fn
}

// LoadKeys initializes the pool with a batch of keys.
// This should be called once at startup.
func (p *Pool) LoadKeys(keys []KeySlot) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.activeKeys = p.activeKeys[:0]
	p.standbyKeys = p.standbyKeys[:0]
	p.coolingKeys = p.coolingKeys[:0]
	p.invalidKeys = p.invalidKeys[:0]

	for i := range keys {
		slot := &keys[i]
		slot.State = KeyStateStandby
		p.standbyKeys = append(p.standbyKeys, slot)
	}

	// Fill the active pool from standby
	p.fillActiveFromStandby()

	// Pre-fill cooling pool reserves from standby
	p.fillCoolingReserves()

	logrus.WithFields(logrus.Fields{
		"active":  len(p.activeKeys),
		"cooling": len(p.coolingKeys),
		"standby": len(p.standbyKeys),
		"total":   len(keys),
	}).Info("[RateLimitPool] Keys loaded into pool")
}

// AddKey adds a single new key to the standby pool.
// If the active pool has capacity, the key is immediately promoted.
func (p *Pool) AddKey(slot KeySlot) {
	p.mu.Lock()
	defer p.mu.Unlock()

	s := &slot
	if len(p.activeKeys) < p.config.ActivePoolSize {
		s.State = KeyStateActive
		p.activeKeys = append(p.activeKeys, s)
		p.available.Broadcast()
	} else {
		s.State = KeyStateStandby
		p.standbyKeys = append(p.standbyKeys, s)
	}
}

// AddKeys adds multiple keys to the pool.
func (p *Pool) AddKeys(keys []KeySlot) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i := range keys {
		s := &keys[i]
		if len(p.activeKeys) < p.config.ActivePoolSize {
			s.State = KeyStateActive
			p.activeKeys = append(p.activeKeys, s)
		} else {
			s.State = KeyStateStandby
			p.standbyKeys = append(p.standbyKeys, s)
		}
	}
	p.available.Broadcast()
}

// AddKeyToCooling adds a key directly to the cooling pool with zero cooldown.
// Used for manual registration: key enters the active↔cooling cycle immediately
// via the next maintenanceLoop tick (every 1 second).
func (p *Pool) AddKeyToCooling(slot KeySlot) {
	p.mu.Lock()
	defer p.mu.Unlock()

	s := &slot
	s.State = KeyStateCooling
	s.CooldownUntil = time.Now().Add(-1 * time.Second) // already expired
	p.coolingKeys = append(p.coolingKeys, s)
}

// UpdateConfig hot-reloads the pool configuration.
func (p *Pool) UpdateConfig(config PoolConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.config = config
	// Adjust active pool size: if increased, pull from standby; if decreased, push to standby
	p.fillActiveFromStandby()
	// Adjust cooling pool reserves
	p.fillCoolingReserves()
	logrus.WithFields(logrus.Fields{
		"active_pool_size": config.ActivePoolSize,
		"rate_limit":       fmt.Sprintf("%d/%ds", config.RateLimitCount, config.RateLimitWindowSeconds),
		"cooldown":         fmt.Sprintf("%ds", config.CooldownSeconds),
	}).Info("[RateLimitPool] Configuration updated")
}

// AcquireKey atomically selects an available key from the active pool.
// It blocks (up to RequestWaitTimeout) if all active keys are rate-limited.
// Returns the KeySlot to use, or an error if no key is available within the timeout.
func (p *Pool) AcquireKey() (*KeySlot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	deadline := time.Now().Add(p.config.RequestWaitTimeout())

	for {
		// Check if pool is stopped
		select {
		case <-p.stopCh:
			return nil, fmt.Errorf("pool is shutting down")
		default:
		}

		// First, recover any cooling keys that have expired
		p.recoverCoolingKeysLocked()

		// Try to find an available key in the active pool
		slot := p.findAvailableKeyLocked()
		if slot != nil {
			return slot, nil
		}

		// No key available in active pool — wait for cooling keys to recover.
		// Standby keys do NOT enter active pool directly (closed loop architecture).

		// Still nothing — check timeout
		now := time.Now()
		if now.After(deadline) {
			return nil, fmt.Errorf("all keys are rate-limited, request wait timeout exceeded")
		}

		// Wait for a key to become available (with timeout)
		p.waitingCount++

		// Use a timer-based wait to avoid holding the mutex forever
		go func() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				remaining = time.Millisecond
			}
			timer := time.NewTimer(remaining)
			defer timer.Stop()
			select {
			case <-timer.C:
				p.available.Broadcast()
			case <-p.stopCh:
				p.available.Broadcast()
			}
		}()

		p.available.Wait()
		p.waitingCount--
	}
}

// ReleaseKey should be called after a request completes (success or failure).
// For failed requests (e.g., 401/403), pass markInvalid=true to permanently remove the key.
func (p *Pool) ReleaseKey(slot *KeySlot, markInvalid bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if markInvalid {
		p.invalidateKeyLocked(slot)
		return
	}

	// Nothing else to do — the key's state is managed in AcquireKey.
	// This method exists as a hook for future extensions.
}

// Stats returns a snapshot of the current pool statistics.
func (p *Pool) Stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()

	return PoolStats{
		ActiveCount:  len(p.activeKeys),
		CoolingCount: len(p.coolingKeys),
		StandbyCount: len(p.standbyKeys),
		InvalidCount: len(p.invalidKeys),
		TotalCount:   len(p.activeKeys) + len(p.coolingKeys) + len(p.standbyKeys),
		WaitingQueue: p.waitingCount,
		ActiveCap:    p.config.ActivePoolSize,
		TotalServed:  p.totalServed,
	}
}

// GetConfig returns the current pool configuration.
func (p *Pool) GetConfig() PoolConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.config
}

// --- Internal methods (must be called with p.mu held) ---

// findAvailableKeyLocked scans the active pool ring buffer for a key that
// has capacity in its sliding window. If found, the request is recorded on that key.
func (p *Pool) findAvailableKeyLocked() *KeySlot {
	n := len(p.activeKeys)
	if n == 0 {
		return nil
	}

	window := p.config.RateLimitWindow()
	limit := p.config.RateLimitCount

	// Scan from cursor, wrapping around
	for i := 0; i < n; i++ {
		idx := (p.cursor + i) % n
		slot := p.activeKeys[idx]

		// Prune old timestamps and check count
		slot.pruneOldTimestamps(window)
		count := len(slot.requestTimes)

		if count < limit {
			// This key has capacity — use it
			slot.recordRequest()
			p.totalServed++

			// Advance cursor past this key for round-robin fairness
			p.cursor = (idx + 1) % n

			// Check if this key just hit the rate limit
			if len(slot.requestTimes) >= limit {
				p.moveToCoolingLocked(idx)
			}

			// Check if this key hit the lifetime limit
			if p.config.MaxTotalRequests > 0 && slot.TotalRequests >= p.config.MaxTotalRequests {
				p.invalidateKeyLocked(slot)
			}

			return slot
		}
	}

	return nil
}

// moveToCoolingLocked moves the key at activeKeys[idx] to the cooling pool.
func (p *Pool) moveToCoolingLocked(idx int) {
	slot := p.activeKeys[idx]
	slot.State = KeyStateCooling
	slot.CooldownUntil = time.Now().Add(p.config.CooldownDuration())

	// Remove from active pool (swap with last, then truncate)
	last := len(p.activeKeys) - 1
	if idx != last {
		p.activeKeys[idx] = p.activeKeys[last]
	}
	p.activeKeys = p.activeKeys[:last]

	// Adjust cursor if needed
	if len(p.activeKeys) > 0 {
		p.cursor = p.cursor % len(p.activeKeys)
	} else {
		p.cursor = 0
	}

	// Add to cooling pool
	p.coolingKeys = append(p.coolingKeys, slot)

	logrus.WithFields(logrus.Fields{
		"key_id":     slot.KeyID,
		"cooldown_s": p.config.CooldownSeconds,
		"active":     len(p.activeKeys),
		"cooling":    len(p.coolingKeys),
	}).Debug("[RateLimitPool] Key moved to cooling pool")

	// Active↔Cooling is a closed loop — do NOT fill from standby here.
	// The cooling key will naturally return to active when its cooldown expires.
}

// invalidateKeyLocked permanently removes a key from whatever pool it's in.
func (p *Pool) invalidateKeyLocked(slot *KeySlot) {
	slot.State = KeyStateInvalid

	// Remove from active pool
	p.removeFromSliceLocked(&p.activeKeys, slot)
	// Remove from cooling pool
	p.removeFromSliceLocked(&p.coolingKeys, slot)
	// Remove from standby pool
	p.removeFromSliceLocked(&p.standbyKeys, slot)

	p.invalidKeys = append(p.invalidKeys, slot)

	logrus.WithFields(logrus.Fields{
		"key_id":         slot.KeyID,
		"total_requests": slot.TotalRequests,
	}).Warn("[RateLimitPool] Key permanently invalidated")

	// Adjust cursor
	if len(p.activeKeys) > 0 {
		p.cursor = p.cursor % len(p.activeKeys)
	} else {
		p.cursor = 0
	}

	// Replace the dead key: move a standby key into cooling pool (with 0 cooldown).
	// It will be recovered to active on the next maintenanceLoop tick (every 1 second).
	p.fillCoolingFromStandby()
}

// recoverCoolingKeysLocked moves expired cooling keys back to the active pool.
func (p *Pool) recoverCoolingKeysLocked() {
	now := time.Now()
	recovered := 0

	// Iterate in reverse so we can safely remove during iteration
	for i := len(p.coolingKeys) - 1; i >= 0; i-- {
		slot := p.coolingKeys[i]
		if now.After(slot.CooldownUntil) {
			if len(p.activeKeys) < p.config.ActivePoolSize {
				// Active pool has room — promote this key
				slot.requestTimes = slot.requestTimes[:0]
				slot.State = KeyStateActive

				// Remove from cooling
				p.coolingKeys = append(p.coolingKeys[:i], p.coolingKeys[i+1:]...)

				p.activeKeys = append(p.activeKeys, slot)
				recovered++
			}
			// else: Active pool is full — leave in cooling as reserve.
			// This key will be promoted when the next active key enters cooling.
		}
	}

	if recovered > 0 {
		logrus.WithFields(logrus.Fields{
			"recovered": recovered,
			"active":    len(p.activeKeys),
			"cooling":   len(p.coolingKeys),
		}).Debug("[RateLimitPool] Recovered cooling keys")
	}
}

// fillActiveFromStandby promotes keys from standby to active pool to fill vacancies.
// Only used during initial LoadKeys; runtime uses fillCoolingFromStandby instead.
func (p *Pool) fillActiveFromStandby() {
	for len(p.activeKeys) < p.config.ActivePoolSize && len(p.standbyKeys) > 0 {
		// Take from the front of standby (FIFO)
		slot := p.standbyKeys[0]
		p.standbyKeys = p.standbyKeys[1:]

		slot.State = KeyStateActive
		p.activeKeys = append(p.activeKeys, slot)
	}

	// Check if standby is running low
	p.checkStandbyThresholdLocked()
}

// fillCoolingFromStandby moves a standby key into the cooling pool with zero cooldown.
// The key will be recovered to active on the next maintenanceLoop tick (every 1 second).
// This is called when a key permanently dies to maintain the active+cooling key count.
func (p *Pool) fillCoolingFromStandby() {
	if len(p.standbyKeys) == 0 {
		logrus.Warn("[RateLimitPool] Standby pool empty, cannot replace dead key")
		p.checkStandbyThresholdLocked()
		return
	}

	// Take from the front of standby (FIFO)
	slot := p.standbyKeys[0]
	p.standbyKeys = p.standbyKeys[1:]

	// Set to cooling with zero cooldown — will be recovered immediately
	slot.State = KeyStateCooling
	slot.CooldownUntil = time.Now().Add(-1 * time.Second) // already expired
	p.coolingKeys = append(p.coolingKeys, slot)

	logrus.WithFields(logrus.Fields{
		"key_id":  slot.KeyID,
		"standby": len(p.standbyKeys),
	}).Info("[RateLimitPool] Standby key moved to cooling pool (replacing dead key)")

	// Check if standby is running low
	p.checkStandbyThresholdLocked()
}

// checkStandbyThresholdLocked fires the onStandbyLow callback if needed.
func (p *Pool) checkStandbyThresholdLocked() {
	if p.onStandbyLow != nil && len(p.standbyKeys) < p.config.StandbyRefillThreshold {
		count := len(p.standbyKeys)
		// Fire callback asynchronously to avoid deadlocks
		go p.onStandbyLow(count)
	}
}

// fillCoolingReserves pre-fills the cooling pool from standby up to CoolingPoolTarget.
// Reserve keys have expired cooldowns, making them immediately available to fill
// active pool vacancies when keys go to cooling due to rate limiting.
func (p *Pool) fillCoolingReserves() {
	target := p.config.CoolingPoolTarget()
	added := 0

	for len(p.coolingKeys) < target && len(p.standbyKeys) > 0 {
		// Take from front of standby (FIFO)
		slot := p.standbyKeys[0]
		p.standbyKeys = p.standbyKeys[1:]

		slot.State = KeyStateCooling
		slot.CooldownUntil = time.Now().Add(-1 * time.Second) // already expired = reserve
		p.coolingKeys = append(p.coolingKeys, slot)
		added++
	}

	if added > 0 {
		logrus.WithFields(logrus.Fields{
			"added":   added,
			"cooling": len(p.coolingKeys),
			"target":  target,
			"standby": len(p.standbyKeys),
		}).Info("[RateLimitPool] Pre-filled cooling pool reserves from standby")

		// Standby may now be low — check threshold
		p.checkStandbyThresholdLocked()
	}
}

// removeFromSliceLocked removes a specific KeySlot from a slice by pointer identity.
func (p *Pool) removeFromSliceLocked(slice *[]*KeySlot, target *KeySlot) {
	s := *slice
	for i, slot := range s {
		if slot == target {
			*slice = append(s[:i], s[i+1:]...)
			return
		}
	}
}

// maintenanceLoop runs periodically to recover cooling keys and check pool health.
func (p *Pool) maintenanceLoop() {
	defer p.wg.Done()

	// Tick every second for responsive cooldown recovery
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// Periodic pool health check every 30 seconds
	healthTicker := time.NewTicker(30 * time.Second)
	defer healthTicker.Stop()

	for {
		select {
		case <-ticker.C:
			p.mu.Lock()
			recovered := p.recoverCoolingAndBroadcast()
			p.mu.Unlock()

			if recovered {
				// Wake up any waiting AcquireKey calls
				p.available.Broadcast()
			}
		case <-healthTicker.C:
			// Periodic health check: refill cooling reserves and check standby
			p.mu.Lock()
			p.fillCoolingReserves()
			p.checkStandbyThresholdLocked()
			p.mu.Unlock()
		case <-p.stopCh:
			return
		}
	}
}

// recoverCoolingAndBroadcast recovers expired cooling keys and returns true if any were recovered.
func (p *Pool) recoverCoolingAndBroadcast() bool {
	before := len(p.coolingKeys)
	p.recoverCoolingKeysLocked()
	return len(p.coolingKeys) < before
}
