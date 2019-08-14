package internal

import (
	"sync"

	"github.com/kubernetes-incubator/custom-metrics-apiserver/pkg/provider"
	"github.com/signalfx/golib/datapoint"
	"github.com/signalfx/golib/sfxclient"
	"k8s.io/klog"
)

type Registry struct {
	sync.RWMutex

	jobRunner    *SignalFlowJobRunner
	metricsByKey map[string]*HPAMetric
}

func NewRegistry(jobRunner *SignalFlowJobRunner) *Registry {
	return &Registry{
		jobRunner:    jobRunner,
		metricsByKey: make(map[string]*HPAMetric),
	}
}

func (r *Registry) HandleHPAUpdated(updatedMetrics []HPAMetric) {
	r.Lock()
	defer r.Unlock()

	for i := range updatedMetrics {
		m := updatedMetrics[i]
		r.metricsByKey[m.Key()] = &m
		err := r.jobRunner.ReplaceOrStartJob(m.SignalFlowProgram())
		if err != nil {
			klog.Errorf("failed to start SignalFlow computation (%s): %v", m.SignalFlowProgram(), err)
		}
	}
}

func (r *Registry) HandleHPADeleted(deletedMetrics []HPAMetric) {
	r.Lock()
	defer r.Unlock()

	for _, m := range deletedMetrics {
		delete(r.metricsByKey, m.Key())
		r.jobRunner.StopJob(m.SignalFlowProgram())
	}
}

func (r *Registry) AllCustomMetrics() []provider.CustomMetricInfo {
	r.RLock()
	defer r.RUnlock()

	// Ensure uniqueness of the metric infos
	outSet := map[provider.CustomMetricInfo]bool{}
	for _, metric := range r.metricsByKey {
		if metric.IsExternal() {
			continue
		}
		outSet[provider.CustomMetricInfo{
			GroupResource: metric.GroupResource,
			Namespaced:    metric.Namespace != "",
			Metric:        metric.Metric,
		}] = true
	}

	var out []provider.CustomMetricInfo
	for k := range outSet {
		out = append(out, k)
	}
	return out
}

func (r *Registry) AllExternalMetrics() []provider.ExternalMetricInfo {
	r.RLock()
	defer r.RUnlock()

	metricsSeen := map[string]bool{}
	var out []provider.ExternalMetricInfo
	for _, metric := range r.metricsByKey {
		if !metric.IsExternal() {
			continue
		}
		if metricsSeen[metric.Metric] {
			continue
		}

		out = append(out, provider.ExternalMetricInfo{
			Metric: metric.Metric,
		})
		metricsSeen[metric.Metric] = true
	}
	return out
}

func (r *Registry) HPAMetricMatchingKey(m *HPAMetric) *HPAMetric {
	r.RLock()
	defer r.RUnlock()

	return r.metricsByKey[m.Key()]
}

func (r *Registry) LatestSnapshot(m *HPAMetric) (*MetricSnapshot, error) {
	return r.jobRunner.LatestSnapshot(m)
}

func (r *Registry) InternalMetrics() []*datapoint.Datapoint {
	externals := r.AllExternalMetrics()
	customs := r.AllCustomMetrics()

	out := []*datapoint.Datapoint{
		sfxclient.Gauge("custom_metrics", nil, int64(len(customs))),
		sfxclient.Gauge("external_metrics", nil, int64(len(externals))),
	}
	out = append(out, r.jobRunner.InternalMetrics()...)
	return out
}
