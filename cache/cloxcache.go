// Package cache provides a lock-free adaptive in-memory cache implementation.
// CloxCache uses protected-freq eviction: items with high frequency are protected,
// with LRU as a tiebreaker among same-frequency items.
package cache

import (
	"math/bits"
	"sync"
	"sync/atomic"
)

const (
	// maxFrequency is the maximum value for the frequency counter (0-15 range)
	maxFrequency = 15

	// initialFreq is the starting frequency for new keys
	initialFreq = 1

	// defaultProtectedFreqThreshold - items with freq > this are protected from eviction.
	// Analysis shows freq>=3 items are likely to return
	defaultProtectedFreqThreshold = 2

	// adaptiveCheckInterval - check graduation rate every N evictions
	adaptiveCheckInterval = 1000

	// Default graduation rate thresholds (will be learned per-shard)
	defaultRateLow  = 2500 // 0.25 * 10000
	defaultRateHigh = 5000 // 0.50 * 10000

	// Bounds for learned thresholds
	minRateLow  = 500  // 0.05 - minimum low threshold
	maxRateLow  = 4000 // 0.40 - maximum low threshold
	minRateHigh = 3000 // 0.30 - minimum high threshold
	maxRateHigh = 8000 // 0.80 - maximum high threshold

	// Learning rate for threshold adjustment (how much to nudge per feedback)
	thresholdLearningRate = 1000 // 0.10 per adjustment (aggressive)

	// Window size for measuring hit rate effect of k changes
	hitRateWindowSize = 2000 // smaller window = faster feedback

)

// Key is a type constraint for cache keys (string or []byte)
type Key interface {
	~string | ~[]byte
}

// CloxCache is a lock-free adaptive in-memory cache.
// It stores generic keys of type K (string or []byte) and values of type V.
type CloxCache[K Key, V any] struct {
	shards    []shard[K, V]
	numShards int
	shardBits int

	// Configuration
	collectStats bool
	sweepPercent int // Percentage of shard to scan during eviction (1-100)

	// Metrics (only updated when collectStats is true)
	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64

	// Lifecycle management
	stop      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// shard contains a portion of the cache slots with minimal lock contention
type shard[K Key, V any] struct {
	slots      []atomic.Pointer[recordNode[K, V]]
	mu         sync.Mutex    // only for insertions and sweeper unlink
	entryCount atomic.Int64  // live entries in this shard
	capacity   int64         // max live entries for this shard
	hand       atomic.Uint64 // per-shard CLOCK hand position
	timestamp  atomic.Uint64 // per-shard timestamp for LRU ordering

	// Ghost tracking - ghosts have freq <= 0, |freq| is remembered frequency
	ghostCount    atomic.Int64 // ghost entries in this shard
	ghostCapacity int64        // max ghosts = slotsPerShard - capacity

	// Adaptive threshold tracking (per-shard, no global contention)
	k                  atomic.Int32  // current protection threshold for this shard
	evictedUnprotected atomic.Uint64 // evicted with freq <= k (unprotected)
	evictedProtected   atomic.Uint64 // evicted with freq > k (protected, fallback)
	reachedProtected   atomic.Uint64 // items whose freq crossed the shard's current k (graduated)
	lastAdaptCheck     atomic.Uint64 // eviction count at last adaptation check

	// Self-tuning threshold learning (gradient descent on hit rate)
	windowHits     atomic.Uint64 // hits in current measurement window
	windowOps      atomic.Uint64 // total ops in current measurement window
	prevHitRate    atomic.Uint64 // previous window hit rate * 10000 (for atomic storage)
	lastKDirection atomic.Int32  // +1 if k increased, -1 if decreased, 0 if no change
	rateLow        atomic.Uint32 // adaptive low threshold * 10000
	rateHigh       atomic.Uint32 // adaptive high threshold * 10000
}

// recordNode is a cache entry with collision chaining
// When freq > 0: live entry with that frequency
// When freq <= 0: ghost entry, |freq| is the remembered frequency
type recordNode[K Key, V any] struct {
	value      atomic.Value                     // value stored (stale for ghosts)
	next       atomic.Pointer[recordNode[K, V]] // chain traversal
	keyHash    uint64                           // fast hash comparison
	freq       atomic.Int32                     // access frequency (negative = ghost)
	lastAccess atomic.Uint64                    // timestamp for LRU tiebreaking
	key        K
}

// Config holds CloxCache configuration
type Config struct {
	NumShards     int  // Must be power of 2
	SlotsPerShard int  // Must be power of 2
	Capacity      int  // Max entries (0 = use SlotsPerShard * NumShards as default)
	CollectStats  bool // Enable hit/miss/eviction counters
	// (recommend: 15 for temporal workloads and low latency)
	SweepPercent int // Percentage of shard to scan during eviction
}

// NewCloxCache creates a new cache with the given configuration
func NewCloxCache[K Key, V any](cfg Config) *CloxCache[K, V] {
	// Validate positive values
	if cfg.NumShards <= 0 {
		panic("NumShards must be positive")
	}
	if cfg.SlotsPerShard <= 0 {
		panic("SlotsPerShard must be positive")
	}

	// Validate power-of-2 requirements
	if cfg.NumShards&(cfg.NumShards-1) != 0 {
		panic("NumShards must be a power of 2")
	}
	if cfg.SlotsPerShard&(cfg.SlotsPerShard-1) != 0 {
		panic("SlotsPerShard must be a power of 2")
	}

	sweepPercent := cfg.SweepPercent
	if sweepPercent <= 0 {
		sweepPercent = 15
	} else if sweepPercent > 100 {
		sweepPercent = 100
	}

	c := &CloxCache[K, V]{
		numShards:    cfg.NumShards,
		shardBits:    bits.Len(uint(cfg.NumShards - 1)),
		shards:       make([]shard[K, V], cfg.NumShards),
		stop:         make(chan struct{}),
		collectStats: cfg.CollectStats,
		sweepPercent: sweepPercent,
	}

	totalCapacity := cfg.Capacity
	if totalCapacity <= 0 {
		totalCapacity = cfg.NumShards * cfg.SlotsPerShard
	}
	perShardCapacity := int64(totalCapacity / cfg.NumShards)
	if perShardCapacity < 1 {
		perShardCapacity = 1
	}

	// Ghost capacity uses unused slot space, capped at 100% of live capacity
	ghostCapacity := int64(cfg.SlotsPerShard) - perShardCapacity
	if ghostCapacity < 0 {
		ghostCapacity = 0
	}
	if ghostCapacity > perShardCapacity {
		ghostCapacity = perShardCapacity
	}

	for i := range c.shards {
		c.shards[i].slots = make([]atomic.Pointer[recordNode[K, V]], cfg.SlotsPerShard)
		c.shards[i].capacity = perShardCapacity
		c.shards[i].ghostCapacity = ghostCapacity
		c.shards[i].k.Store(defaultProtectedFreqThreshold)
		// Initialize self-tuning threshold learning
		c.shards[i].rateLow.Store(defaultRateLow)
		c.shards[i].rateHigh.Store(defaultRateHigh)
	}

	return c
}

// Close stops background goroutines and waits for them to exit.
// Safe to call multiple times.
func (c *CloxCache[K, V]) Close() {
	c.closeOnce.Do(func() {
		close(c.stop)
	})
	c.wg.Wait()
}

func keysEqual[K Key](a, b K) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func copyKey[K Key](key K) K {
	switch k := any(key).(type) {
	case []byte:
		cp := make([]byte, len(k))
		copy(cp, k)
		return any(cp).(K)
	default:
		return key
	}
}

// Get retrieves a value from the cache (lock-free)
func (c *CloxCache[K, V]) Get(key K) (V, bool) {
	var zero V

	hash := hashKey(key)
	shardID := hash & uint64(c.numShards-1)
	slotID := (hash >> c.shardBits) & uint64(len(c.shards[0].slots)-1)

	shard := &c.shards[shardID]
	slot := &shard.slots[slotID]

	// Track ops for hit rate learning (always, even if collectStats is false)
	shard.windowOps.Add(1)

	node := slot.Load()
	for node != nil {
		if node.keyHash == hash && keysEqual(node.key, key) {
			f := node.freq.Load()
			// Skip ghosts (freq <= 0)
			if f <= 0 {
				node = node.next.Load()
				continue
			}

			// Bump frequency (saturating at 15)
			// If already at max, skip all updates - the item is clearly hot
			if f < maxFrequency {
				if node.freq.CompareAndSwap(f, f+1) {
					// Track when items cross into protected status (freq > k)
					// This happens when freq goes from k to k+1
					// Only count when at capacity (under eviction pressure)
					if f == shard.k.Load() && shard.entryCount.Load() >= shard.capacity {
						shard.reachedProtected.Add(1)
					}
					// Only update timestamp when we successfully bumped freq
					// This amortises the cost, and hot items skip updates entirely
					node.lastAccess.Store(shard.timestamp.Add(1))
				}
			}

			// Track hits for hit rate learning
			shard.windowHits.Add(1)

			if c.collectStats {
				c.hits.Add(1)
			}
			return node.value.Load().(V), true
		}
		node = node.next.Load()
	}

	if c.collectStats {
		c.misses.Add(1)
	}
	return zero, false
}

// Put inserts or updates a value in the cache
func (c *CloxCache[K, V]) Put(key K, value V) bool {
	hash := hashKey(key)
	shardID := hash & uint64(c.numShards-1)
	slotID := (hash >> c.shardBits) & uint64(len(c.shards[0].slots)-1)

	shard := &c.shards[shardID]
	slot := &shard.slots[slotID]

	// First, try to update the existing key (lock-free)
	node := slot.Load()
	for node != nil {
		if node.keyHash == hash {
			if keysEqual(node.key, key) {
				f := node.freq.Load()
				// Skip ghosts - we'll handle them under lock
				if f <= 0 {
					node = node.next.Load()
					continue
				}
				// Update existing - bump frequency and update access time
				node.value.Store(value)
				node.lastAccess.Store(shard.timestamp.Add(1))
				for {
					f := node.freq.Load()
					if f >= maxFrequency {
						break
					}
					if node.freq.CompareAndSwap(f, f+1) {
						break
					}
				}
				return true
			}
		}
		node = node.next.Load()
	}

	// Allocate new node with a copied key to prevent caller mutations
	newNode := &recordNode[K, V]{
		keyHash: hash,
		key:     copyKey(key),
	}
	newNode.value.Store(value)
	newNode.freq.Store(initialFreq)
	newNode.lastAccess.Store(shard.timestamp.Add(1))

	// Try CAS onto head
	shard.mu.Lock()
	defer shard.mu.Unlock()

	// Re-check for an existing key under lock (including ghosts)
	node = slot.Load()
	for node != nil {
		if node.keyHash == hash {
			if keysEqual(node.key, key) {
				f := node.freq.Load()
				if f <= 0 {
					// Found a ghost - promote it! Use remembered freq + 1
					promotedFreq := -f + 1
					if promotedFreq > maxFrequency {
						promotedFreq = maxFrequency
					}
					if promotedFreq < initialFreq {
						promotedFreq = initialFreq
					}
					node.value.Store(value)
					node.freq.Store(promotedFreq)
					node.lastAccess.Store(shard.timestamp.Add(1))
					shard.ghostCount.Add(-1)
					shard.entryCount.Add(1)
					return true
				}
				// Someone else inserted it - update value and access time
				node.value.Store(value)
				node.lastAccess.Store(shard.timestamp.Add(1))
				return true
			}
		}
		node = node.next.Load()
	}

	// Evict from this shard if over capacity
	for shard.entryCount.Load() >= shard.capacity {
		evicted := c.evictFromShard(int(shardID), len(shard.slots))
		if evicted == 0 {
			// Couldn't evict anything, break to avoid infinite loop
			return false
		}
	}

	// Insert at head
	head := slot.Load()
	newNode.next.Store(head)
	slot.Store(newNode)
	shard.entryCount.Add(1)

	return true
}

// evictFromShard uses protected-freq eviction with LRU tiebreaking.
// Called during Put when shard is over capacity. Caller must hold shard lock.
// Returns the number of entries evicted (0 or 1).
//
// Algorithm:
// - Scans a portion of the shard (sweepPercent)
// - Finds LRU item among low-frequency items (freq <= k)
// - Falls back to any LRU item if no low-freq items are found
// - Low-freq items become ghosts (freq negated) instead of being removed
// - Adapts k based on graduation rate
func (c *CloxCache[K, V]) evictFromShard(shardID, slotsPerShard int) int {
	shard := &c.shards[shardID]
	k := shard.k.Load()

	// Calculate scan range
	maxScan := slotsPerShard * c.sweepPercent / 100
	if maxScan < 1 {
		maxScan = 1
	}

	// Advance CLOCK hand
	advance := (maxScan + 1) / 2
	startSlot := int(shard.hand.Add(uint64(advance)) % uint64(slotsPerShard))

	// Track the best victims: low-freq preferred, any as fallback
	// Also track oldest ghost for eviction when ghost capacity is full
	var lowFreqVictim, lowFreqPrev *recordNode[K, V]
	var lowFreqSlot *atomic.Pointer[recordNode[K, V]]
	lowFreqAccess := uint64(^uint64(0)) // max value

	var fallbackVictim, fallbackPrev *recordNode[K, V]
	var fallbackSlot *atomic.Pointer[recordNode[K, V]]
	fallbackAccess := uint64(^uint64(0))

	var oldestGhost, oldestGhostPrev *recordNode[K, V]
	var oldestGhostSlot *atomic.Pointer[recordNode[K, V]]
	oldestGhostAccess := uint64(^uint64(0))

	for scanned := 0; scanned < maxScan; scanned++ {
		slotID := (startSlot + scanned) % slotsPerShard
		slot := &shard.slots[slotID]

		node := slot.Load()
		var prev *recordNode[K, V]

		for node != nil {
			access := node.lastAccess.Load()
			freq := node.freq.Load()

			// Skip ghosts for live eviction, but track oldest ghost
			if freq <= 0 {
				if access < oldestGhostAccess {
					oldestGhost = node
					oldestGhostPrev = prev
					oldestGhostSlot = slot
					oldestGhostAccess = access
				}
				prev = node
				node = node.next.Load()
				continue
			}

			// Track LRU among low-freq items (freq <= k, unprotected)
			if freq <= k && access < lowFreqAccess {
				lowFreqVictim = node
				lowFreqPrev = prev
				lowFreqSlot = slot
				lowFreqAccess = access
			}

			// Track LRU overall (fallback)
			if access < fallbackAccess {
				fallbackVictim = node
				fallbackPrev = prev
				fallbackSlot = slot
				fallbackAccess = access
			}

			prev = node
			node = node.next.Load()
		}
	}

	// Choose a victim: prefer low-freq, protect high-freq items
	var victim, victimPrev *recordNode[K, V]
	var victimSlot *atomic.Pointer[recordNode[K, V]]
	isUnprotected := false

	if lowFreqVictim != nil {
		shard.evictedUnprotected.Add(1) // evicting low-freq (unprotected) item
		victim = lowFreqVictim
		victimPrev = lowFreqPrev
		victimSlot = lowFreqSlot
		isUnprotected = true
	} else if fallbackVictim != nil {
		shard.evictedProtected.Add(1) // forced to evict high-freq (protected) item
		victim = fallbackVictim
		victimPrev = fallbackPrev
		victimSlot = fallbackSlot
	}

	if victim == nil {
		return 0
	}

	// Check if we can convert to ghost (only for unprotected items with ghost capacity)
	canGhost := isUnprotected && shard.ghostCapacity > 0 && shard.ghostCount.Load() < shard.ghostCapacity

	// If ghost capacity is full, evict oldest ghost first to make room
	if isUnprotected && shard.ghostCapacity > 0 && !canGhost && oldestGhost != nil {
		// Remove oldest ghost
		next := oldestGhost.next.Load()
		if oldestGhostPrev == nil {
			oldestGhostSlot.Store(next)
		} else {
			oldestGhostPrev.next.Store(next)
		}
		shard.ghostCount.Add(-1)
		canGhost = true
	}

	victimFreq := victim.freq.Load()

	if canGhost {
		// Convert to ghost: negate freq, keep in chain
		victim.freq.Store(-victimFreq)
		shard.entryCount.Add(-1)
		shard.ghostCount.Add(1)
	} else {
		// Fully evict: unlink from chain
		if c.collectStats {
			c.evictions.Add(1)
		}
		shard.entryCount.Add(-1)

		next := victim.next.Load()
		if victimPrev == nil {
			victimSlot.Store(next)
		} else {
			victimPrev.next.Store(next)
		}
	}

	// Periodically adapt k based on graduation rate
	totalEvictions := shard.evictedUnprotected.Load() + shard.evictedProtected.Load()
	lastCheck := shard.lastAdaptCheck.Load()
	if totalEvictions-lastCheck >= adaptiveCheckInterval {
		if shard.lastAdaptCheck.CompareAndSwap(lastCheck, totalEvictions) {
			c.adaptThreshold(shard)
		}
	}

	return 1
}

// adaptThreshold adjusts the per-shard k based on graduation rate.
// Also implements self-tuning: adjusts the rate thresholds based on whether
// k changes actually improved hit rate (gradient descent on hit rate).
// Called periodically during eviction.
func (c *CloxCache[K, V]) adaptThreshold(shard *shard[K, V]) {
	graduated := shard.reachedProtected.Load()
	totalEvictions := shard.evictedUnprotected.Load() + shard.evictedProtected.Load()

	if totalEvictions == 0 {
		return
	}

	// First, check if we have enough data to evaluate the effect of the last k change
	windowOps := shard.windowOps.Load()
	if windowOps >= hitRateWindowSize {
		windowHits := shard.windowHits.Load()
		currentHitRate := uint64(float64(windowHits) / float64(windowOps) * 10000)
		prevHitRate := shard.prevHitRate.Load()
		lastDirection := shard.lastKDirection.Load()

		// Only learn if we have a previous measurement and made a change
		if prevHitRate > 0 && lastDirection != 0 {
			hitRateImproved := currentHitRate > prevHitRate
			rateLow := shard.rateLow.Load()
			rateHigh := shard.rateHigh.Load()

			if lastDirection > 0 {
				// We increased k last time
				if hitRateImproved {
					// Increasing k helped - make it easier to increase (lower highThreshold)
					if rateHigh > minRateHigh+thresholdLearningRate {
						shard.rateHigh.Store(rateHigh - thresholdLearningRate)
					}
				} else {
					// Increasing k hurt - make it harder to increase (raise highThreshold)
					if rateHigh < maxRateHigh-thresholdLearningRate {
						shard.rateHigh.Store(rateHigh + thresholdLearningRate)
					}
				}
			} else {
				// We decreased k last time
				if hitRateImproved {
					// Decreasing k helped - make it easier to decrease (raise lowThreshold)
					if rateLow < maxRateLow-thresholdLearningRate {
						shard.rateLow.Store(rateLow + thresholdLearningRate)
					}
				} else {
					// Decreasing k hurt - make it harder to decrease (lower lowThreshold)
					if rateLow > minRateLow+thresholdLearningRate {
						shard.rateLow.Store(rateLow - thresholdLearningRate)
					}
				}
			}
		}

		// Save current hit rate and reset window
		shard.prevHitRate.Store(currentHitRate)
		shard.windowHits.Store(0)
		shard.windowOps.Store(0)
	}

	// Graduation rate = items that crossed threshold k / total evictions
	// This tells us: "what fraction of items survived long enough to become protected?"
	rate := float64(graduated) / float64(totalEvictions)
	currentK := shard.k.Load()

	// Use the learned thresholds (convert from uint32 * 10000 back to float)
	rateLow := float64(shard.rateLow.Load()) / 10000.0
	rateHigh := float64(shard.rateHigh.Load()) / 10000.0

	var kDirection int32 = 0
	if rate < rateLow && currentK > 1 {
		// Very few items graduating - protection isn't helping
		// Lower k, but never below 1 (need freq>=2 protection to allow graduation)
		shard.k.Store(currentK - 1)
		kDirection = -1
	} else if rate > rateHigh && currentK < maxFrequency-1 {
		// Many items graduating - protection is working, can raise k
		// Cap at maxFrequency-1 so there's always room to reach protected status
		shard.k.Store(currentK + 1)
		kDirection = 1
	}
	shard.lastKDirection.Store(kDirection)

	// Decay counters to weight recent behavior (but keep minimum for signal)
	if graduated > 100 {
		shard.reachedProtected.Store(graduated / 2)
	}
	if totalEvictions > 100 {
		shard.evictedUnprotected.Store(shard.evictedUnprotected.Load() / 2)
		shard.evictedProtected.Store(shard.evictedProtected.Load() / 2)
	}
}

// Stats return cache statistics
func (c *CloxCache[K, V]) Stats() (hits, misses, evictions uint64) {
	return c.hits.Load(), c.misses.Load(), c.evictions.Load()
}

// AdaptiveStats returns per-shard adaptive threshold statistics
type AdaptiveStats struct {
	ShardID            int
	K                  int32   // current protection threshold for this shard
	GraduationRate     float64 // fraction of items whose freq crossed the shard's k
	EvictedUnprotected uint64  // items evicted with freq <= k
	EvictedProtected   uint64  // items evicted with freq > k (fallback)
	ReachedProtected   uint64  // items whose freq crossed the shard's current k
	// Learned thresholds (self-tuning)
	LearnedRateLow  float64 // learned low threshold (rate below which k decreases)
	LearnedRateHigh float64 // learned high threshold (rate above which k increases)
	WindowHitRate   float64 // current window hit rate
}

// GetAdaptiveStats returns adaptive threshold stats for all shards
func (c *CloxCache[K, V]) GetAdaptiveStats() []AdaptiveStats {
	stats := make([]AdaptiveStats, c.numShards)
	for i := range c.shards {
		shard := &c.shards[i]
		graduated := shard.reachedProtected.Load()
		evictedU := shard.evictedUnprotected.Load()
		evictedP := shard.evictedProtected.Load()
		total := evictedU + evictedP

		var rate float64
		if total > 0 {
			rate = float64(graduated) / float64(total)
		}

		// Calculate current window hit rate
		windowOps := shard.windowOps.Load()
		var windowHitRate float64
		if windowOps > 0 {
			windowHitRate = float64(shard.windowHits.Load()) / float64(windowOps)
		}

		stats[i] = AdaptiveStats{
			ShardID:            i,
			K:                  shard.k.Load(),
			GraduationRate:     rate,
			EvictedUnprotected: evictedU,
			EvictedProtected:   evictedP,
			ReachedProtected:   graduated,
			LearnedRateLow:     float64(shard.rateLow.Load()) / 10000.0,
			LearnedRateHigh:    float64(shard.rateHigh.Load()) / 10000.0,
			WindowHitRate:      windowHitRate,
		}
	}
	return stats
}

// AverageK returns the average protection threshold across all shards
func (c *CloxCache[K, V]) AverageK() float64 {
	var sum int32
	for i := range c.shards {
		sum += c.shards[i].k.Load()
	}
	return float64(sum) / float64(c.numShards)
}

// AverageLearnedThresholds returns the average learned rate thresholds across all shards
func (c *CloxCache[K, V]) AverageLearnedThresholds() (rateLow, rateHigh float64) {
	var sumLow, sumHigh uint32
	for i := range c.shards {
		sumLow += c.shards[i].rateLow.Load()
		sumHigh += c.shards[i].rateHigh.Load()
	}
	rateLow = float64(sumLow) / float64(c.numShards) / 10000.0
	rateHigh = float64(sumHigh) / float64(c.numShards) / 10000.0
	return
}
