# CloxCache Configuration Guide

CloxCache provides automatic hardware detection and dynamic configuration to optimize memory usage for your system.

## Configuration Options

### 1. Automatic Hardware Detection (Default)

By default, CloxCache automatically detects your system's hardware and configures itself:

```caddyfile
:443 {
    atlas {
          # ... other options ...
          # No max_cache_size specified - auto-detect hardware
          }
}
```

**Behavior:**

- Detects CPU count and system memory
- Allocates **10% of total RAM** for cache (bounded: 256MB - 16GB)
- Calculates optimal shard count: **4-8 shards per CPU core**
- Ensures power-of-2 shard and slot counts for efficient bit-masking

**Example auto-configuration:**

- **16 CPU system with 8GB RAM**:
    - Cache size: ~819MB
    - Shards: 64 (4 per core)
    - Slots per shard: 65,536
    - Total capacity: ~4M records

### 2. Manual Cache Size Configuration

Specify a custom cache size with the `max_cache_size` directive:

```caddyfile
:443 {
    atlas {
          # ... other options ...
              max_cache_size 2GB
          }
}
```

**Supported formats:**

- Bytes: `536870912` or `512MB` or `1GB`
- Caddy's `ParseSize()` supports: `KB`, `MB`, `GB`, `TB`

**Example configurations:**

| `max_cache_size` | Shards | Slots/Shard | Total Slots | Estimated Memory |
|------------------|--------|-------------|-------------|------------------|
| `256MB`          | 64     | 16,384      | 1,048,576   | ~128MB           |
| `512MB`          | 64     | 32,768      | 2,097,152   | ~256MB           |
| `1GB`            | 64     | 65,536      | 4,194,304   | ~512MB           |
| `2GB`            | 64     | 65,536      | 4,194,304   | ~512MB           |
| `4GB`            | 64     | 65,536      | 4,194,304   | ~512MB           |

**Note:** The estimated memory is lower than the target because it only accounts for cache structures (nodes, slots).
Actual record data is stored separately.

## Complete Caddyfile Examples

### Development Environment (Small Cache)

```caddyfile
:443 {
    atlas {
              credentials "dev-api-key"
              db_path "./data/"
              socket "./atlas.sock"
              development_mode true
              max_cache_size 256MB
          }
}
```

### Production Environment (Auto-detect)

```caddyfile
:443 {
    atlas {
              credentials {$ATLAS_API_KEY}
              db_path "/var/lib/atlas/"
              socket "/var/run/atlas.sock"
              region "us-west-2"
              advertise "db-node-1.example.com:443"

          # Auto-detect hardware and configure cache
          # No max_cache_size specified
          }
}
```

### Production Environment (Large Server)

```caddyfile
:443 {
    atlas {
              credentials {$ATLAS_API_KEY}
              db_path "/var/lib/atlas/"
              socket "/var/run/atlas.sock"
              region "us-west-2"
              advertise "db-node-1.example.com:443"
              max_cache_size 8GB
          }
}
```

### Multi-Region Cluster

```caddyfile
:443 {
    atlas {
              credentials {$ATLAS_API_KEY}
              connect "https://bootstrap-node.example.com"
              db_path "/var/lib/atlas/"
              region "eu-central-1"
              advertise "db-node-eu-1.example.com:443"
              max_cache_size 4GB
          }
}
```

## Configuration Algorithm

### Shard Calculation

```
numShards = nextPowerOf2(numCPU * 4)
numShards = clamp(numShards, 16, 256)
```

**Rationale:**

- 4-8 shards per core provides good parallelism
- Power of 2 enables efficient bit-masking for routing
- Bounds (16-256) prevent extreme configurations

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
- Target load factor: 1.0 (avg chain length ~1)

### Memory Estimation

```
slotArrayMemory = totalSlots × 8 bytes
nodeMemory = (totalSlots × 1.25) × 96 bytes
shardOverhead = numShards × 64 bytes
totalMemory = slotArrayMemory + nodeMemory + shardOverhead
```

## Monitoring Cache Performance

### Startup Logs

When CloxCache initializes, it logs the configuration:

**Auto-configured:**

```
📊 CloxCache auto-configured from hardware
  cpu_count=16
  total_memory=8.0 GB
  cache_size=819.2 MB
  shards=64
  slots_per_shard=65536
```

**Manually configured:**

```
📊 CloxCache configured from max_cache_size
  size=2.0 GB
  shards=64
  slots_per_shard=65536
```

### Runtime Metrics

Access cache statistics programmatically (requires `CollectStats: true`):

```go
hits, misses, evictions := stateMachine.Stats()
hitRate := float64(hits) / float64(hits + misses)
```

**Note:** Statistics are only collected when `CollectStats` or `AdaptiveDecay` is enabled in the cache configuration.
When disabled, `Stats()` returns zeros.

## Tuning Guidelines

### When to Use Auto-configuration

✅ **Good for:**

- Development environments
- Standard production servers (4-32 cores, 8-64GB RAM)
- When you want reasonable defaults without tuning

### When to Manually Configure

✅ **Good for:**

- Dedicated database servers with predictable workloads
- Large servers (64+ cores, 128GB+ RAM) where 10% may be too much
- Memory-constrained environments where you need precise control
- Shared servers where you want to limit atlas-db's memory footprint

### Sizing Recommendations

| Server Type         | CPUs  | RAM      | Recommended `max_cache_size` |
|---------------------|-------|----------|------------------------------|
| Development         | 2-4   | 4-8GB    | `256MB`                      |
| Small Production    | 4-8   | 8-16GB   | `512MB` - `1GB`              |
| Medium Production   | 8-16  | 16-32GB  | `1GB` - `2GB`                |
| Large Production    | 16-32 | 32-64GB  | `2GB` - `4GB`                |
| Dedicated DB Server | 32-64 | 64-256GB | `4GB` - `16GB`               |

### Performance Considerations

**Shard Count:**

- More shards = better parallelism on multi-core systems
- Too many shards = overhead from shard structures
- Sweet spot: **4-8 shards per CPU core**

**Load Factor:**

- Target: **1.0-1.25** (avg 1 node per slot)
- Lower load factor = faster lookups, more memory
- Higher load factor = slower lookups, less memory
- CloxCache uses chaining, so load factor > 1.0 is acceptable

**Memory vs. Hit Rate:**

- Larger cache = higher hit rate = fewer log reconstructions
- Balance cache size with available RAM
- Monitor eviction rate - high evictions may indicate cache is too small

## Troubleshooting

### Cache Too Small (High Eviction Rate)

**Symptom:** Frequent "cache admission rejected" warnings in logs

**Solution:**

```caddyfile
max_cache_size 2GB  # Increase from current size
```

### Memory Pressure

**Symptom:** System running out of memory, OOM kills

**Solution:**

```caddyfile
max_cache_size 512MB  # Reduce cache size
```

### Poor Cache Performance

**Symptom:** High miss rate despite adequate memory

**Possible causes:**

- Working set larger than cache size → increase `max_cache_size`
- Zipf distribution with many cold keys → cache is working as designed
- Scans overwhelming cache → implement admission gate tuning (future)

## Performance Options

CloxCache provides two optional features that can be enabled for observability or adaptive behavior at the cost of some
performance overhead.

### CollectStats

When enabled, tracks hit/miss/eviction counters.

```go
cfg := cache.Config{
NumShards:     64,
SlotsPerShard: 4096,
CollectStats:  true, // Enable statistics collection
}
```

**Trade-off:** Adds atomic counter increments on every Get operation. Disable for maximum throughput.

### AdaptiveDecay

When enabled, dynamically adjusts the eviction decay rate based on hit rate and admission pressure. Automatically
enables `CollectStats`.

```go
cfg := cache.Config{
NumShards:     64,
SlotsPerShard: 4096,
AdaptiveDecay: true, // Enable adaptive decay (implies CollectStats)
}
```

**Behavior:**

- Monitors hit rate and admission rejection pressure
- Adjusts decay step (1-4) every second
- Higher pressure + lower hit rate → more aggressive eviction

**Trade-off:** Adds a background goroutine and atomic counter overhead. Use when workload patterns vary significantly.

### Performance Comparison

| Configuration         | Get Latency | Use Case             |
|-----------------------|-------------|----------------------|
| Default (both false)  | ~26ns       | Maximum throughput   |
| `CollectStats: true`  | ~52ns       | Observability needed |
| `AdaptiveDecay: true` | ~52ns       | Variable workloads   |

## Advanced: Programmatic Configuration

For custom integrations, configure CloxCache directly in Go:

```go
import "github.com/bottledcode/atlas-db/atlas/cache"

// Auto-detect hardware
cfg := cache.ConfigFromHardware()

// Or specify size
cfg := cache.ConfigFromMemorySize(2 * 1024 * 1024 * 1024) // 2GB

// Enable optional features
cfg.CollectStats = true   // Enable hit/miss/eviction tracking
cfg.AdaptiveDecay = true  // Enable adaptive decay tuning

// Create cache with key type ([]byte or string) and value type
myCache := cache.NewCloxCache[[]byte, *MyType](cfg)

// Get statistics (only meaningful when CollectStats is enabled)
hits, misses, evictions := myCache.Stats()
hitRate := float64(hits) / float64(hits + misses)

// Get configuration info
hw := cache.DetectHardware()
fmt.Printf("CPUs: %d, RAM: %s, Cache: %s\n",
hw.NumCPU,
cache.FormatMemory(hw.TotalMemory),
cache.FormatMemory(hw.CacheSize))

// Estimate memory for a config
estimated := cfg.EstimateMemoryUsage()
fmt.Printf("Estimated memory: %s\n", cache.FormatMemory(estimated))
```

## References

- [CloxCache Design Document](../../kv-cache.md)
- [Caddy Configuration](https://caddyserver.com/docs/caddyfile)
- [Atlas-DB Options](../options/options.go)
