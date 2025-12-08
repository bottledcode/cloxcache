package cache

import (
	"fmt"
	"runtime"
)

// ConfigFromCapacity creates a CloxCache config for a specific entry capacity.
// Automatically configures optimal shard count and slot sizing.
func ConfigFromCapacity(capacity int) Config {
	if capacity <= 0 {
		capacity = 1000 // reasonable default
	}

	// Total slots = capacity * 3 for optimal performance
	totalSlots := capacity * 3

	// Determine optimal shard count based on both CPU count AND capacity
	numCPU := runtime.NumCPU()

	// Base shards on CPU count (4 shards per core for parallelism)
	shardsFromCPU := numCPU * 4

	// Also scale shards based on capacity to keep chains short
	// Target: ~1000 items per shard max for fast eviction scans
	shardsFromCapacity := capacity / 1000

	// Use the larger of the two
	numShards := shardsFromCPU
	if shardsFromCapacity > numShards {
		numShards = shardsFromCapacity
	}

	// Round up to power of 2 for bit-masking efficiency
	numShards = nextPowerOf2(numShards)

	// Clamp to reasonable bounds (16 min, 8192 max)
	if numShards < 16 {
		numShards = 16
	}
	if numShards > 8192 {
		numShards = 8192
	}

	// Calculate slots per shard (must be power of 2)
	slotsPerShard := totalSlots / numShards
	slotsPerShard = nextPowerOf2(slotsPerShard)
	if slotsPerShard < 64 {
		slotsPerShard = 64
	}

	return Config{
		NumShards:     numShards,
		SlotsPerShard: slotsPerShard,
		Capacity:      capacity,
	}
}

// ConfigFromMemorySize creates a CloxCache config for a specific memory budget.
// Estimates how many entries fit in the given memory and configures accordingly.
func ConfigFromMemorySize(targetBytes uint64) Config {
	// Estimate bytes per entry:
	// - Node overhead: ~96 bytes (atomic pointers, freq, timestamp, key hash)
	// - Average value overhead: ~100 bytes (estimate for typical use)
	// - Slot overhead: ~8 bytes per slot (atomic pointer)
	// With 3x slots per capacity, slot overhead per entry ≈ 24 bytes
	const bytesPerEntry = 220 // 96 + 100 + 24

	capacity := int(targetBytes / bytesPerEntry)
	if capacity < 100 {
		capacity = 100
	}

	return ConfigFromCapacity(capacity)
}

// nextPowerOf2 returns the next power of 2 >= n
func nextPowerOf2(n int) int {
	if n <= 0 {
		return 1
	}

	// Check if already power of 2
	if n&(n-1) == 0 {
		return n
	}

	// Find next power of 2
	power := 1
	for power < n {
		power <<= 1
	}

	return power
}

// EstimateMemoryUsage estimates total memory usage for a given configuration
func (c Config) EstimateMemoryUsage() uint64 {
	const bytesPerNode = 96
	const bytesPerSlot = 8
	const shardOverhead = 64 // approximate overhead per shard struct

	totalSlots := uint64(c.NumShards * c.SlotsPerShard)

	// Slot array memory
	slotArrayMemory := totalSlots * bytesPerSlot

	// Estimate nodes (assume load factor 1.25)
	estimatedNodes := uint64(float64(totalSlots) * 1.25)
	nodeMemory := estimatedNodes * bytesPerNode

	// Shard overhead
	shardMemory := uint64(c.NumShards) * shardOverhead

	return slotArrayMemory + nodeMemory + shardMemory
}

// FormatMemory formats bytes as human-readable string
func FormatMemory(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}

	div, exp := uint64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}

	units := []string{"KB", "MB", "GB", "TB"}
	return fmt.Sprintf("%.1f %s", float64(bytes)/float64(div), units[exp])
}
