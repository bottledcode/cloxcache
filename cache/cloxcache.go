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

	// graduationRateLow - if the graduation rate falls below this, lower k
	graduationRateLow = 0.05

	// graduationRateHigh - if the graduation rate rises above this, raise k
	graduationRateHigh = 0.20
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

	// Adaptive threshold tracking (per-shard, no global contention)
	k                  atomic.Uint32 // current protection threshold for this shard
	evictedUnprotected atomic.Uint64 // evicted with freq <= k (unprotected)
	evictedProtected   atomic.Uint64 // evicted with freq > k (protected, fallback)
	reachedProtected   atomic.Uint64 // items whose freq crossed the shard's current k (graduated)
	lastAdaptCheck     atomic.Uint64 // eviction count at last adaptation check
}

// recordNode is a cache entry with collision chaining
type recordNode[K Key, V any] struct {
	value      atomic.Value                     // value stored
	next       atomic.Pointer[recordNode[K, V]] // chain traversal
	keyHash    uint64                           // fast hash comparison
	freq       atomic.Uint32                    // access frequency (0-15)
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

	for i := range c.shards {
		c.shards[i].slots = make([]atomic.Pointer[recordNode[K, V]], cfg.SlotsPerShard)
		c.shards[i].capacity = perShardCapacity
		c.shards[i].k.Store(defaultProtectedFreqThreshold)
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

	node := slot.Load()
	for node != nil {
		if node.keyHash == hash && keysEqual(node.key, key) {
			// Bump frequency (saturating at 15)
			// If already at max, skip all updates - the item is clearly hot
			f := node.freq.Load()
			if f < maxFrequency {
				if node.freq.CompareAndSwap(f, f+1) {
					// Track when items cross into protected status (freq > k)
					// This happens when freq goes from k to k+1
					if f == shard.k.Load() {
						shard.reachedProtected.Add(1)
					}
					// Only update timestamp when we successfully bumped freq
					// This amortises the cost, and hot items skip updates entirely
					node.lastAccess.Store(shard.timestamp.Add(1))
				}
			}

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

	// Re-check for an existing key under lock
	node = slot.Load()
	for node != nil {
		if node.keyHash == hash {
			if keysEqual(node.key, key) {
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
	var lowFreqVictim, lowFreqPrev *recordNode[K, V]
	var lowFreqSlot *atomic.Pointer[recordNode[K, V]]
	lowFreqAccess := uint64(^uint64(0)) // max value

	var fallbackVictim, fallbackPrev *recordNode[K, V]
	var fallbackSlot *atomic.Pointer[recordNode[K, V]]
	fallbackAccess := uint64(^uint64(0))

	for scanned := 0; scanned < maxScan; scanned++ {
		slotID := (startSlot + scanned) % slotsPerShard
		slot := &shard.slots[slotID]

		node := slot.Load()
		var prev *recordNode[K, V]

		for node != nil {
			access := node.lastAccess.Load()
			freq := node.freq.Load()

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

	if lowFreqVictim != nil {
		shard.evictedUnprotected.Add(1) // evicting low-freq (unprotected) item
		victim = lowFreqVictim
		victimPrev = lowFreqPrev
		victimSlot = lowFreqSlot
	} else if fallbackVictim != nil {
		shard.evictedProtected.Add(1) // forced to evict high-freq (protected) item
		victim = fallbackVictim
		victimPrev = fallbackPrev
		victimSlot = fallbackSlot
	}

	if victim == nil {
		return 0
	}

	// Evict the victim
	if c.collectStats {
		c.evictions.Add(1)
	}
	shard.entryCount.Add(-1)

	// Unlink from the chain
	next := victim.next.Load()
	if victimPrev == nil {
		victimSlot.Store(next)
	} else {
		victimPrev.next.Store(next)
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
// Called periodically during eviction.
func (c *CloxCache[K, V]) adaptThreshold(shard *shard[K, V]) {
	graduated := shard.reachedProtected.Load()
	totalEvictions := shard.evictedUnprotected.Load() + shard.evictedProtected.Load()

	if totalEvictions == 0 {
		return
	}

	// Graduation rate = items that crossed threshold k / total evictions
	// This tells us: "what fraction of items survived long enough to become protected?"
	rate := float64(graduated) / float64(totalEvictions)
	currentK := shard.k.Load()

	if rate < graduationRateLow && currentK > 1 {
		// Very few items graduating - protection isn't helping
		// Lower k, but never below 1 (need freq>=2 protection to allow graduation)
		shard.k.Store(currentK - 1)
	} else if rate > graduationRateHigh && currentK < maxFrequency-1 {
		// Many items graduating - protection is working, can raise k
		// Cap at maxFrequency-1 so there's always room to reach protected status
		shard.k.Store(currentK + 1)
	}

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
	K                  uint32  // current protection threshold for this shard
	GraduationRate     float64 // fraction of items whose freq crossed the shard's k
	EvictedUnprotected uint64  // items evicted with freq <= k
	EvictedProtected   uint64  // items evicted with freq > k (fallback)
	ReachedProtected   uint64  // items whose freq crossed the shard's current k
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

		stats[i] = AdaptiveStats{
			ShardID:            i,
			K:                  shard.k.Load(),
			GraduationRate:     rate,
			EvictedUnprotected: evictedU,
			EvictedProtected:   evictedP,
			ReachedProtected:   graduated,
		}
	}
	return stats
}

// AverageK returns the average protection threshold across all shards
func (c *CloxCache[K, V]) AverageK() float64 {
	var sum uint32
	for i := range c.shards {
		sum += c.shards[i].k.Load()
	}
	return float64(sum) / float64(c.numShards)
}
