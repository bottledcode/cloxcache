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

| Trace        | Capacity | CloxCache | LRU    | Otter (S3-FIFO) | vs LRU  | vs Otter | Avg K |
|--------------|----------|-----------|--------|-----------------|---------|----------|-------|
| OLTP         | 10%      | 61.99%    | 61.48% | 63.23%          | +0.51%  | -1.24%   | 7.97  |
| OLTP         | 20%      | 67.31%    | 66.83% | 68.15%          | +0.48%  | -0.85%   | 8.39  |
| OLTP         | 50%      | 72.49%    | 72.92% | 72.66%          | -0.43%  | -0.17%   | 7.00  |
| Twitter52    | 10%      | 76.24%    | 76.04% | 78.54%          | +0.20%  | -2.30%   | 5.09  |
| Twitter52    | 20%      | 79.75%    | 79.70% | 80.61%          | +0.04%  | -0.86%   | 7.00  |
| Twitter52    | 50%      | 82.68%    | 82.67% | 82.57%          | +0.01%  | +0.11%   | 2.00  |
| CloudPhysics | 50%      | 37.84%    | 36.21% | 37.75%          | +1.63%  | +0.09%   | 2.00  |
| CloudPhysics | 80%      | 42.95%    | 42.99% | 37.95%          | -0.04%  | +5.00%   | 2.00  |

CloxCache consistently beats LRU on most workloads and becomes competitive with Otter at moderate-to-large cache sizes.

<details>
<summary>Raw benchmark output</summary>

```
=== RUN   TestCapacitySweep/OLTP
    Trace: OLTP, Accesses: 500000, Unique keys: 121783

    | Capacity | CloxCache | SimpleLRU | Otter | Clox vs LRU | Clox vs Otter | Avg K |
    |----------|-----------|-----------|-------|-------------|---------------|-------|
    | 1217 (1%) | 35.44% | 37.01% | 45.07% | -1.58% | -9.63% | 11.91 |
    | 6089 (5%) | 55.58% | 55.42% | 57.74% | +0.16% | -2.17% | 10.97 |
    | 12178 (10%) | 61.99% | 61.48% | 63.23% | +0.51% | -1.24% | 7.97 |
    | 24356 (20%) | 67.31% | 66.83% | 68.15% | +0.48% | -0.85% | 8.39 |
    | 36534 (30%) | 69.85% | 69.51% | 70.15% | +0.34% | -0.30% | 7.00 |
    | 48713 (40%) | 71.14% | 70.86% | 71.19% | +0.28% | -0.05% | 7.00 |
    | 60891 (50%) | 72.49% | 72.92% | 72.66% | -0.43% | -0.17% | 7.00 |
    | 73069 (60%) | 74.52% | 74.62% | 74.51% | -0.10% | +0.02% | 2.08 |
    | 85248 (70%) | 75.31% | 75.29% | 75.23% | +0.02% | +0.07% | 2.00 |
    | 97426 (80%) | 75.49% | 75.49% | 75.46% | +0.01% | +0.03% | 2.00 |

=== RUN   TestCapacitySweep/CloudPhysics
    Trace: CloudPhysics, Accesses: 100000, Unique keys: 43731

    | Capacity | CloxCache | SimpleLRU | Otter | Clox vs LRU | Clox vs Otter | Avg K |
    |----------|-----------|-----------|-------|-------------|---------------|-------|
    | 437 (1%) | 13.23% | 14.76% | 15.95% | -1.53% | -2.72% | 2.02 |
    | 2186 (5%) | 16.40% | 16.14% | 17.85% | +0.25% | -1.45% | 2.12 |
    | 4373 (10%) | 17.92% | 17.63% | 22.52% | +0.28% | -4.61% | 2.05 |
    | 8746 (20%) | 23.47% | 22.89% | 29.84% | +0.58% | -6.37% | 2.34 |
    | 13119 (30%) | 33.78% | 33.45% | 36.02% | +0.33% | -2.24% | 2.00 |
    | 17492 (40%) | 36.85% | 36.09% | 37.66% | +0.75% | -0.81% | 2.00 |
    | 21865 (50%) | 37.84% | 36.21% | 37.75% | +1.63% | +0.09% | 2.00 |
    | 26238 (60%) | 38.68% | 38.50% | 37.83% | +0.17% | +0.84% | 2.00 |
    | 30611 (70%) | 40.14% | 39.93% | 37.90% | +0.21% | +2.24% | 2.00 |
    | 34984 (80%) | 42.95% | 42.99% | 37.95% | -0.04% | +5.00% | 2.00 |

=== RUN   TestCapacitySweep/Twitter52
    Trace: Twitter52, Accesses: 500000, Unique keys: 82416

    | Capacity | CloxCache | SimpleLRU | Otter | Clox vs LRU | Clox vs Otter | Avg K |
    |----------|-----------|-----------|-------|-------------|---------------|-------|
    | 824 (1%) | 60.04% | 63.27% | 67.49% | -3.23% | -7.45% | 8.23 |
    | 4120 (5%) | 72.52% | 72.36% | 75.52% | +0.16% | -3.00% | 7.17 |
    | 8241 (10%) | 76.24% | 76.04% | 78.54% | +0.20% | -2.30% | 5.09 |
    | 16483 (20%) | 79.75% | 79.70% | 80.61% | +0.04% | -0.86% | 7.00 |
    | 24724 (30%) | 81.27% | 81.16% | 81.55% | +0.11% | -0.28% | 6.67 |
    | 32966 (40%) | 82.15% | 82.14% | 82.14% | +0.01% | +0.01% | 2.00 |
    | 41208 (50%) | 82.68% | 82.67% | 82.57% | +0.01% | +0.11% | 2.00 |
    | 49449 (60%) | 83.02% | 83.04% | 82.92% | -0.02% | +0.10% | 2.00 |
    | 57691 (70%) | 83.24% | 83.25% | 83.17% | -0.01% | +0.07% | 2.00 |
    | 65932 (80%) | 83.39% | 83.40% | 83.36% | -0.01% | +0.03% | 2.00 |
```

</details>

### Concurrent Throughput

Where CloxCache really shines—lock-free reads scale linearly with goroutines:

| Goroutines | CloxCache    | SimpleLRU (mutex) | Otter       | CloxCache vs LRU |
|------------|--------------|-------------------|-------------|------------------|
| 1          | 13.3M ops/s  | 23.3M ops/s       | 9.5M ops/s  | 0.6x             |
| 2          | 24.4M ops/s  | 15.3M ops/s       | 11.3M ops/s | 1.6x             |
| 4          | 43.8M ops/s  | 12.9M ops/s       | 19.5M ops/s | 3.4x             |
| 8          | 77.7M ops/s  | 11.4M ops/s       | 22.4M ops/s | 6.8x             |
| 16         | 95.7M ops/s  | 8.5M ops/s        | 21.1M ops/s | 11.3x            |

*(90% reads, 10% writes workload)*

<details>
<summary>Raw benchmark output</summary>

```
=== RUN   TestConcurrentReadHeavy
    Read-heavy concurrent test: 90% reads, 10% writes, 1000000 ops total

    | Goroutines | CloxCache (ops/s) | SimpleLRU (ops/s) | Otter (ops/s) |
    |------------|-------------------|-------------------|---------------|
    | 1 | 13289867 | 23307248 | 9510357 |
    | 2 | 24360529 | 15284006 | 11329478 |
    | 4 | 43795533 | 12873824 | 19459776 |
    | 8 | 77670392 | 11407027 | 22433634 |
    | 16 | 95669977 | 8474024 | 21113742 |
```

</details>

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
| Very high (1-5% capacity) | 8-12      | Only protect very hot items |
| High (10-20% capacity)    | 5-8       | Selective protection        |
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
