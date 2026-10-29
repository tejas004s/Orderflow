# OrderFlow Benchmarks

## Hardware & Environment

- **CPU:** AMD Ryzen 5 5600H with Radeon Graphics (4 vCPUs allocated to Ubuntu 22.04 VM)
- **RAM:** 15 GiB
- **Go:** `go1.26.0 linux/amd64`

---

## 1. End-to-End Processor, Latency Distribution & Crash Recovery (`cmd/loadgen`)

Command:

```bash
go run ./cmd/loadgen -engine-only -orders 100000
```

| Metric | SDD Target | Measured (1 Partition, Single Goroutine) |
|---|---|---|
| **Pure Engine Throughput** | — | **276,000 cmds/sec** (`BenchmarkEngineBTree-4`: `3,623 ns/op`) |
| **Pure Match Latency** | p99 < 10 ms | **p50 = 2.74 µs · p95 = 9.25 µs · p99 = 25.00 µs (0.025 ms)** |
| **Full Processor Throughput** *(envelope decode + match + event encode + snapshots)* | 5,000+ orders/sec | **10,144 orders/sec** per partition (**60,000+ orders/sec** across 6 partitions) |
| **Full Processor Latency** | p99 < 10 ms | **p50 = 50.6 µs · p95 = 174.4 µs · p99 = 495.3 µs (0.495 ms)** |
| **Crash Recovery Time** *(restore snapshot + replay tail after simulated `kill -9`)* | < 10 s | **322.8 ms (0.32 s)** |

---

## 2. M1: Matching Engine Microbenchmarks (`internal/engine`)

Command:

```bash
go test -run=^$ -bench=. -benchmem ./internal/engine
```

Workload: 72% limit orders across 201 price ticks, 8% market orders, 20% cancels, across 8 users (`makeWorkload(65536)`). `*WithDepth` additionally takes a 20-level L2 book snapshot (`Book.Depth(20)`) every 50 commands.

| Benchmark | ns/op | Throughput (cmds/sec/core) | B/op | allocs/op |
|---|---:|---:|---:|---:|
| `BenchmarkEngineBTree-4` | 3,623 ns | ~276,000 / sec | 594 B | 10 |
| `BenchmarkEngineHeap-4` | 4,058 ns | ~246,400 / sec | 593 B | 10 |
| `BenchmarkEngineBTreeWithDepth-4` | 4,255 ns | ~235,000 / sec | 616 B | 10 |
| `BenchmarkEngineHeapWithDepth-4` | 4,608 ns | ~217,000 / sec | 631 B | 10 |

---

## 3. 1,000,000-Command Invariant Verification

```bash
go test -v ./internal/engine -run TestInvariants1M
```

- **Commands processed:** 1,000,000
- **Trades matched:** 575,598
- **Events emitted:** 2,151,196
- **Resting orders at completion:** 93,308
- **Elapsed time (including periodic full-book invariant walks + snapshot/restore round-trip):** 13.05 s
