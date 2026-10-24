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
# A lab scenario value, warm enough that the training job throttles under the thermal model below.
SUPPLY_RISE = 27.0
PRIMARY_FLOW_LOSS = 0.55
VALVE_OPEN = 94.0
# An InfiniBand leaf-uplink fault slows the synchronous training job to this share of its step rate.
FAULT_START, FAULT_SECONDS, FAULT_STEP_RATE = 60.0, 180.0, 0.5
AFFECTED = ["rack02", "rack03", "rack04", "rack05", "rack06"]
CDU = "/C-F00-CDU-05/"
CDU_SUPPLY = CDU + "secondary/supply/temperature/"
# Each rack holds 10 GB200 compute trays of 4 GPUs. A GPU draws 145 W idle plus 705 W at full
# utilisation and clock. A tray adds 450 W for its Grace CPUs, memory, NICs and fans, and its
# hot-swap controllers measure the busbar input ahead of a 95% power conversion (a lab value).
TRAYS_PER_RACK, GPUS_PER_TRAY = 10, 4
GPUS_PER_RACK = TRAYS_PER_RACK * GPUS_PER_TRAY
IDLE_W, DYNAMIC_W, TRAY_OVERHEAD_W, TRAY_EFFICIENCY = 145.0, 705.0, 450.0, 0.95
# Each rack's GPUs run at evenly spread utilisation levels from 90% to 100%.
UTILIZATION = [0.9 + 0.1 * (k + 0.5) / GPUS_PER_RACK for k in range(GPUS_PER_RACK)]
# GPU thermals: a real B200's nvidia-smi T.Limit (88 C max operating), and a die rise over the
# coolant of C_PER_W per watt of board power. C_PER_W is an upper bound: 88 C at the 1000 W limit on
# the 45 C coolant NVIDIA rates GB200 NVL72 for. SW thermal slowdown lowers the clock just enough to
# hold 88 C, no lower than a real B200's 120 MHz idle SM clock out of GB200's 2062 MHz.
MAX_OPERATING_C = 88.0
C_PER_W = (MAX_OPERATING_C - 45.0) / 1000.0
MIN_CLOCK = 120.0 / 2062.0
COOLANT_SUPPLY_C = 31.4
# Water heat balance: specific heat in kJ/(kg K) at 1 kg/L, kW per ton of refrigeration, and the
# 24/dT gpm per ton a 10 F chilled-water rise gives.
WATER_CP = 4.186
KW_PER_TON = 3.517
CHW_GPM_PER_TON = 24.0 / 10.0
LITRES_PER_GALLON = 3.78541
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


def fault_pace(t: float) -> float:
    """Return the share of its step rate the training job runs at t seconds into the pass."""
    return FAULT_STEP_RATE if FAULT_START <= t < FAULT_START + FAULT_SECONDS else 1.0


def held_clock(load: float, supply_c: float) -> float:
    """Return the clock share that holds one GPU at its max operating temperature, 1 when it runs cooler."""
    budget_w = (MAX_OPERATING_C - supply_c) / C_PER_W - IDLE_W
    dynamic_w = DYNAMIC_W * load
    return 1.0 if budget_w >= dynamic_w else max(budget_w / dynamic_w, MIN_CLOCK)


def trays_kw(pace: float, job_clock: float) -> float:
    """Return one rack's compute-tray input power in kW, every GPU stepping at the job's pace and clock."""
    gpu_w = IDLE_W + DYNAMIC_W * mean(UTILIZATION) * pace * job_clock
    return TRAYS_PER_RACK * (GPUS_PER_TRAY * gpu_w + TRAY_OVERHEAD_W) / TRAY_EFFICIENCY / 1000


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


def capture_means(values: list[list[str]]) -> dict[str, float]:
    """Return the mean reading of every value point in the capture, keyed by topic."""
    samples: dict[str, list[float]] = {}
    for _, topic, payload in values:
        if VALUE.search(payload):
            samples.setdefault(topic, []).append(reading(payload))
    return {topic: mean(v) for topic, v in samples.items()}


def rack_point(means: dict[str, float], rack: str, kind: str) -> float:
    return next(v for t, v in means.items() if f"/Rack/{kind}/" in t and f"/{rack}/" in t)


def cdu_point(means: dict[str, float], path: str) -> float:
    return next(v for t, v in means.items() if CDU + path in t)


def make_adjuster(values: list[list[str]]):
    means = capture_means(values)
    capture_kw = {r: rack_point(means, r, "RackPower") for r in AFFECTED}
    other_kw = rack_point(means, "rack01", "RackPower")
    rack_flow = {r: rack_point(means, r, "RackLiquidFlow") for r in AFFECTED}
    rack_supply = {r: rack_point(means, r, "RackLiquidSupplyTemperature") for r in AFFECTED}
    rack_return = {r: rack_point(means, r, "RackLiquidReturnTemperature") for r in AFFECTED}
    primary_inlet = cdu_point(means, "primary/inlet/temperature/")
    secondary_supply = cdu_point(means, "secondary/supply/temperature/")
    inlet_kpa = cdu_point(means, "primary/inlet/pressure/")
    outlet_kpa = cdu_point(means, "primary/outlet/pressure/")

    def job_clock(t: float) -> float:
        # The job steps at the clock that holds its hottest GPU, the busiest one on the warmest rack supply.
        load = max(UTILIZATION) * fault_pace(t)
        return min(held_clock(load, rack_supply[r] + SUPPLY_RISE * intensity(t)) for r in AFFECTED)

    def rack_trays_kw(t: float) -> float:
        return trays_kw(fault_pace(t), job_clock(t))

    # A rack keeps any load the capture shows beyond its trays, such as switch trays.
    remainder_kw = {r: max(0.0, capture_kw[r] - rack_trays_kw(0.0)) for r in AFFECTED}

    def rack_kw(rack: str, t: float) -> float:
        return rack_trays_kw(t) + remainder_kw[rack]

    def liquid_kw(rack: str, t: float) -> float:
        # The trays' conversion loss leaves as air; their delivered load and the other load go to the liquid.
        return rack_trays_kw(t) * TRAY_EFFICIENCY + remainder_kw[rack]

    def heat_kw(t: float) -> float:
        # CDU-05 carries the liquid heat of the row E racks it serves; row F has no telemetry and counts as unloaded.
        return sum(liquid_kw(r, t) for r in AFFECTED)

    def power_ratio(rack: str | None, t: float) -> float:
        if rack:
            return rack_kw(rack, t) / capture_kw[rack]
        return (sum(rack_kw(r, t) for r in AFFECTED) + other_kw) / (sum(capture_kw.values()) + other_kw)

    # The facility water is sized for the racks' liquid heat at a 10 F rise.
    design_lpm = heat_kw(0.0) / KW_PER_TON * CHW_GPM_PER_TON * LITRES_PER_GALLON

    def primary_flow(t: float) -> float:
        return design_lpm * (1 - PRIMARY_FLOW_LOSS * intensity(t))

    def primary_outlet(t: float) -> float:
        return primary_inlet + heat_kw(t) / (primary_flow(t) / 60 * WATER_CP)

    def primary_inlet_kpa(t: float) -> float:
        # The facility supply loses pressure; the drop across the heat exchanger follows its square law.
        return outlet_kpa + (inlet_kpa - outlet_kpa) * (primary_flow(t) / design_lpm) ** 2

    def rack_return_at(rack: str, t: float) -> float:
        # The rack's supply plus its liquid heat over its flow
        supply = rack_supply[rack] + SUPPLY_RISE * intensity(t)
        return supply + liquid_kw(rack, t) / (rack_flow[rack] / 60 * WATER_CP)

    def secondary_return(t: float) -> float:
        # The racks' liquid heat over the secondary flow, so both sides of the CDU carry the same heat.
        supply = secondary_supply + SUPPLY_RISE * intensity(t)
        return supply + heat_kw(t) / (sum(rack_flow.values()) / 60 * WATER_CP)

    def adjust(topic: str, t: float):
        i = intensity(t)
        if CDU in topic and "/LiquidTemperature/" in topic:
            # Readings keep their capture noise around the heat-balance value.
            if "/primary/outlet/temperature/" in topic:
                return lambda v: primary_outlet(t) + v - means[topic]
            if "/secondary/return/temperature/" in topic:
                return lambda v: secondary_return(t) + v - means[topic]
            return (lambda v: v + SUPPLY_RISE * i) if CDU_SUPPLY in topic and i > 0 else None
        if CDU in topic and "/CDU/LiquidFlow/" in topic and "/primary/flow/" in topic:
            return lambda v: primary_flow(t)
        if CDU in topic and "/CDU/LiquidFlow/" in topic and "/secondary/flow/" in topic:
            return lambda v: sum(rack_flow.values())
        if CDU in topic and "/CDU/LiquidPressure/" in topic and "/primary/inlet/pressure/" in topic:
            return lambda v: primary_inlet_kpa(t) + v - means[topic]
        parts = topic.split("/")
        rack = rack_of(topic)
        power_like = (
            parts[4] == "PowerMeter" and parts[5] in ("ActivePower", "ApparentPower", "Current", "PhaseCurrent")
        ) or (parts[4] == "Rack" and parts[5] == "RackPower")
        if rack and parts[5] == "RackLiquidReturnTemperature":
            return lambda v: v + rack_return_at(rack, t) - rack_return[rack]
        if (rack or "/C-E-BUS-" in topic) and power_like:
            return lambda v: v * power_ratio(rack, t)
        if i == 0:
            return None
        if CDU in topic and "/CDU/ValvePosition/" in topic and "/primary/valve/" in topic:
            # The valves open fully as the controller tries to recover the lost flow.
            return lambda v: v + VALVE_OPEN * i
        if rack and parts[5] == "RackLiquidSupplyTemperature":
            return lambda v: v + SUPPLY_RISE * i
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
            fn = adjust(topic, ns / 1e9)
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
        payload = rewrite(supply[2], lambda v: COOLANT_SUPPLY_C + SUPPLY_RISE * intensity(sec))
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
