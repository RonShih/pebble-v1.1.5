#!/usr/bin/env python3
"""Analyze a CASTLE trace log: latency distribution for cache hit vs miss."""

import re
import sys
import statistics
import matplotlib.pyplot as plt
import matplotlib
matplotlib.use("Agg")  # non-interactive backend

# ── Parse ──────────────────────────────────────────────────────────────
def parse_trace(path):
    pattern = re.compile(
        r"OPType: Get,.*cache: (\S+), latency_us: (\d+)"
    )
    hit, miss = [], []
    for line in open(path):
        m = pattern.search(line)
        if not m:
            continue
        cache, lat = m.group(1), int(m.group(2))
        if cache == "hit":
            hit.append(lat)
        elif cache == "miss":
            miss.append(lat)
        # "n/a" (memtable) is skipped
    return hit, miss

# ── Stats ──────────────────────────────────────────────────────────────
def print_stats(label, data):
    if not data:
        print(f"  {label}: (no data)")
        return
    print(f"  {label}:")
    print(f"    count  = {len(data)}")
    print(f"    min    = {min(data)} us")
    print(f"    max    = {max(data)} us")
    print(f"    mean   = {statistics.mean(data):.1f} us")
    print(f"    median = {statistics.median(data):.1f} us")
    if len(data) >= 2:
        print(f"    stdev  = {statistics.stdev(data):.1f} us")
    p50 = sorted(data)[len(data) * 50 // 100]
    p90 = sorted(data)[len(data) * 90 // 100]
    p99 = sorted(data)[min(len(data) * 99 // 100, len(data) - 1)]
    print(f"    p50    = {p50} us")
    print(f"    p90    = {p90} us")
    print(f"    p99    = {p99} us")

# ── Plot ───────────────────────────────────────────────────────────────
def plot(hit, miss, out_path):
    fig, axes = plt.subplots(1, 3, figsize=(18, 5))

    # 1) Histogram: hit vs miss side by side
    ax = axes[0]
    if miss:
        ax.hist(miss, bins=max(5, len(set(miss))), alpha=0.7, label=f"miss (n={len(miss)})", color="#e74c3c")
    if hit:
        ax.hist(hit, bins=max(10, min(50, len(set(hit)))), alpha=0.7, label=f"hit (n={len(hit)})", color="#2ecc71")
    ax.set_xlabel("Latency (μs)")
    ax.set_ylabel("Count")
    ax.set_title("Latency Distribution: Hit vs Miss")
    ax.legend()

    # 2) Box plot
    ax = axes[1]
    box_data, box_labels = [], []
    if miss:
        box_data.append(miss)
        box_labels.append(f"miss\n(n={len(miss)})")
    if hit:
        box_data.append(hit)
        box_labels.append(f"hit\n(n={len(hit)})")
    bp = ax.boxplot(box_data, labels=box_labels, patch_artist=True, showfliers=True)
    colors = ["#e74c3c", "#2ecc71"][:len(box_data)]
    for patch, color in zip(bp["boxes"], colors):
        patch.set_facecolor(color)
        patch.set_alpha(0.7)
    ax.set_ylabel("Latency (μs)")
    ax.set_title("Latency Box Plot")

    # 3) Time series — all Gets in order, colored by cache status
    ax = axes[2]
    all_gets = []
    for v in hit:
        all_gets.append(("hit", v))
    for v in miss:
        all_gets.append(("miss", v))
    # Re-parse to preserve original order
    all_gets_ordered = []
    pattern = re.compile(r"OPType: Get,.*cache: (\S+), latency_us: (\d+)")
    for line in open(trace_path):
        m = pattern.search(line)
        if not m:
            continue
        cache, lat = m.group(1), int(m.group(2))
        if cache in ("hit", "miss"):
            all_gets_ordered.append((cache, lat))

    hit_x = [i for i, (c, _) in enumerate(all_gets_ordered) if c == "hit"]
    hit_y = [lat for c, lat in all_gets_ordered if c == "hit"]
    miss_x = [i for i, (c, _) in enumerate(all_gets_ordered) if c == "miss"]
    miss_y = [lat for c, lat in all_gets_ordered if c == "miss"]

    ax.scatter(hit_x, hit_y, s=8, alpha=0.6, color="#2ecc71", label="hit")
    ax.scatter(miss_x, miss_y, s=30, alpha=0.9, color="#e74c3c", marker="x", label="miss")
    ax.set_xlabel("Get Operation #")
    ax.set_ylabel("Latency (μs)")
    ax.set_title("Latency per Get (time order)")
    ax.legend()

    plt.tight_layout()
    plt.savefig(out_path, dpi=150)
    print(f"\n  Plot saved to: {out_path}")

# ── Main ───────────────────────────────────────────────────────────────
if __name__ == "__main__":
    if len(sys.argv) < 2:
        print(f"Usage: {sys.argv[0]} <trace-file>")
        sys.exit(1)

    trace_path = sys.argv[1]
    hit, miss = parse_trace(trace_path)

    print("=" * 60)
    print("CASTLE Trace Analysis — Cache Hit vs Miss Latency")
    print("=" * 60)
    print(f"  File: {trace_path}")
    print(f"  Total Gets (SSTable): {len(hit) + len(miss)}")
    print()
    print_stats("Cache HIT", hit)
    print()
    print_stats("Cache MISS", miss)
    print()

    if hit and miss:
        speedup = statistics.mean(miss) / statistics.mean(hit)
        print(f"  Speedup (mean miss / mean hit): {speedup:.1f}x")
    print("=" * 60)

    out_png = trace_path + "_analysis.png"
    plot(hit, miss, out_png)
