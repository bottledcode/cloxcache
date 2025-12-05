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

	// protectedFreqThreshold - items with freq > this is protected from eviction.
	// Analysis shows freq>=3 items that will likely return
	protectedFreqThreshold = 2
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
// - Finds LRU item among low-frequency items (freq <= protectedFreqThreshold)
// - Falls back to any LRU item if no low-freq items are found
// - No decay - preserves frequency information
func (c *CloxCache[K, V]) evictFromShard(shardID, slotsPerShard int) int {
	shard := &c.shards[shardID]

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

			// Track LRU among low-freq items (protected threshold)
			if freq <= protectedFreqThreshold && access < lowFreqAccess {
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
		victim = lowFreqVictim
		victimPrev = lowFreqPrev
		victimSlot = lowFreqSlot
	} else if fallbackVictim != nil {
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

	return 1
}

// Stats return cache statistics
func (c *CloxCache[K, V]) Stats() (hits, misses, evictions uint64) {
	return c.hits.Load(), c.misses.Load(), c.evictions.Load()
}
