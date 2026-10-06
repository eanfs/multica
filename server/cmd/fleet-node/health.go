package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"

	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/fleet/model"
)

// ReadObservation maps native daemon health. Maintenance trust is additive:
// absent or malformed stats must not turn ordinary readiness into a claim barrier.
func ReadObservation(client *http.Client, healthURL string) (model.Observation, error) {
	fail := func() (model.Observation, error) { return model.Observation{}, model.ErrUnknownHealth }
	if client == nil {
		return fail()
	}
	resp, err := client.Get(healthURL)
	if err != nil {
		return fail()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fail()
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024+1))
	if err != nil || len(raw) > 64*1024 {
		return fail()
	}
	fields, err := nativeHealthFields(raw)
	if err != nil {
		return fail()
	}
	for _, key := range []string{"daemon_id", "status", "active_task_count", "agents", "workspaces"} {
		value, ok := fields[key]
		if !ok || (key != "agents" && key != "workspaces" && string(value) == "null") {
			return fail()
		}
	}
	reportRaw := fields["report_queue_stats"]
	delete(fields, "report_queue_stats")
	// Decode stats separately so malformed new fields cannot invalidate legacy liveness.
	legacy, err := json.Marshal(fields)
	if err != nil {
		return fail()
	}
	var health daemon.HealthResponse
	if json.Unmarshal(legacy, &health) != nil || !validUUID(health.DaemonID) || health.ActiveTaskCount < 0 || health.ActiveTaskCount > int64(int(^uint(0)>>1)) {
		return fail()
	}
	out := model.Observation{DaemonID: health.DaemonID, Status: health.Status, ActiveRuns: int(health.ActiveTaskCount), Agents: health.Agents}
	for _, workspace := range health.Workspaces {
		out.RuntimeCount += len(workspace.Runtimes)
	}
	out.Ready = health.Status == "running" && slices.Contains(health.Agents, "claude") && out.RuntimeCount > 0
	var stats daemon.ReportQueueStats
	f, err := model.DecodeStrictObject(reportRaw, &stats)
	if err == nil && len(f) == 3 && stats.Known && stats.Pending >= 0 && stats.Failed >= 0 {
		out.ReportStatsKnown = true
		out.PendingReports = stats.Pending
		out.FailedReports = stats.Failed
	}
	return out, nil
}

// Native health allows nullable lists and additive legacy fields. Ambiguous new
// stats are unknown, while duplicate legacy fields invalidate the observation.
func nativeHealthFields(raw []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return nil, model.ErrUnknownHealth
	}
	fields := map[string]json.RawMessage{}
	duplicateStats := false
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, model.ErrUnknownHealth
		}
		key, ok := token.(string)
		if !ok {
			return nil, model.ErrUnknownHealth
		}
		var value json.RawMessage
		if dec.Decode(&value) != nil {
			return nil, model.ErrUnknownHealth
		}
		if _, exists := fields[key]; exists {
			if key != "report_queue_stats" {
				return nil, model.ErrUnknownHealth
			}
			duplicateStats = true
		}
		fields[key] = value
	}
	if token, err := dec.Token(); err != nil || token != json.Delim('}') {
		return nil, model.ErrUnknownHealth
	}
	if dec.Decode(new(any)) != io.EOF {
		return nil, model.ErrUnknownHealth
	}
	if duplicateStats {
		delete(fields, "report_queue_stats")
	}
	return fields, nil
}

type healthWire struct {
	DaemonID     string                  `json:"daemon_id"`
	Ready        bool                    `json:"ready"`
	RuntimeCount int                     `json:"runtime_count"`
	ActiveRuns   int                     `json:"active_runs"`
	Agents       []string                `json:"agents"`
	Reports      daemon.ReportQueueStats `json:"report_queue_stats"`
}

func observationWire(o model.Observation) healthWire {
	agents := o.Agents
	if agents == nil {
		agents = []string{}
	}
	return healthWire{DaemonID: o.DaemonID, Ready: o.Ready, RuntimeCount: o.RuntimeCount, ActiveRuns: o.ActiveRuns, Agents: agents, Reports: daemon.ReportQueueStats{Known: o.ReportStatsKnown, Pending: o.PendingReports, Failed: o.FailedReports}}
}
