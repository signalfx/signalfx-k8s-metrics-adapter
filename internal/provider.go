package internal

import (
	"context"
	"sync/atomic"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
	"k8s.io/metrics/pkg/apis/custom_metrics"
	"k8s.io/metrics/pkg/apis/external_metrics"

	"github.com/davecgh/go-spew/spew"
	"github.com/signalfx/golib/datapoint"
	"github.com/signalfx/golib/sfxclient"
	"sigs.k8s.io/custom-metrics-apiserver/pkg/provider"
)

// SignalFxProvider maps K8s metrics to SignalFlow job output
type SignalFxProvider struct {
	mapper   apimeta.RESTMapper
	registry *Registry

	totalGetMetricByNameCalls        int64
	totalGetMetricBySelectorCalls    int64
	totalGetExternalMetricCalls      int64
	totalListAllMetricsCalls         int64
	totalListAllExternalMetricsCalls int64
}

// NewSignalFxProvider returns an instance of SignalFxProvider
func NewSignalFxProvider(registry *Registry, mapper apimeta.RESTMapper) *SignalFxProvider {

	provider := &SignalFxProvider{
		mapper:   mapper,
		registry: registry,
	}

	return provider
}

var _ provider.MetricsProvider = &SignalFxProvider{}

func (p *SignalFxProvider) GetMetricByName(ctx context.Context, name types.NamespacedName, info provider.CustomMetricInfo, metricSelector labels.Selector) (*custom_metrics.MetricValue, error) {
	atomic.AddInt64(&p.totalGetMetricByNameCalls, 1)

	keyMetric := &HPAMetric{
		Namespace:      name.Namespace,
		TargetName:     name.Name,
		Metric:         info.Metric,
		GroupResource:  info.GroupResource,
		MetricSelector: metricSelector,
	}
	metric := p.registry.HPAMetricMatchingKey(keyMetric)
	if metric == nil {
		klog.Errorf("no matching metric found for key metric: %v", spew.Sdump(keyMetric))
		return nil, provider.NewMetricNotFoundForError(info.GroupResource, info.Metric, name.Name)
	}

	snapshot, err := p.registry.LatestSnapshot(metric)
	if err != nil || snapshot == nil {
		klog.Errorf("Error getting latest metric snapshot for metric %v: %v", spew.Sdump(keyMetric), err)
		return nil, provider.NewMetricNotFoundForError(info.GroupResource, info.Metric, name.Name)
	}
	if len(snapshot) == 0 {
		return nil, provider.NewMetricNotFoundForError(info.GroupResource, info.Metric, name.Name)
	}
	if len(snapshot) > 1 {
		klog.Errorf("expected one value for metric %v and got %d, using first one.", spew.Sdump(keyMetric), len(snapshot))
	}

	gvk, err := p.mapper.KindFor(schema.GroupVersionResource{
		Group:    info.GroupResource.Group,
		Resource: info.GroupResource.Resource,
	})
	if err != nil {
		klog.Errorf("Could not derive kind for %v", info.GroupResource)
	}

	var val float64
	var ts time.Time
	// Grab the first one since there is only one
	for _, valueMetadata := range snapshot {
		val = valueMetadata.Val
		ts = valueMetadata.Timestamp
	}

	out := custom_metrics.MetricValue{
		Timestamp: metav1.Time{Time: ts},
		DescribedObject: custom_metrics.ObjectReference{
			Kind:       gvk.Kind,
			APIVersion: gvk.Version,
			Namespace:  name.Namespace,
			Name:       name.Name,
		},
		Metric: custom_metrics.MetricIdentifier{
			Name: info.Metric,
		},
		Value: *resource.NewMilliQuantity(int64(val*1000), resource.DecimalSI),
	}

	klog.V(5).Infof("GetMetricByName(%v, %v, %v) -> %v", name, info, metricSelector, out)

	return &out, nil
}

func (p *SignalFxProvider) GetMetricBySelector(ctx context.Context, namespace string, selector labels.Selector, info provider.CustomMetricInfo, metricSelector labels.Selector) (*custom_metrics.MetricValueList, error) {
	atomic.AddInt64(&p.totalGetMetricBySelectorCalls, 1)

	keyMetric := &HPAMetric{
		Namespace:      namespace,
		PodSelector:    selector,
		GroupResource:  info.GroupResource,
		MetricSelector: metricSelector,
		Metric:         info.Metric,
	}
	metric := p.registry.HPAMetricMatchingKey(keyMetric)
	if metric == nil {
		klog.Errorf("no matching metric found for key metric: %v", spew.Sdump(keyMetric))
		return nil, provider.NewMetricNotFoundError(info.GroupResource, info.Metric)
	}

	snapshot, err := p.registry.LatestSnapshot(metric)
	if err != nil {
		klog.Errorf("Error getting latest metric snapshot for metric %v: %v", spew.Sdump(keyMetric), err)
		return nil, provider.NewMetricNotFoundError(info.GroupResource, info.Metric)
	}

	if snapshot == nil {
		return &custom_metrics.MetricValueList{}, nil
	}

	gvk, err := p.mapper.KindFor(schema.GroupVersionResource{
		Group:    info.GroupResource.Group,
		Resource: info.GroupResource.Resource,
	})
	if err != nil {
		klog.Errorf("Could not derive kind for %v", info.GroupResource)
	}

	var out custom_metrics.MetricValueList
	for _, tvm := range snapshot {
		out.Items = append(out.Items, custom_metrics.MetricValue{
			Timestamp: metav1.Time{Time: tvm.Timestamp},
			DescribedObject: custom_metrics.ObjectReference{
				Kind:       gvk.Kind,
				APIVersion: gvk.Version,
				Namespace:  namespace,
				Name:       tvm.PodName(),
			},
			Metric: custom_metrics.MetricIdentifier{
				Name: info.Metric,
			},
			Value: *resource.NewMilliQuantity(int64(tvm.Val*1000), resource.DecimalSI),
		})
	}

	klog.V(5).Infof("GetMetricBySelector(%v, %v, %v, %v) -> %v", namespace, selector, info, metricSelector, out)

	return &out, nil
}

func (p *SignalFxProvider) ListAllMetrics() []provider.CustomMetricInfo {
	out := p.registry.AllCustomMetrics()
	klog.Infof("ListAllMetrics() -> %v", out)
	atomic.AddInt64(&p.totalListAllMetricsCalls, 1)
	return out
}

func (p *SignalFxProvider) GetExternalMetric(ctx context.Context, namespace string, metricSelector labels.Selector, info provider.ExternalMetricInfo) (*external_metrics.ExternalMetricValueList, error) {
	atomic.AddInt64(&p.totalGetExternalMetricCalls, 1)

	keyMetric := &HPAMetric{
		Namespace:      namespace,
		MetricSelector: metricSelector,
		Metric:         info.Metric,
	}
	metric := p.registry.HPAMetricMatchingKey(keyMetric)
	if metric == nil {
		klog.Errorf("no matching metric found for key metric: %v", spew.Sdump(keyMetric))
		return nil, provider.NewMetricNotFoundError(schema.GroupResource{}, info.Metric)
	}
	snapshot, err := p.registry.LatestSnapshot(metric)
	if snapshot == nil {
		return &external_metrics.ExternalMetricValueList{}, nil
	}

	out := []external_metrics.ExternalMetricValue{}
	for _, tvm := range snapshot {
		out = append(out, external_metrics.ExternalMetricValue{
			Timestamp:    metav1.Time{Time: tvm.Timestamp},
			MetricName:   info.Metric,
			MetricLabels: nil,
			Value:        *resource.NewMilliQuantity(int64(tvm.Val*1000), resource.DecimalSI),
		})
	}

	klog.V(5).Infof("GetExternalMetric(%v, %v, %v) -> (%v, %v)", namespace, metricSelector, info, out, err)

	return &external_metrics.ExternalMetricValueList{
		Items: out,
	}, nil
}

func (p *SignalFxProvider) ListAllExternalMetrics() []provider.ExternalMetricInfo {
	metrics := p.registry.AllExternalMetrics()
	klog.V(5).Infof("ListAllExternalMetrics() -> %v", metrics)
	atomic.AddInt64(&p.totalListAllExternalMetricsCalls, 1)
	return metrics
}

func (p *SignalFxProvider) InternalMetrics() []*datapoint.Datapoint {
	out := []*datapoint.Datapoint{
		sfxclient.CumulativeP("total_calls", map[string]string{"method": "GetExternalMetric"}, &p.totalGetExternalMetricCalls),
		sfxclient.CumulativeP("total_calls", map[string]string{"method": "GetMetricBySelector"}, &p.totalGetMetricBySelectorCalls),
		sfxclient.CumulativeP("total_calls", map[string]string{"method": "GetMetricByName"}, &p.totalGetMetricByNameCalls),
		sfxclient.CumulativeP("total_calls", map[string]string{"method": "ListAllExternalMetrics"}, &p.totalListAllExternalMetricsCalls),
		sfxclient.CumulativeP("total_calls", map[string]string{"method": "ListAllMetrics"}, &p.totalListAllMetricsCalls),
	}
	out = append(out, p.registry.InternalMetrics()...)
	return out
}
