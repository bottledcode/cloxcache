# CloxCache

A high-performance, concurrent in-memory cache for Go with adaptive frequency-based eviction.

CloxCache uses an **adaptive protected-frequency eviction** strategy that automatically tunes itself to your workload,
achieving competitive hit rates while providing **4-11x better concurrent throughput** than mutex-protected caches.

## Key Features

- **Adaptive eviction threshold**: Automatically adjusts protection level based on workload characteristics
- **Lock-free reads**: Reads use only atomic operations, enabling massive read concurrency
- **Sharded writes**: Minimises write contention across CPU cores
- **Generic**: Supports `string` or `[]byte` keys with any value type

## How It Works

CloxCache protects frequently accessed items from eviction. The protection threshold (k) adapts automatically:

- **High cache pressure** (small cache relative to the working set): k rises, only protecting the hottest items
- **Low cache pressure** (large cache): k settles at 2, protecting items accessed 3+ times

```go
// Items with freq > k are protected from eviction
// k adapts per-shard based on observed graduation rate
if item.freq <= k {
    // Candidate for eviction
} else {
    // Protected (likely working set)
}
```

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
	cfg := cache.Config{
		NumShards:     64,
		SlotsPerShard: 4096,
		Capacity:      10000,
	}
	c := cache.NewCloxCache[string, *MyData](cfg)
	defer c.Close()

	c.Put("user:123", &MyData{Name: "Alice"})

	if data, ok := c.Get("user:123"); ok {
		fmt.Println(data.Name)
	}
}

type MyData struct {
	Name string
}
```

## Performance

### Hit Rate

Tested on real-world cache traces at various cache capacities:

| Trace        | Capacity | CloxCache | LRU   | Otter (S3-FIFO) | vs LRU     | vs Otter   |
|--------------|----------|-----------|-------|-----------------|------------|------------|
| Twitter17    | 5%       | 67.2%     | 63.8% | 70.7%           | **+3.4%**  | -3.5%      |
| Twitter17    | 50%      | 88.3%     | 88.1% | 88.2%           | **+0.2%**  | **+0.1%**  |
| Twitter44    | 10%      | 67.4%     | 66.5% | 70.1%           | **+0.9%**  | -2.7%      |
| Twitter44    | 50%      | 77.7%     | 77.6% | 77.6%           | **+0.04%** | **+0.08%** |
| OLTP         | 20%      | 67.2%     | 66.8% | 68.2%           | **+0.4%**  | -0.9%      |
| OLTP         | 50%      | 72.5%     | 72.9% | 72.7%           | -0.4%      | -0.2%      |
| CloudPhysics | 50%      | 37.8%     | 36.2% | 37.8%           | **+1.6%**  | **+0.09%** |
| CloudPhysics | 80%      | 43.0%     | 43.0% | 38.0%           | 0%         | **+5.0%**  |

CloxCache consistently beats LRU on most workloads and becomes competitive with Otter at moderate-to-large cache sizes.

### Concurrent Throughput

Where CloxCache really shines - lock-free reads scale linearly with goroutines:

| Goroutines | CloxCache   | SimpleLRU (mutex) | Otter       | CloxCache vs LRU |
|------------|-------------|-------------------|-------------|------------------|
| 1          | 14.3M ops/s | 29.9M ops/s       | 9.5M ops/s  | 0.5x             |
| 4          | 40.0M ops/s | 11.7M ops/s       | 16.7M ops/s | **3.4x**         |
| 8          | 66.6M ops/s | 9.3M ops/s        | 20.9M ops/s | **7.2x**         |
| 16         | 85.9M ops/s | 7.5M ops/s        | 18.8M ops/s | **11.4x**        |

*(90% reads, 10% writes workload)*

### When to Use CloxCache

**Best for:**
- Read-heavy concurrent workloads
- Web/API response caches
- Session stores
- Any workload where reads vastly outnumber the writes

**Consider alternatives when:**
- Cache is tiny relative to the working set (<10%)
- Sequential scan workloads
- Single-threaded applications (LRU is simpler and faster)

## Adaptive Threshold

CloxCache automatically adapts its protection threshold (k)
based on the "graduation rate"—what fraction of items survives long enough to become frequently accessed.

| Cache Pressure            | Typical k | Behavior                    |
|---------------------------|-----------|-----------------------------|
| Very high (1-5% capacity) | 10-14     | Only protect very hot items |
| High (10-20% capacity)    | 7-11      | Selective protection        |
| Medium (30-50% capacity)  | 2-7       | Balanced protection         |
| Low (60%+ capacity)       | 2         | Standard protection         |

This adaptation happens per-shard with no global coordination, maintaining lock-free read performance.

## Configuration

```go
cfg := cache.Config{
    NumShards:     64,    // Must be power of 2, recommend 64-256
    SlotsPerShard: 4096,  // Must be power of 2
    Capacity:      10000, // Max entries (distributed across shards)
    CollectStats:  true,  // Enable hit/miss/eviction counters
    SweepPercent:  15,    // Percent of shard to scan during eviction (1-100)
}
c := cache.NewCloxCache[string, *MyValue](cfg)
```

## API

```go
// Store a value (returns false if eviction failed)
ok := c.Put(key, value)

// Retrieve a value (lock-free)
value, found := c.Get(key)

// Get statistics (requires CollectStats: true)
hits, misses, evictions := c.Stats()

// Get adaptive threshold stats per shard
adaptiveStats := c.GetAdaptiveStats()

// Get average k across all shards
avgK := c.AverageK()

// Clean shutdown
c.Close()
```

## License

MIT License - see [LICENSE](LICENSE)
