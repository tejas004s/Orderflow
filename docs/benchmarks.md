# OrderFlow Benchmarks

## Hardware & Environment

- **CPU:** AMD Ryzen 5 5600H with Radeon Graphics (4 vCPUs allocated to Ubuntu 22.04 VM)
- **RAM:** 15 GiB
- **Go:** `go1.26.0 linux/amd64`

---

## M1: Matching Engine Microbenchmarks (`internal/engine`)

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

### 1,000,000-Command Invariant Verification

```bash
go test -v ./internal/engine -run TestInvariants1M
```

- **Commands processed:** 1,000,000
- **Trades matched:** 575,598
- **Events emitted:** 2,151,196
- **Resting orders at completion:** 93,308
- **Elapsed time (including periodic full-book invariant walks + snapshot/restore round-trip):** 13.05 s

