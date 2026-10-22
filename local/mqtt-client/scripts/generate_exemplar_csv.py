#!/usr/bin/env python3
# Copyright 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Generate examples/dsx_exemplar.csv from the recorded 99-second BMS capture.

The output is one 900-second pass that repeats the capture and adds a cooling
excursion on CDU C-F00-CDU-05 and the racks E02-E06 that it serves.
"""

from __future__ import annotations

import argparse
import csv
import io
import re
from pathlib import Path
from statistics import mean

PASS_NS = 900 * 10**9
CAPTURE_NS = 99_367_544_100
START, RAMP, HOLD, RECOVERY = 300.0, 120.0, 300.0, 180.0
SUPPLY_RISE = 15.0
PRIMARY_FLOW_LOSS = 0.55
VALVE_OPEN = 94.0
AFFECTED = ["rack02", "rack03", "rack04", "rack05", "rack06"]
GPUS_PER_RACK = 40
CDU = "/C-F00-CDU-05/"
CDU_SUPPLY = CDU + "secondary/supply/temperature/"
UTILIZATION = [0.9 + 0.1 * (i + 0.5) / 2000 for i in range(2000)]
UNITS = {"h": 3600 * 10**9, "m": 60 * 10**9, "s": 10**9, "ms": 10**6, "us": 10**3, "µs": 10**3, "ns": 1}
VALUE = re.compile(r'("value":)(-?[\d.]+)')

DEFAULT_OUTPUT = Path(__file__).resolve().parent.parent / "examples" / "dsx_exemplar.csv"


def intensity(t: float) -> float:
    """Return the excursion strength from 0 to 1 at t seconds into the pass."""
    t -= START
    if t < 0 or t >= RAMP + HOLD + RECOVERY:
        return 0.0
    if t < RAMP:
        return t / RAMP
    if t < RAMP + HOLD:
        return 1.0
    return 1.0 - (t - RAMP - HOLD) / RECOVERY


def gpu_power(utilization: float, rise: float) -> float:
    """Return one GPU's power in watts after its clock throttles at 85 C and 90 C."""
    temp = round(31.4 + 45.0 * utilization + rise)
    clock = 0.45 if temp >= 90 else 0.8 if temp >= 85 else 1.0
    return 145.0 + 705.0 * utilization * clock


BASE_GPU_W = mean(gpu_power(u, 0) for u in UTILIZATION)


def gpu_drop_kw(i: float) -> float:
    """Return the power in kW that one rack's GPUs shed at excursion intensity i."""
    return GPUS_PER_RACK * (BASE_GPU_W - mean(gpu_power(u, SUPPLY_RISE * i) for u in UTILIZATION)) / 1000


def parse_ns(s: str) -> int:
    total = 0
    for num, unit in re.findall(r"([\d.]+)(ms|us|µs|ns|h|m|s)", s):
        whole, _, frac = num.partition(".")
        scale = UNITS[unit]
        total += int(whole) * scale + (int(frac) * scale // 10 ** len(frac) if frac else 0)
    return total


def format_ns(ns: int) -> str:
    """Format ns as Go's time.Duration.String() does for durations of 1 s or more."""
    minutes, rem = divmod(ns, 60 * 10**9)
    secs, frac = divmod(rem, 10**9)
    s = f"{secs}" + (f".{frac:09d}".rstrip("0") if frac else "") + "s"
    return f"{minutes}m{s}" if minutes else s


def reading(payload: str) -> float:
    return float(VALUE.search(payload).group(2))


def rewrite(payload: str, fn) -> str:
    """Replace the payload value and keep the number of decimal places that the capture uses."""
    m = VALUE.search(payload)
    old = m.group(2)
    decimals = len(old.partition(".")[2])
    new = f"{fn(float(old)):.{max(decimals, 1) if '.' in old else 0}f}"
    return payload[: m.start(2)] + new + payload[m.end(2) :]


def rack_of(topic: str) -> str | None:
    return next((p for p in topic.split("/") if p in AFFECTED), None)


def rack_baselines(values: list[list[str]]) -> tuple[dict[str, float], dict[str, float]]:
    """Return the mean rack power and coolant delta-T of each affected rack in the capture."""
    samples: dict[tuple[str, str], list[float]] = {}
    for _, topic, payload in values:
        rack = rack_of(topic)
        if not rack:
            continue
        for kind in ("RackLiquidSupplyTemperature", "RackLiquidReturnTemperature", "RackPower"):
            if f"/{kind}/" in topic:
                samples.setdefault((rack, kind), []).append(reading(payload))
    power = {r: mean(samples[(r, "RackPower")]) for r in AFFECTED}
    delta_t = {
        r: mean(samples[(r, "RackLiquidReturnTemperature")]) - mean(samples[(r, "RackLiquidSupplyTemperature")])
        for r in AFFECTED
    }
    return power, delta_t


def make_adjuster(values: list[list[str]]):
    rack_kw, delta_t = rack_baselines(values)

    def power_ratio(rack: str | None, i: float) -> float:
        # Each rack, tap, and busway keeps its non-GPU load and loses only what its GPUs shed.
        racks = [rack] if rack else AFFECTED
        return 1 - gpu_drop_kw(i) * len(racks) / sum(rack_kw[r] for r in racks)

    def adjust(topic: str, i: float):
        if CDU in topic and "/LiquidTemperature/" in topic:
            # The capture's CDU secondary return is only 0.3 C above supply, so it follows supply.
            moved = (CDU_SUPPLY, "/secondary/return/temperature/", "/primary/outlet/temperature/")
            return (lambda v: v + SUPPLY_RISE * i) if any(p in topic for p in moved) else None
        if CDU in topic and "/CDU/LiquidFlow/" in topic and "/primary/flow/" in topic:
            return lambda v: v * (1 - PRIMARY_FLOW_LOSS * i)
        if CDU in topic and "/CDU/ValvePosition/" in topic and "/primary/valve/" in topic:
            return lambda v: v + VALVE_OPEN * i
        parts = topic.split("/")
        rack = rack_of(topic)
        power_like = (
            parts[4] == "PowerMeter" and parts[5] in ("ActivePower", "ApparentPower", "Current", "PhaseCurrent")
        ) or (parts[4] == "Rack" and parts[5] == "RackPower")
        if rack and parts[5] == "RackLiquidSupplyTemperature":
            return lambda v: v + SUPPLY_RISE * i
        if rack and parts[5] == "RackLiquidReturnTemperature":
            return lambda v: v + SUPPLY_RISE * i + delta_t[rack] * (power_ratio(rack, i) - 1)
        if (rack or "/C-E-BUS-" in topic) and power_like:
            return lambda v: v * power_ratio(rack, i)
        return None

    return adjust


def generate(rows: list[list[str]]) -> list[list[str]]:
    meta = [r for r in rows if "/Metadata/" in r[1]]
    values = [r for r in rows if "/Metadata/" not in r[1]]
    if rows[: len(meta)] != meta:
        raise SystemExit("input must list every Metadata row before the first Value row")
    adjust = make_adjuster(values)

    out = list(meta)
    pass_index = 0
    while True:
        added = False
        for offset, topic, payload in values:
            ns = parse_ns(offset) + pass_index * CAPTURE_NS
            if ns > PASS_NS:
                continue
            added = True
            i = intensity(ns / 1e9)
            fn = adjust(topic, i) if i > 0 else None
            if fn:
                payload = rewrite(payload, fn)
            out.append([offset if pass_index == 0 else format_ns(ns), topic, payload])
        if not added:
            break
        pass_index += 1
    # dummy-bms takes the pass length from the last offset, so the pass ends at exactly 900 s.
    out[-1][0] = format_ns(PASS_NS)
    # Add CDU supply readings at each phase change, as a change-of-value BMS sends them.
    supply = next(r for r in values if CDU_SUPPLY in r[1])
    for sec in (START, START + RAMP, START + RAMP + HOLD):
        payload = rewrite(supply[2], lambda v: 31.4 + SUPPLY_RISE * intensity(sec))
        ns = int(sec * 10**9)
        at = next(k for k, row in enumerate(out) if k >= len(meta) and parse_ns(row[0]) > ns)
        out.insert(at, [format_ns(ns), supply[1], payload])
    return out


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("capture", type=Path, help="recorded 99-second capture CSV")
    parser.add_argument("output", type=Path, nargs="?", default=DEFAULT_OUTPUT, help="output CSV path")
    args = parser.parse_args()

    with args.capture.open(newline="", encoding="utf-8") as f:
        header, *rows = list(csv.reader(f))
    if CAPTURE_NS != max(parse_ns(r[0]) for r in rows):
        raise SystemExit(f"{args.capture} is not the 99-second capture; its last offset differs")
    buf = io.StringIO()
    writer = csv.writer(buf, lineterminator="\n")
    writer.writerow(header)
    writer.writerows(generate(rows))
    with args.output.open("w", encoding="utf-8", newline="") as output:
        output.write(buf.getvalue())


if __name__ == "__main__":
    main()
