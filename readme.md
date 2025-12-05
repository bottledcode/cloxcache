# CloxCache

A high-performance, concurrent in-memory cache for Go with theoretically grounded eviction.

CloxCache uses a **protected-frequency eviction** strategy that achieves 81–99% of Bélády’s optimal hit rate on
real-world workloads, validated on 10 diverse cache traces.

## Key Insight

Items accessed 3+ times are likely part of the working set.
Protecting them from eviction significantly improves hit rates for workloads with temporal locality
(web, database, API caching).

```go
// The core idea: protect frequently-accessed items
if item.freq <= 2 {
// Candidate for eviction (one-shot or uncertain)
} else {
// Protected (likely working set)
}
```

## Features

- **Protected-frequency eviction**: Validated on 10 traces to achieve 81–99% of optimal
- **Concurrent**: Lock-free reads, sharded writes
- **Generic**: Supports `string` or `[]byte` keys with any value type
- **Autoconfigured**: Detects hardware and configures shards/capacity automatically
- **Low overhead**: O(1) operations with minimal memory overhead

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

## Performance

Tested against Bélády’s optimal algorithm (which knows the future) on real-world traces:

| Workload  | CloxCache | Optimal | Efficiency |
|-----------|-----------|---------|------------|
| Zipf-1.2  | 88.95%    | 90.06%  | **98.8%**  |
| Twitter   | 72.98%    | 77.36%  | **94.3%**  |
| Zipf-1.01 | 73.26%    | 80.16%  | **91.4%**  |
| OLTP      | 47.27%    | 58.00%  | **81.5%**  |

### When It Works Best

The protected-frequency strategy excels on workloads with **temporal locality**:

- Web application caches
- Database query caches
- API response caches
- Session stores

### When to Consider Alternatives

For **scan-resistant workloads** (sequential scans, batch processing), the strategy may underperform. Consider:

- Larger cache sizes
- A different cache

## Configuration

### Automatic (Recommended)

```go
cfg := cache.ConfigFromHardware()
c := cache.NewCloxCache[string, *MyValue](cfg)
```

Automatically configures:

- Cache size: 10% of RAM (256MB—16GB)
- Shards: 4–8 per CPU core (16–256 total)
- Slots per shard: Based on a target load factor

### Memory Target

```go
cfg := cache.ConfigFromMemorySize(2 * 1024 * 1024 * 1024) // 2GB
c := cache.NewCloxCache[[]byte, *MyValue](cfg)
```

### Manual

```go
cfg := cache.Config{
NumShards:     64,   // Must be power of 2
SlotsPerShard: 4096, // Must be power of 2
CollectStats:  true, // Enable hit/miss counters
}
c := cache.NewCloxCache[string, *MyValue](cfg)
```

## API

```go
// Store a value
c.Put(key, value)

// Retrieve a value
value, found := c.Get(key)

// Get statistics (requires CollectStats: true)
hits, misses, evictions := c.Stats()

// Clean shutdown
c.Close()
```

## Theoretical Foundation

The eviction strategy is backed by three theorems, validated empirically on 10 traces:

1. **Frequency-Future Correlation**: Higher observed frequency correlates with higher reaccessed probability (confirmed on
   9/10 traces)

2. **Optimal Threshold**: For Zipf-distributed workloads, k=2 (protect freq>=3) is optimal (confirmed on 5/10 real-world
   traces)

3. **Hit Rate Bound**: Protection improves hit rate proportional to the reaccessed probability difference (8/10 traces
   show improvement)

## Sizing Guide

| Use Case          | CPUs  | RAM     | Recommended                       |
|-------------------|-------|---------|-----------------------------------|
| Development       | 2-4   | 4-8GB   | `ConfigFromMemorySize(256MB)`     |
| Small Production  | 4-8   | 8-16GB  | `ConfigFromMemorySize(512MB-1GB)` |
| Medium Production | 8-16  | 16-32GB | `ConfigFromMemorySize(1-2GB)`     |
| Large Production  | 16-32 | 32-64GB | `ConfigFromMemorySize(2-4GB)`     |

## License

MIT License - see [LICENSE](LICENSE)
