# CloxCache

A high-performance, lock-free, adaptive in-memory LRU cache for Go.
CloxCache combines CLOCK-Pro eviction with TinyLFU admission and optional adaptive frequency decay.

## Features

- **Lock-free reads**: Get operations are fully lock-free for maximum throughput
- **Generic keys and values**: Supports `string` or `[]byte` keys with any value type
- **Automatic hardware detection**: Configures itself based on CPU count and available memory
- **Adaptive eviction**: Optional adaptive decay adjusts eviction aggressiveness based on workload
- **Low overhead**: Minimal memory overhead with efficient bit-masking for shard routing

## Installation

```bash
go get github.com/bottledcode/cloxcache
```

## Quick Start

```go
package main

import (
	"fmt"
	"github.com/bottledcode/cloxcache/cache"
)

func main() {
	// Auto-detect hardware and configure cache
	cfg := cache.ConfigFromHardware()
	c := cache.NewCloxCache[string, *MyData](cfg)
	defer c.Close()

	// Store a value
	c.Put("user:123", &MyData{Name: "Alice"})

	// Retrieve a value
	if data, ok := c.Get("user:123"); ok {
		fmt.Println(data.Name)
	}
}

type MyData struct {
	Name string
}
```

## Configuration

### Automatic Hardware Detection (Recommended)

```go
cfg := cache.ConfigFromHardware()
c := cache.NewCloxCache[string, *MyValue](cfg)
```

This detects your system’s hardware and configures:

- **Cache size**: 10% of total RAM (bounded: 256MB — 16GB)
- **Shard count**: 4–8 shards per CPU core (power of 2, range: 16–256)
- **Slots per shard**: Calculated for optimal load factor (power of 2, range: 256–65,536)

### Manual Size Configuration

```go
// Configure for a specific memory target
cfg := cache.ConfigFromMemorySize(2 * 1024 * 1024 * 1024) // 2GB
c := cache.NewCloxCache[[]byte, *MyValue](cfg)
```

### Full Manual Configuration

```go
cfg := cache.Config{
NumShards:     64,   // Must be power of 2
SlotsPerShard: 4096, // Must be power of 2
CollectStats:  true, // Enable hit/miss/eviction counters
AdaptiveDecay: false, // Enable adaptive decay tuning
}
c := cache.NewCloxCache[string, *MyValue](cfg)
```

## API Reference

### Creating a Cache

```go
// From hardware detection
cfg := cache.ConfigFromHardware()

// From memory target
cfg := cache.ConfigFromMemorySize(targetBytes uint64)

// Create cache with key type K (string or []byte) and value type V
c := cache.NewCloxCache[K, V](cfg)
```

### Operations

```go
// Store a value (returns false if admission rejected)
admitted := c.Put(key, value)

// Retrieve a value
value, found := c.Get(key)

// Get statistics (only meaningful when CollectStats is enabled)
hits, misses, evictions := c.Stats()

// Clean shutdown (stops background goroutines)
c.Close()
```

### Hardware Detection Utilities

```go
// Get hardware info
hw := cache.DetectHardware()
fmt.Printf("CPUs: %d, RAM: %s\n", hw.NumCPU, cache.FormatMemory(hw.TotalMemory))

// Estimate memory for a config
estimated := cfg.EstimateMemoryUsage()
fmt.Printf("Estimated memory: %s\n", cache.FormatMemory(estimated))
```

## Performance Options

### CollectStats

Tracks hit/miss/eviction counters. Adds atomic counter increments on every Get operation.

```go
cfg := cache.Config{
NumShards:     64,
SlotsPerShard: 4096,
CollectStats:  true,
}
```

### AdaptiveDecay

Dynamically adjusts eviction aggressiveness based on hit rate and admission pressure.
Automatically enables and requires `CollectStats`.

```go
cfg := cache.Config{
NumShards:     64,
SlotsPerShard: 4096,
AdaptiveDecay: true, // implies CollectStats
}
```

**Behavior:**

- Monitors hit rate and admission rejection pressure every second
- Adjusts decay step (1–4) based on cache pressure
- Higher pressure + lower hit rate = more aggressive eviction

### Performance Comparison

| Configuration         | Get Latency | Use Case             |
|-----------------------|-------------|----------------------|
| Default (both false)  | ~26ns       | Maximum throughput   |
| `CollectStats: true`  | ~52ns       | Observability needed |
| `AdaptiveDecay: true` | ~52ns       | Variable workloads   |

## Configuration Algorithm

### Shard Calculation

```
numShards = nextPowerOf2(numCPU * 4)
numShards = clamp(numShards, 16, 256)
```

4–8 shards per core provide good parallelism, while power-of-2 enables efficient bit-masking.

### Slot Calculation

```
estimatedRecords = cacheSize / (recordSize + nodeSize + slotSize/loadFactor)
totalSlots = estimatedRecords / targetLoadFactor
slotsPerShard = nextPowerOf2(totalSlots / numShards)
slotsPerShard = clamp(slotsPerShard, 256, 65536)
```

**Assumptions:**

- Average record: 200 bytes
- Node overhead: 96 bytes
- Slot overhead: 8 bytes
- Target load factor: 1.0

### Memory Estimation

```
slotArrayMemory = totalSlots * 8 bytes
nodeMemory = (totalSlots * 1.25) * 96 bytes
shardOverhead = numShards * 64 bytes
totalMemory = slotArrayMemory + nodeMemory + shardOverhead
```

## Sizing Recommendations

| Use Case          | CPUs  | RAM      | Recommended Config                |
|-------------------|-------|----------|-----------------------------------|
| Development       | 2-4   | 4-8GB    | `ConfigFromMemorySize(256MB)`     |
| Small Production  | 4-8   | 8-16GB   | `ConfigFromMemorySize(512MB-1GB)` |
| Medium Production | 8-16  | 16-32GB  | `ConfigFromMemorySize(1-2GB)`     |
| Large Production  | 16-32 | 32-64GB  | `ConfigFromMemorySize(2-4GB)`     |
| Dedicated Server  | 32-64 | 64-256GB | `ConfigFromMemorySize(4-16GB)`    |

## Tuning Guidelines

### Shard Count

- More shards = better parallelism on multi-core systems
- Too many shards = overhead from shard structures
- Sweet spot: 4–8 shards per CPU core

### Load Factor

- Target: 1.0-1.25 (avg 1 node per slot)
- Lower = faster lookups, more memory
- Higher = slower lookups, less memory
- CloxCache uses chaining, so load factor > 1.0 is acceptable

### Memory vs. Hit Rate

- Larger cache = higher hit rate
- Monitor eviction rate — high evictions may indicate the cache is too small

## Troubleshooting

### High Eviction Rate

If `Put()` frequently returns `false`, the cache is under pressure. Increase cache size:

```go
cfg := cache.ConfigFromMemorySize(4 * 1024 * 1024 * 1024) // 4GB
```

### Memory Pressure

If system memory is constrained, reduce the cache size:

```go
cfg := cache.ConfigFromMemorySize(256 * 1024 * 1024) // 256MB
```

### Poor Hit Rate

If the hit rate is low despite adequate memory:

- Working set may be larger than cache — increase size
- Access pattern may have many cold keys — cache is working as designed

## License

See [LICENSE](LICENSE) file — MIT License.
