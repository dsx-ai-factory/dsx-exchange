// Copyright 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dummybms

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestLoadScenarioParsesScenarioAndRendersTimestamp(t *testing.T) {
	path := writeScenario(t, `offset,topic,payload
0s,BMS/v1/PUB/Metadata/Rack/RackPower/site-a/row-1/rack-1/power,"{""objectType"":""Rack"",""pointType"":""RackPower"",""rackLocationName"":""row-1 rack-1"",""rackLocationId"":""rack-1"",""engUnit"":""kW""}"
500ms,BMS/v1/PUB/Value/Rack/RackPower/site-a/row-1/rack-1/power,"{""value"":42.5,""timestamp"":{{timestamp_ms}},""quality"":1}"
`)

	scenario, err := loadScenario(t, path)
	if err != nil {
		t.Fatalf("loadScenario() error = %v", err)
	}

	if len(scenario.Entries) != 2 {
		t.Fatalf("len(Entries) = %d, want 2", len(scenario.Entries))
	}
	if scenario.Entries[0].Offset != 0 {
		t.Fatalf("first offset = %s, want 0s", scenario.Entries[0].Offset)
	}
	if scenario.Entries[1].Offset != 500*time.Millisecond {
		t.Fatalf("second offset = %s, want 500ms", scenario.Entries[1].Offset)
	}

	payload, err := scenario.Entries[1].RenderPayload(time.UnixMilli(1743620423000))
	if err != nil {
		t.Fatalf("RenderPayload() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("rendered payload is not JSON: %v", err)
	}
	if got["timestamp"] != float64(1743620423000) {
		t.Fatalf("timestamp = %v, want 1743620423000", got["timestamp"])
	}
}

func TestLoadScenarioRejectsComments(t *testing.T) {
	path := writeScenario(t, `# comment
offset,topic,payload
500ms,BMS/v1/PUB/Value/Rack/RackPower/site-a/row-1/rack-1/power,"{""value"":42.5,""timestamp"":1743620423000,""quality"":1}"
`)

	if _, err := loadScenario(t, path); err == nil {
		t.Fatal("loadScenario() error = nil, want comment rejection")
	}
}

func TestLoadScenarioRejectsInvalidValuePayload(t *testing.T) {
	path := writeScenario(t, `offset,topic,payload
0s,BMS/v1/PUB/Value/Rack/RackPower/site-a/row-1/rack-1/power,"{""value"":42.5,""timestamp"":1743620423000}"
`)

	if _, err := loadScenario(t, path); err == nil {
		t.Fatal("loadScenario() error = nil, want missing quality error")
	}
}

func TestLoadScenarioRejectsMetadataTopicMismatch(t *testing.T) {
	path := writeScenario(t, `offset,topic,payload
0s,BMS/v1/PUB/Metadata/Rack/RackPower/site-a/row-1/rack-1/power,"{""objectType"":""Rack"",""pointType"":""RackLiquidFlow"",""rackLocationName"":""row-1 rack-1"",""rackLocationId"":""rack-1"",""engUnit"":""LPM""}"
`)

	if _, err := loadScenario(t, path); err == nil {
		t.Fatal("loadScenario() error = nil, want topic mismatch error")
	}
}

func TestLoadScenarioRejectsMQTTWildcardsInPublishTopic(t *testing.T) {
	path := writeScenario(t, `offset,topic,payload
0s,BMS/v1/PUB/Value/Rack/RackPower/site-a/row-1/#,"{""value"":42.5,""timestamp"":1743620423000,""quality"":1}"
`)

	if _, err := loadScenario(t, path); err == nil {
		t.Fatal("loadScenario() error = nil, want wildcard topic error")
	}
}

func TestLoadScenarioRejectsMetadataThatViolatesBMSSchema(t *testing.T) {
	path := writeScenario(t, `offset,topic,payload
0s,BMS/v1/PUB/Metadata/Rack/RackPower/site-a/row-1/rack-1/power,"{""objectType"":""Rack"",""pointType"":""RackPower"",""rackLocationName"":""row-1 rack-1"",""rackLocationId"":""rack-1"",""engUnit"":""C""}"
`)

	if _, err := loadScenario(t, path); err == nil {
		t.Fatal("loadScenario() error = nil, want BMS schema validation error")
	}
}

func TestLoadScenarioRejectsOutOfOrderOffsets(t *testing.T) {
	path := writeScenario(t, `offset,topic,payload
1s,BMS/v1/PUB/Value/Rack/RackPower/site-a/row-1/rack-1/power,"{""value"":42.5,""timestamp"":1743620423000,""quality"":1}"
500ms,BMS/v1/PUB/Value/Rack/RackPower/site-a/row-1/rack-1/power,"{""value"":43.0,""timestamp"":1743620423500,""quality"":1}"
`)

	if _, err := loadScenario(t, path); err == nil {
		t.Fatal("loadScenario() error = nil, want out-of-order offset error")
	}
}

func TestLoadScenarioRejectsBareNumericOffset(t *testing.T) {
	path := writeScenario(t, `offset,topic,payload
500,BMS/v1/PUB/Value/Rack/RackPower/site-a/row-1/rack-1/power,"{""value"":42.5,""timestamp"":1743620423000,""quality"":1}"
`)

	if _, err := loadScenario(t, path); err == nil {
		t.Fatal("loadScenario() error = nil, want bare numeric offset error")
	}
}

func TestLoadScenarioRequiresCanonicalCSVColumns(t *testing.T) {
	path := writeScenario(t, `time,topic,message
500ms,BMS/v1/PUB/Value/Rack/RackPower/site-a/row-1/rack-1/power,"{""value"":42.5,""timestamp"":1743620423000,""quality"":1}"
`)

	if _, err := loadScenario(t, path); err == nil {
		t.Fatal("loadScenario() error = nil, want canonical column error")
	}
}

func TestLoadScenarioRejectsNonCanonicalCSVColumns(t *testing.T) {
	tests := []struct {
		name   string
		header string
	}{
		{
			name:   "uppercase",
			header: "Offset,topic,payload",
		},
		{
			name:   "leading space",
			header: "offset, topic,payload",
		},
		{
			name:   "trailing space",
			header: "offset,topic,payload ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeScenario(t, tt.header+`
500ms,BMS/v1/PUB/Value/Rack/RackPower/site-a/row-1/rack-1/power,"{""value"":42.5,""timestamp"":1743620423000,""quality"":1}"
`)

			if _, err := loadScenario(t, path); err == nil {
				t.Fatal("loadScenario() error = nil, want noncanonical column error")
			}
		})
	}
}

func TestLoadScenarioRejectsDuplicateCSVColumns(t *testing.T) {
	path := writeScenario(t, `offset,topic,payload,payload
500ms,BMS/v1/PUB/Value/Rack/RackPower/site-a/row-1/rack-1/power,"{""value"":42.5,""timestamp"":1743620423000,""quality"":1}","{""value"":42.5,""timestamp"":1743620423000,""quality"":1}"
`)

	if _, err := loadScenario(t, path); err == nil {
		t.Fatal("loadScenario() error = nil, want duplicate column error")
	}
}

func TestLoadScenarioRejectsUnknownCSVColumns(t *testing.T) {
	path := writeScenario(t, `offset,topic,payload,extra
500ms,BMS/v1/PUB/Value/Rack/RackPower/site-a/row-1/rack-1/power,"{""value"":42.5,""timestamp"":1743620423000,""quality"":1}",ignored
`)

	if _, err := loadScenario(t, path); err == nil {
		t.Fatal("loadScenario() error = nil, want unknown column error")
	}
}

func TestLoadScenarioRejectsExtraCSVFields(t *testing.T) {
	path := writeScenario(t, `offset,topic,payload
500ms,BMS/v1/PUB/Value/Rack/RackPower/site-a/row-1/rack-1/power,"{""value"":42.5,""timestamp"":1743620423000,""quality"":1}",ignored
`)

	if _, err := loadScenario(t, path); err == nil {
		t.Fatal("loadScenario() error = nil, want extra field error")
	}
}

func TestLoadScenarioRejectsNonCanonicalValues(t *testing.T) {
	tests := []struct {
		name string
		row  string
	}{
		{
			name: "offset leading space",
			row:  ` 500ms,BMS/v1/PUB/Value/Rack/RackPower/site-a/row-1/rack-1/power,"{""value"":42.5,""timestamp"":1743620423000,""quality"":1}"`,
		},
		{
			name: "topic leading space",
			row:  `500ms, BMS/v1/PUB/Value/Rack/RackPower/site-a/row-1/rack-1/power,"{""value"":42.5,""timestamp"":1743620423000,""quality"":1}"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeScenario(t, "offset,topic,payload\n"+tt.row+"\n")

			if _, err := loadScenario(t, path); err == nil {
				t.Fatal("loadScenario() error = nil, want noncanonical value error")
			}
		})
	}
}

func TestLoadScenarioAcceptsGeneratedExemplarCSV(t *testing.T) {
	scenario, err := LoadScenario("../../examples/dsx_exemplar.csv", testBMSSchema(t))
	if err != nil {
		t.Fatalf("LoadScenario() error = %v", err)
	}

	if len(scenario.Entries) != 41340 {
		t.Fatalf("len(Entries) = %d, want 41340", len(scenario.Entries))
	}
	placeholderCount := 0
	for _, entry := range scenario.Entries {
		if strings.Contains(entry.payloadTemplate, timestampPlaceholder) {
			placeholderCount++
		}
	}
	if placeholderCount != 41174 {
		t.Fatalf("timestamp placeholder count = %d, want 41174", placeholderCount)
	}
	wantLastOffset := 15 * time.Minute
	if got := scenario.Entries[len(scenario.Entries)-1].Offset; got != wantLastOffset {
		t.Fatalf("last offset = %s, want %s", got, wantLastOffset)
	}
}

func TestExemplarCSVCarriesTheCDUCoolingExcursion(t *testing.T) {
	scenario, err := LoadScenario("../../examples/dsx_exemplar.csv", testBMSSchema(t))
	if err != nil {
		t.Fatalf("LoadScenario() error = %v", err)
	}

	const prefix = "BMS/v1/PUB/Value/"
	cdu := prefix + "CDU/%s/nvidia/titan/pod1C/C-F00-CDU-05/%s"
	cduSupply := fmt.Sprintf(cdu, "LiquidTemperature", "secondary/supply/temperature/feedback/value")
	primaryFlow := fmt.Sprintf(cdu, "LiquidFlow", "primary/flow/feedback/value")
	primaryOutlet := fmt.Sprintf(cdu, "LiquidTemperature", "primary/outlet/temperature/feedback/value")
	primaryInlet := fmt.Sprintf(cdu, "LiquidTemperature", "primary/inlet/temperature/feedback/value")
	secondaryReturn := fmt.Sprintf(cdu, "LiquidTemperature", "secondary/return/temperature/feedback/value")
	secondaryFlow := fmt.Sprintf(cdu, "LiquidFlow", "secondary/flow/feedback/value")
	inletPressure := fmt.Sprintf(cdu, "LiquidPressure", "primary/inlet/pressure/feedback/value")
	outletPressure := fmt.Sprintf(cdu, "LiquidPressure", "primary/outlet/pressure/feedback/value")
	e02Power := prefix + "Rack/RackPower/nvidia/titan/pod1C/rowE/rack02/info/power/power/value"
	e01Power := prefix + "Rack/RackPower/nvidia/titan/pod1C/rowE/rack01/info/power/power/value"

	type sample struct {
		at    time.Duration
		value float64
	}
	series := map[string][]sample{}
	for _, entry := range scenario.Entries {
		if !strings.HasPrefix(entry.Topic, prefix) {
			continue
		}
		payload, err := entry.RenderPayload(time.UnixMilli(0))
		if err != nil {
			t.Fatalf("RenderPayload() error = %v", err)
		}
		var reading struct {
			Value *float64 `json:"value"`
		}
		if err := json.Unmarshal(payload, &reading); err != nil {
			t.Fatalf("json.Unmarshal() error = %v", err)
		}
		if reading.Value != nil {
			series[entry.Topic] = append(series[entry.Topic], sample{entry.Offset, *reading.Value})
		}
	}
	// The latest sample at or before a time, as a sample-and-hold consumer reads it
	held := func(topic string, when time.Duration) (sample, bool) {
		samples := series[topic]
		k := sort.Search(len(samples), func(i int) bool { return samples[i].at > when })
		if k == 0 {
			return sample{}, false
		}
		return samples[k-1], true
	}
	window := func(topic string, from, to time.Duration) []float64 {
		var values []float64
		for _, s := range series[topic] {
			if s.at >= from && s.at < to {
				values = append(values, s.value)
			}
		}
		if len(values) == 0 {
			t.Fatalf("%s has no readings in %s-%s", topic, from, to)
		}
		return values
	}

	minute := time.Minute
	checks := []struct {
		topic    string
		from, to time.Duration
		min, max float64
	}{
		{cduSupply, 0, 5 * minute, 31, 32},
		{cduSupply, 7 * minute, 12 * minute, 58, 59},
		{cduSupply, 14*minute + 50*time.Second, 15*minute + time.Second, 31, 32.5},
		// Rack E02 is ten compute trays: their draw before, during the fabric fault and at the hold
		{e02Power, 0, minute, 38.9, 39.2},
		{e02Power, 70 * time.Second, 230 * time.Second, 24.8, 25.1},
		{e02Power, 7 * minute, 12 * minute, 31, 31.3},
		{e01Power, 7 * minute, 12 * minute, 0.09, 0.1},
		// Facility water sized for the racks' liquid heat at a 10 F rise, losing 55% in the hold
		{primaryFlow, 0, 5 * minute, 542, 547},
		{primaryFlow, 7 * minute, 12 * minute, 243, 247},
	}
	for _, c := range checks {
		for _, v := range window(c.topic, c.from, c.to) {
			if v < c.min || v > c.max {
				t.Fatalf("%s at %s-%s = %v, want %v-%v", c.topic, c.from, c.to, v, c.min, c.max)
			}
		}
	}

	// The racks' liquid heat, CDU-05's secondary and its primary agree wherever the held samples are current:
	// from the same phase, and in the ramp and recovery published within 2 s of each other
	const waterCp = 4.186 // kJ/(kg K), at 1 kg/L
	bounds := []time.Duration{60 * time.Second, 240 * time.Second, 5 * minute, 7 * minute, 12 * minute}
	phases := []string{"before the fault", "fault", "after the fault", "ramp", "hold", "recovery"}
	phase := func(when time.Duration) int {
		return sort.Search(len(bounds), func(i int) bool { return bounds[i] > when })
	}
	var rackInputs []string
	for _, rack := range []string{"rack02", "rack03", "rack04", "rack05", "rack06"} {
		for _, point := range [][2]string{
			{"RackLiquidFlow", "flow"},
			{"RackLiquidSupplyTemperature", "supplyTemperature"},
			{"RackLiquidReturnTemperature", "returnTemperature"},
		} {
			rackInputs = append(rackInputs, prefix+"Rack/"+point[0]+"/nvidia/titan/pod1C/rowE/"+rack+"/energyValve/"+point[1]+"/feedback/value")
		}
	}
	cduInputs := []string{primaryFlow, primaryOutlet, primaryInlet, secondaryFlow, secondaryReturn, cduSupply}
	checked := map[int]int{}
	// Check whenever any input publishes
	var times []time.Duration
	for _, topic := range append(append([]string{}, cduInputs...), rackInputs...) {
		for _, s := range series[topic] {
			times = append(times, s.at)
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	for _, when := range times {
		values := map[string]float64{}
		current := true
		for _, topic := range append(append([]string{}, cduInputs...), rackInputs...) {
			h, ok := held(topic, when)
			transient := phase(when) == 3 || phase(when) == 5
			if !ok || phase(h.at) != phase(when) || (transient && when-h.at > 2*time.Second) {
				current = false
				break
			}
			values[topic] = h.value
		}
		if !current {
			continue
		}
		rackKW := 0.0
		for k := 0; k < len(rackInputs); k += 3 {
			rackKW += values[rackInputs[k]] / 60 * waterCp * (values[rackInputs[k+2]] - values[rackInputs[k+1]])
		}
		secondaryKW := values[secondaryFlow] / 60 * waterCp * (values[secondaryReturn] - values[cduSupply])
		primaryKW := values[primaryFlow] / 60 * waterCp * (values[primaryOutlet] - values[primaryInlet])
		for name, kw := range map[string]float64{"secondary": secondaryKW, "primary": primaryKW} {
			if kw < rackKW*0.96 || kw > rackKW*1.04 {
				t.Fatalf("%s heat at %s = %.1f kW, want the racks' liquid heat %.1f kW within 4%%", name, when, kw, rackKW)
			}
		}
		checked[phase(when)]++
	}
	// Before the fault no secondary flow reading has been published yet
	for k, name := range phases[1:] {
		if checked[k+1] == 0 {
			t.Fatalf("no current heat-balance check in the %s", name)
		}
	}

	// The facility supply loses pressure: inlet stays above outlet, the drop following the square of the flow
	first := series[primaryFlow][0]
	design := first.value
	firstInlet := series[inletPressure][0]
	firstOutlet, _ := held(outletPressure, firstInlet.at)
	designDrop := firstInlet.value - firstOutlet.value
	for _, s := range series[inletPressure] {
		outlet, ok := held(outletPressure, s.at)
		flow, flowOK := held(primaryFlow, s.at)
		transient := phase(s.at) == 3 || phase(s.at) == 5
		if !ok || !flowOK || phase(flow.at) != phase(s.at) || (transient && s.at-flow.at > 2*time.Second) {
			continue
		}
		drop := s.value - outlet.value
		if drop <= 0 {
			t.Fatalf("primary inlet pressure at %s is not above the outlet", s.at)
		}
		ratio := flow.value / design
		if want := designDrop * ratio * ratio; math.Abs(drop-want) > 8 {
			t.Fatalf("primary pressure drop at %s = %.1f kPa, want %.1f kPa within 8 kPa", s.at, drop, want)
		}
	}
}

func TestPublishOnceWaitsAndPublishesScenarioRows(t *testing.T) {
	path := writeScenario(t, `offset,topic,payload
0s,BMS/v1/PUB/Value/Rack/RackPower/site-a/row-1/rack-1/power,"{""value"":42.5,""timestamp"":{{timestamp_ms}},""quality"":1}"
750ms,BMS/v1/PUB/Value/Rack/RackLiquidSupplyTemperature/site-a/row-1/rack-1/supply-temp,"{""value"":22.9,""timestamp"":{{timestamp_ms}},""quality"":1}"
`)

	scenario, err := loadScenario(t, path)
	if err != nil {
		t.Fatalf("loadScenario() error = %v", err)
	}

	publisher := &recordingPublisher{}
	startedAt := time.UnixMilli(1743620423000)
	var publishTimes []time.Time
	options := PublishOptions{
		QoS: 1,
		Now: func() time.Time {
			return startedAt
		},
		WaitUntil: func(ctx context.Context, publishAt time.Time) error {
			publishTimes = append(publishTimes, publishAt)
			return nil
		},
	}

	if err := PublishOnce(context.Background(), scenario, publisher, options); err != nil {
		t.Fatalf("PublishOnce() error = %v", err)
	}

	if len(publishTimes) != 2 {
		t.Fatalf("len(publishTimes) = %d, want 2", len(publishTimes))
	}
	if !publishTimes[0].Equal(startedAt) || !publishTimes[1].Equal(startedAt.Add(750*time.Millisecond)) {
		t.Fatalf("publishTimes = %v, want [%s %s]", publishTimes, startedAt, startedAt.Add(750*time.Millisecond))
	}
	if len(publisher.messages) != 2 {
		t.Fatalf("len(messages) = %d, want 2", len(publisher.messages))
	}
	if publisher.messages[0].qos != 1 || publisher.messages[0].retain {
		t.Fatalf("publish options = qos %d retain %v, want qos 1 retain false", publisher.messages[0].qos, publisher.messages[0].retain)
	}

	var got map[string]any
	if err := json.Unmarshal(publisher.messages[0].payload, &got); err != nil {
		t.Fatalf("published payload is not JSON: %v", err)
	}
	if got["timestamp"] != float64(1743620423000) {
		t.Fatalf("timestamp = %v, want 1743620423000", got["timestamp"])
	}
}

func TestPublishOnceRetainsMetadataOnly(t *testing.T) {
	path := writeScenario(t, `offset,topic,payload
0s,BMS/v1/PUB/Metadata/Rack/RackPower/site-a/row-1/rack-1/power,"{""objectType"":""Rack"",""pointType"":""RackPower"",""rackLocationName"":""row-1 rack-1"",""rackLocationId"":""rack-1"",""engUnit"":""kW""}"
1s,BMS/v1/PUB/Value/Rack/RackPower/site-a/row-1/rack-1/power,"{""value"":42.5,""timestamp"":1743620423000,""quality"":1}"
`)

	scenario, err := loadScenario(t, path)
	if err != nil {
		t.Fatalf("loadScenario() error = %v", err)
	}

	publisher := &recordingPublisher{}
	options := PublishOptions{
		WaitUntil: func(ctx context.Context, publishAt time.Time) error {
			return nil
		},
	}

	if err := PublishOnce(context.Background(), scenario, publisher, options); err != nil {
		t.Fatalf("PublishOnce() error = %v", err)
	}

	if len(publisher.messages) != 2 {
		t.Fatalf("len(messages) = %d, want 2", len(publisher.messages))
	}
	if !publisher.messages[0].retain {
		t.Fatal("metadata retain = false, want true")
	}
	if publisher.messages[1].retain {
		t.Fatal("value retain = true, want false")
	}
}

func TestPublishOncePassesContextToPublisher(t *testing.T) {
	path := writeScenario(t, `offset,topic,payload
0s,BMS/v1/PUB/Value/Rack/RackPower/site-a/row-1/rack-1/power,"{""value"":42.5,""timestamp"":1743620423000,""quality"":1}"
`)

	scenario, err := loadScenario(t, path)
	if err != nil {
		t.Fatalf("loadScenario() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	publisher := contextCheckingPublisher{}
	err = PublishOnce(ctx, scenario, publisher, PublishOptions{
		WaitUntil: func(ctx context.Context, publishAt time.Time) error {
			return nil
		},
	})
	if err == nil {
		t.Fatal("PublishOnce() error = nil, want canceled context error")
	}
}

type recordingPublisher struct {
	messages []recordedMessage
}

type recordedMessage struct {
	topic   string
	payload []byte
	qos     byte
	retain  bool
}

func (p *recordingPublisher) Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error {
	p.messages = append(p.messages, recordedMessage{
		topic:   topic,
		payload: payload,
		qos:     qos,
		retain:  retain,
	})
	return nil
}

type contextCheckingPublisher struct{}

func (p contextCheckingPublisher) Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error {
	return ctx.Err()
}

func loadScenario(t *testing.T, path string) (*Scenario, error) {
	t.Helper()

	return LoadScenario(path, testBMSSchema(t))
}

func testBMSSchema(t *testing.T) *BMSSchema {
	t.Helper()

	schema, err := LoadBMSSchema(testBMSSchemaPath(t))
	if err != nil {
		t.Fatalf("failed to load BMS schema: %v", err)
	}
	return schema
}

func testBMSSchemaPath(t *testing.T) string {
	t.Helper()

	path, err := filepath.Abs("../../../../schemas/asyncapi/bms/bms.yaml")
	if err != nil {
		t.Fatalf("failed to resolve BMS schema path: %v", err)
	}
	return path
}

func writeScenario(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "scenario.csv")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write scenario: %v", err)
	}
	return path
}
