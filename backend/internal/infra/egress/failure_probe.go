package egress

import (
	"context"
	"errors"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
)

const maxConcurrentFailureProbes = 8
const maxCachedFailureProbes = 1024

// SetFailureProber enables an immediate, deduplicated connectivity probe after
// a fixed proxy reports a transport failure. The callback persists the probe
// result; it must not depend on the failed request context.
func (m *Manager) SetFailureProber(value FailureProber) {
	m.failureProbeMu.Lock()
	m.failureProber = value
	if value == nil {
		clear(m.failureProbes)
	}
	m.failureProbeMu.Unlock()
}

func (m *Manager) scheduleFailureProbe(node domain.Node) {
	m.failureProbeMu.Lock()
	prober := m.failureProber
	state := m.failureProbes[node.ID]
	if prober == nil || state.running {
		m.failureProbeMu.Unlock()
		return
	}
	// A proxy outage can affect thousands of nodes at once. Bound recovery
	// traffic across nodes, and retire completed entries after the retry grace.
	active := 0
	now := time.Now()
	for id, probe := range m.failureProbes {
		if probe.running {
			active++
		} else if now.Sub(probe.lastCompleted) >= failureProbeCompletionGrace {
			delete(m.failureProbes, id)
		}
	}
	if active >= maxConcurrentFailureProbes {
		m.failureProbeMu.Unlock()
		return
	}
	if _, exists := m.failureProbes[node.ID]; !exists && len(m.failureProbes) >= maxCachedFailureProbes {
		var oldestID uint64
		var oldest time.Time
		for id, probe := range m.failureProbes {
			if !probe.running && (oldest.IsZero() || probe.lastCompleted.Before(oldest)) {
				oldestID, oldest = id, probe.lastCompleted
			}
		}
		delete(m.failureProbes, oldestID)
	}
	state.running = true
	state.done = make(chan struct{})
	m.failureProbes[node.ID] = state
	done := state.done
	m.failureProbeMu.Unlock()

	m.log().Info("egress_failure_probe_scheduled", "node_id", node.ID, "node_name", node.Name)
	if !m.tasks.start("failure_probe", func() {
		// 探针回调走 repo/probe 全链路, panic 不得击穿进程:batch.Do 转 PanicError。
		var (
			result domain.ProbeResult
			err    error
		)
		if probeErr := batch.Do(m.tasks.ctx, func(taskCtx context.Context) error {
			taskCtx, probeCancel := context.WithTimeout(taskCtx, failureProbeTimeout)
			defer probeCancel()
			result, err = prober(taskCtx, node.ID)
			return nil
		}); probeErr != nil {
			var panicErr *batch.PanicError
			if errors.As(probeErr, &panicErr) {
				err = panicErr
			}
		}

		if err == nil && result.Status == domain.ProbeStatusHealthy {
			m.invalidateNodes()
		}
		m.failureProbeMu.Lock()
		state := m.failureProbes[node.ID]
		if state.done == done {
			state.running = false
			state.lastCompleted = time.Now().UTC()
			m.failureProbes[node.ID] = state
		}
		close(done)
		m.failureProbeMu.Unlock()

		if err != nil {
			m.log().Warn("egress_failure_probe_failed", "node_id", node.ID, "node_name", node.Name, "error", err)
			return
		}
		m.log().Info("egress_failure_probe_completed", "node_id", node.ID, "node_name", node.Name, "probe_status", result.Status, "latency_ms", result.LatencyMS)
	}) {
		m.failureProbeMu.Lock()
		delete(m.failureProbes, node.ID)
		close(done)
		m.failureProbeMu.Unlock()
	}
}

func (m *Manager) waitForFailureProbe(ctx context.Context, nodeID uint64) (bool, error) {
	now := time.Now().UTC()
	m.failureProbeMu.Lock()
	state, exists := m.failureProbes[nodeID]
	m.failureProbeMu.Unlock()
	if !exists {
		return false, nil
	}
	if !state.running {
		return !state.lastCompleted.IsZero() && now.Sub(state.lastCompleted) < failureProbeCompletionGrace, nil
	}
	if state.done == nil {
		return false, nil
	}
	timer := time.NewTimer(failureProbeWaitTimeout)
	defer timer.Stop()
	select {
	case <-state.done:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return false, nil
	}
}
