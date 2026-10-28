# MQTT Test Client

MQTT performance and functional testing framework for evaluating event bus solutions.

The manual subscribe examples require `mosquitto_sub`.

## Features

- **Performance Tests**: Comprehensive throughput testing (QoS 0/1, retained, federation)
- **Functional Tests**: MQTT protocol compliance, HA, federation validation
- **Metrics**: Prometheus metrics for monitoring
- **Reusable Components**: Client and configuration packages

## Testing

```bash
# Run unit tests
go test ./pkg/...

# Run functional tests
go test ./tests/functional/...

# Run the full local e2e suite from local/
make -C .. test

# Run all tests with coverage
go test -cover ./...

# Skip performance tests in short mode
go test -short ./...
```

## Performance Tests

### Test Matrix

Performance tests exercise throughput under different conditions:

**QoS and Retained Combinations:**

- **QoS 0**: No persistence
- **QoS 0 + Retained**: Retained messages without persistence guarantee
- **QoS 1**: With persistence
- **QoS 1 + Retained**: Retained messages with persistence guarantee

**Deployment Scenarios:**

- **Local**: Publishers and subscribers on same cluster
- **Federation**: Publishers on one cluster, subscribers on another

**12 Test Combinations:**

1. Local + QoS 0
2. Local + QoS 0 + Retained
3. Local + QoS 1 (persistence)
4. Local + QoS 1 + Retained (persistence)
5. CPC to CSC + QoS 0
6. CPC to CSC + QoS 0 + Retained
7. CPC to CSC + QoS 1 (persistence)
8. CPC to CSC + QoS 1 + Retained (persistence)
9. CSC to CPC + QoS 0
10. CSC to CPC + QoS 0 + Retained
11. CSC to CPC + QoS 1 (persistence)
12. CSC to CPC + QoS 1 + Retained (persistence)

### Local Performance Tests

Test throughput on a single cluster:

```bash
# From local/
make -C .. test

# Or run directly when broker URLs are already exported
go test -v ./tests/performance/ -run 'TestThroughput.*_Local'
```

**Tests:**

- `TestThroughputQoS0_Local`: QoS 0
- `TestThroughputQoS0Retained_Local`: QoS 0 with retained
- `TestThroughputQoS1_Local`: QoS 1 with persistence
- `TestThroughputQoS1Retained_Local`: QoS 1 with retained (persistence)

### Federation Performance Tests

Test cross-cluster throughput (CPC1 <-> CSC):

```bash
# From local/
make -C .. test

# Or run directly when broker URLs are already exported
go test -v ./tests/performance/ -run 'TestThroughput.*_(CPCtoCSC|CSCtoCPC)'
```

**Tests (CPC1 -> CSC):**

- `TestThroughputQoS0_CPCtoCSC`
- `TestThroughputQoS0Retained_CPCtoCSC`
- `TestThroughputQoS1_CPCtoCSC` (with persistence)
- `TestThroughputQoS1Retained_CPCtoCSC` (persistence)

**Tests (CSC -> CPC1):**

- `TestThroughputQoS0_CSCtoCPC`
- `TestThroughputQoS0Retained_CSCtoCPC`
- `TestThroughputQoS1_CSCtoCPC` (with persistence)
- `TestThroughputQoS1Retained_CSCtoCPC` (persistence)

## Metrics

The performance tests expose Prometheus metrics:

**Metrics:**

- `mqtt_messages_published_total`: Total messages published
- `mqtt_messages_received_total`: Total messages received
- `mqtt_publish_duration_seconds`: Message publish latency histogram
- `mqtt_e2e_latency_seconds`: End-to-end latency histogram (publish to receive)
- `mqtt_connections_active`: Number of active connections
- `mqtt_errors_total`: Total errors by type
- `mqtt_throughput_messages_per_second`: Current throughput gauge

**Labels:**

- `broker`: Broker address
- `broker_pub`: Publisher broker (for federation)
- `broker_sub`: Subscriber broker (for federation)
- `topic`: Message topic
- `qos`: QoS level (0, 1, or 2)
- `retained`: Retained flag (true/false)
- `federation`: Federation mode (true/false)
- `role`: Connection role (publisher/subscriber)
- `direction`: Throughput direction (publish/receive)
- `error_type`: Type of error

## Quick Start

```bash
# From the repository root, run the full local e2e suite against the
# MetalLB Envoy Gateway IPs.
make test
make dummy-bms

# Run the smoke-sized MQTT benchmark suite.
go -C local/mqttbs run ./cmd/mqttbs run basic-suite \
  --broker tcp://172.18.200.1:1883 \
  --duration 5s \
  --connection-clients 50 \
  --connection-rate 25 \
  --fanout-subscribers 20 \
  --p2p-clients 20 \
  --fanin-publishers 20 \
  --fanin-subscribers 3 \
  --fanin-topics 20

# Direct test runs use the MetalLB Envoy Gateway IPs.
export CSC_BROKER_URL=tcp://172.18.200.1:1883
export CPC1_BROKER_URL=tcp://172.18.201.1:1883
export CPC2_BROKER_URL=tcp://172.18.202.1:1883
go test -v ./tests/performance/

# Publish looping BMS demo data directly against a reachable broker.
go run ./cmd/dummy-bms --broker tcp://172.18.200.1:1883 --csv examples/dsx_exemplar.csv --schema ../../schemas/asyncapi/bms/bms.yaml

# Run specific test
go test -v ./tests/performance/ -run TestThroughputQoS0_Local

# Skip long-running performance tests
go test -short ./tests/...
```

## Dummy BMS Producer

`cmd/dummy-bms` feeds the local CSC MQTT broker with representative BMS traffic
so the demo environment has realistic data for subscribers, dashboards, and
manual integration checks. It is a replay tool, not a synthetic data generator:
the CSV defines the timing and exact messages to publish, and the command keeps
replaying that sequence until it is stopped.

The producer validates each rendered message against the canonical BMS AsyncAPI
schema before it publishes anything. The sample scenario uses
`{{timestamp_ms}}` for readings that need a fresh event timestamp on each pass.

See `examples/dsx_exemplar.csv` for the sample scenario. You can provide
your own sample or edit the sample for custom data.

The sample is one 15-minute pass. It repeats a recorded 99-second capture and
adds two incidents, so consumers have something to detect.

Racks E02 to E06 each hold ten GB200 compute trays of four GPUs. Their power comes from a GPU
and tray model, so it agrees with GPU and BMC telemetry built on the same model:

- A GPU draws 145 W idle plus 705 W at full utilisation and clock.
- The GPUs of a rack run at utilisation levels spread evenly from 90% to 100%.
- A tray adds 450 W for its Grace CPUs, memory, NICs and fans, and its busbar input
  is its load over a 95% conversion, a lab value.
- A rack draws its trays' input plus any load the capture shows beyond them, such as
  switch trays. E02 and E06 read less than their ten trays in the capture, so they get
  none; E03 to E05 keep 7.9 to 8.8 kW.

From 1:00 to 4:00, an InfiniBand fault slows the synchronous training job on racks E02 to E06
to half its step rate. Every GPU waits on the same collective, so rack power falls, E02 from
39.0 kW to 24.9 kW. The busways, taps and CDU heat fall with it.

At 5:00 a cooling excursion starts on CDU `C-F00-CDU-05`:

- **Primary flow.** The facility water supply loses pressure, so the CDU's primary flow falls by
  55%. Its primary valves open fully as the controller tries to recover the flow. The
  pressure drop from the primary inlet to the outlet follows the square of the flow, the
  heat exchanger's system curve (ΔP ∝ Q²). The outlet stays at the return header's pressure,
  so the inlet falls from about 288 kPa to 215 kPa.
- **Secondary supply.** The CDU secondary supply rises from 31.4 C to 58.4 C over 2 minutes. It
  holds for 5 minutes and recovers over 3 minutes, at the end of the pass. The 27 C rise is a
  lab scenario value, chosen so the GPUs throttle. Each rack's supply and return liquid warm
  with it.
- **Throttling.** Each GPU sits 0.043 C per watt of board power above its rack's own supply.
  That is an upper bound: a real B200's 88 C max operating at the 1000 W limit on the 45 C
  coolant NVIDIA rates GB200 NVL72 for. Once the hottest GPU would pass 88 C, its SW thermal
  slowdown lowers its clock just enough to hold 88 C, no lower than a real B200's 120 MHz
  idle SM clock. The synchronous job steps at that clock: about 0.72 of full at the hold,
  set by E06's 60.0 C supply.
- **Rack power.** Each rack draws about 7.9 kW less at the hold (E02: 39.0 to 31.1 kW, 20%).
  The E05 taps and the row E busways show the matching drop. Rack E01 does not change.

Throughout the pass, the liquid side follows one heat quantity per rack: its liquid heat.

- A rack's liquid heat is the load its trays deliver past their conversion, plus its other
  load, which is taken as liquid-cooled.
- The trays' 5% conversion loss leaves as air, about 10 kW across the five racks before the
  fault. No air handler is modelled to remove it.
- Each rack's return is its supply plus its liquid heat over its flow.

The CDU readings follow a heat balance with that liquid heat:

- The primary flow is sized for the racks' liquid heat at a 10 F rise, about
  545 L/min, before the 55% loss.
- The primary outlet is the primary inlet plus that heat over the primary flow.
- The secondary flow is the sum of the rack flows.
- The secondary return is the secondary supply plus the racks' liquid heat over the
  secondary flow, so the racks, the CDU's secondary and its primary all carry the same heat.
- `C-F00-CDU-05` also lists rows F02 to F06 in `servesId`, but the capture has
  no row F telemetry, so the balance treats row F as unloaded.

The capture's own values for these four points are not physically possible.
Its primary outlet was hotter than the secondary return, its secondary return
was colder than every rack return, its secondary flow read 0, and its primary
flow read under 7 L/min.

`scripts/generate_exemplar_csv.py` generates the sample from the recorded
capture, which is the version of `examples/dsx_exemplar.csv` at commit
`28c66bf`. The script uses only the Python standard library. To regenerate the
sample, run these commands from `local/mqtt-client`:

```bash
git show 28c66bf:local/mqtt-client/examples/dsx_exemplar.csv > /tmp/bms-capture.csv
python3 scripts/generate_exemplar_csv.py /tmp/bms-capture.csv
```

From the repo root, run `make dummy-bms` after the local environment is
deployed. For direct runs, pass the broker, CSV, and schema paths explicitly.
Authentication options for real clients are covered in
[authentication.md](../../docs/authentication.md); the local dummy-BMS target
uses the no-auth example profile.

### Dummy BMS Scenario CSV

Scenario files must use exactly this header:

```csv
offset,topic,payload
```

Each row is one MQTT publish:

- `offset` is a Go duration such as `0s`, `500ms`, or `2m`. Offsets must be
  non-negative and non-decreasing because they are replayed relative to the
  start of each pass.
- `topic` must be a concrete topic that matches a BMS AsyncAPI channel. For
  BMS-originated sample data, use
  `BMS/v1/PUB/{Value|Metadata}/{objectType}/{pointType}/{tagPath}`. For
  example, `BMS/v1/PUB/Metadata/Rack/RackPower/site-a/row-1/rack-1/power` and
  `BMS/v1/PUB/Value/Rack/RackPower/site-a/row-1/rack-1/power` are valid
  concrete topics. Empty segments and MQTT wildcards (`+`, `#`) are rejected.
- `payload` must be JSON and must validate against the BMS AsyncAPI schema for
  the selected topic. Any topic parameter fields present in the payload must
  match the topic.
- `{{timestamp_ms}}` in the payload is replaced at publish time with the current
  Unix timestamp in milliseconds.
- `Metadata` topics are published retained. `Value` topics are live,
  non-retained publishes.

The command loops the scenario until interrupted. Add `--once` to publish one
pass and exit:

```bash
go run ./cmd/dummy-bms \
  --broker tcp://172.18.200.1:1883 \
  --csv examples/dsx_exemplar.csv \
  --schema ../../schemas/asyncapi/bms/bms.yaml \
  --once
```

To verify messages while `make dummy-bms` is running, subscribe to the CSC Envoy
Gateway from another terminal:

```bash
mosquitto_sub -h 172.18.200.1 -p 1883 -t 'BMS/v1/PUB/#' -v
```

## Development

### Adding a New Test

1. Add test function in `tests/functional/` or `tests/performance/`
2. Follow the existing patterns for test configuration and execution
3. Update test documentation in this README

### Running Against Local Broker

Use this only for client development against a standalone broker. The local Kind
environment uses the MetalLB Envoy Gateway IPs documented in the quick start
above.

```bash
# Start a local MQTT broker (using Docker)
docker run -d -p 1883:1883 eclipse-mosquitto:latest

# Run tests against that standalone broker
MQTT_BROKER=tcp://127.0.0.1:1883 go test -v ./tests/functional/
CSC_BROKER_URL=tcp://127.0.0.1:1883 CPC1_BROKER_URL=tcp://127.0.0.1:1883 go test -v ./tests/performance/
```

## Troubleshooting

### Connection Refused

```
Error: connection refused
```

**Solution:** Ensure the broker is running and accessible:

```bash
# Test connectivity
telnet 172.18.200.1 1883

# Check broker logs
kubectl --context kind-dsx-exchange logs -n csc-event-bus <pod-name>
```

### High Latency

If you see high latency in benchmarks:

1. Check network connectivity
2. Verify broker is not overloaded
3. Check QoS settings (QoS 0 is fastest)
4. Monitor broker resources (CPU, memory)
