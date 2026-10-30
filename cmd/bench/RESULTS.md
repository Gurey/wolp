# Benchmark results

This compared a Go and a Rust version of the proxy. Go won and the Rust
version has since been removed; these numbers are kept as the record of why.

Run on 2026-10-30 on `server.local` (Ryzen 7 1800X, Ubuntu), all over loopback.
Each proxy was pinned to one physical core (`taskset -c 2`, sibling idle); the
`bench` client and backends ran on other cores. Throughput tests move 8 GiB.

| Test | Direct (baseline) | Go | Rust, 8 KiB buffers | Rust, 256 KiB buffers |
|---|---|---|---|---|
| Upload, 1 stream | 23.4 Gbit/s | 20.6 Gbit/s (41% core) | 8.1 Gbit/s (88%) | 14.7 Gbit/s (82%) |
| Download, 1 stream | 19.5 Gbit/s | 21.1 Gbit/s (42%) | 8.1 Gbit/s (88%) | 15.8 Gbit/s (100%) |
| Upload, 8 streams | 59.2 Gbit/s | 52.8 Gbit/s (82%) | 10.2 Gbit/s (100%) | 15.6 Gbit/s (100%) |
| Download, 8 streams | 69.8 Gbit/s | 52.0 Gbit/s (83%) | 10.4 Gbit/s (100%) | 15.6 Gbit/s (100%) |
| CPU per GiB moved (1 stream) | — | 0.17 s | 0.94 s | 0.48 s |
| RTT, persistent conn (p50 / p99) | 24 / 34 µs | 68 / 85 µs | 64 / 93 µs | 71 / 86 µs |
| New conn + 1 RTT (p50 / p99) | 91 / 130 µs | 256 / 310 µs | 248 / 292 µs | 255 / 292 µs |
| Peak RSS | — | 7.7 MB | 1.1 MB | 3.0 MB |

Go's `io.Copy` between two `TCPConn`s uses Linux `splice`, so data stays in the
kernel. tokio's `copy_bidirectional` copies through user-space buffers; bigger
buffers help, but it's still CPU-bound well below Go.

## Rust with tokio-splice

Same setup, run back to back with a fresh Go run for comparison. The Rust
proxy now uses `tokio_splice::zero_copy_bidirectional` on Linux (and the
256 KiB `copy_bidirectional_with_sizes` elsewhere).

| Test | Go | Rust, tokio-splice |
|---|---|---|
| Upload, 1 stream | 21.3 Gbit/s (39% core) | 20.4 Gbit/s (51%) |
| Download, 1 stream | 19.8 Gbit/s (39%) | 21.9 Gbit/s (55%) |
| Upload, 8 streams | 53.6 Gbit/s (84%) | 39.3 Gbit/s (99%) |
| Download, 8 streams | 59.1 Gbit/s (97%) | 43.3 Gbit/s (99%) |
| CPU per GiB moved (1 stream) | 0.16 s | 0.22 s |
| RTT, persistent conn (p50 / p99) | 65 / 76 µs | 67 / 83 µs |
| New conn + 1 RTT (p50 / p99) | 261 / 307 µs | 244 / 292 µs |
| Peak RSS | 7.7 MB | 0.95 MB |

Reproduce: cross-compile `bench`, both proxies and `bench.toml` (three ports,
18001-18003 -> 19001-19003 on 127.0.0.1) to the server, then

    taskset -c 12-15 ./bench serve &
    taskset -c 8-11 ./bench run                                   # baseline
    taskset -c 2 ./wol-proxy-go -config bench.toml & 
    taskset -c 8-11 ./bench run -ports 18001,18002,18003 -pid $!
