package internal

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/davecgh/go-spew/spew"
	"github.com/signalfx/golib/datapoint"
	"github.com/signalfx/golib/sfxclient"
	autoscaling "k8s.io/api/autoscaling/v2"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
)

type HPACallback func(ctx context.Context, ms []HPAMetric)

type HPADiscoverer struct {
	client         kubernetes.Interface
	mapper         apimeta.RESTMapper
	metricConfig   map[types.UID][]HPAMetric
	updateCallback HPACallback
	deleteCallback HPACallback
	sync.Mutex

	totalHPAsWithMetrics int64
	totalMetricsTracked  int64
}

func NewHPADiscoverer(client kubernetes.Interface, updateCallback HPACallback, deleteCallback HPACallback, mapper apimeta.RESTMapper) *HPADiscoverer {
	if updateCallback == nil {
		panic("updateCallack should not be nil")
	}
	if deleteCallback == nil {
		panic("deleteCallack should not be nil")
	}

	return &HPADiscoverer{
		client:         client,
		metricConfig:   make(map[types.UID][]HPAMetric),
		updateCallback: updateCallback,
		deleteCallback: deleteCallback,
		mapper:         mapper,
	}
}

func (d *HPADiscoverer) Discover(ctx context.Context) {
	watchList := cache.NewListWatchFromClient(d.client.AutoscalingV2().RESTClient(), "horizontalpodautoscalers", "", fields.Everything())

	_, controller := cache.NewInformer(
		watchList,
		&autoscaling.HorizontalPodAutoscaler{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				metrics := d.updateHPA(ctx, obj.(*autoscaling.HorizontalPodAutoscaler))
				d.updateCallback(ctx, metrics)

				if len(metrics) > 0 {
					d.totalHPAsWithMetrics++
					d.totalMetricsTracked += int64(len(metrics))
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldHPA := oldObj.(*autoscaling.HorizontalPodAutoscaler)
				newHPA := newObj.(*autoscaling.HorizontalPodAutoscaler)
				// The Status field in HPAs changes pretty frequently, so only
				// process changes if the spec (where metrics are defined) or
				// the annotations (which is where config for this adapter
				// lives), is different from the old version.
				if reflect.DeepEqual(oldHPA.Spec, newHPA.Spec) && reflect.DeepEqual(oldHPA.Annotations, newHPA.Annotations) {
					return
				}
				oldMetrics := d.metricConfig[newHPA.UID]
				d.deleteCallback(ctx, oldMetrics)

				newMetrics := d.updateHPA(ctx, newHPA)
				d.updateCallback(ctx, newMetrics)

				if len(oldMetrics) != len(newMetrics) {
					d.totalMetricsTracked += int64(len(newMetrics) - len(oldMetrics))
					if len(newMetrics) == 0 {
						d.totalHPAsWithMetrics--
					}
				}
			},
			DeleteFunc: func(obj interface{}) {
				hpa := obj.(*autoscaling.HorizontalPodAutoscaler)
				klog.V(2).Infof("HPA was deleted: %v / %v", hpa.Namespace, hpa.Name)

				metrics := d.metricConfig[hpa.UID]
				delete(d.metricConfig, hpa.UID)

				if len(metrics) > 0 {
					d.totalHPAsWithMetrics--
					d.totalMetricsTracked -= int64(len(metrics))
				}

				d.deleteCallback(ctx, metrics)
			},
		})

	go controller.Run(ctx.Done())
}

func (d *HPADiscoverer) updateHPA(ctx context.Context, hpa *autoscaling.HorizontalPodAutoscaler) []HPAMetric {
	klog.V(2).Infof("HPA was updated: %v / %v", hpa.Namespace, hpa.Name)

	metrics := d.extractConfig(ctx, hpa)
	klog.V(2).Infof("Extracted from %v / %v HPA: %v", hpa.Namespace, hpa.Name, spew.Sdump(metrics))
	d.metricConfig[hpa.UID] = metrics
	return metrics
}

var externalRE = regexp.MustCompile(`signalfx.com.external.metric/(?P<metric_name>.+)`)

const customMetricsAnnotation = "signalfx.com.custom.metrics"

func (d *HPADiscoverer) extractConfig(ctx context.Context, hpa *autoscaling.HorizontalPodAutoscaler) []HPAMetric {
	metrics := map[string]HPAMetric{}

	if whitelist, ok := hpa.Annotations[customMetricsAnnotation]; ok {
		whitelistSet := map[string]bool{}
		if whitelist != "" {
			for _, name := range strings.Split(whitelist, ",") {
				whitelistSet[name] = true
			}
		}
		d.populateHPACustomMetrics(ctx, metrics, hpa, whitelistSet)
	}

	externals := map[string]bool{}
	for k, v := range hpa.Annotations {
		if !strings.HasPrefix(k, "signalfx.com") {
			continue
		}

		if groups := externalRE.FindStringSubmatch(k); len(groups) > 0 {
			name := groups[1]
			ex := metrics[name]
			ex.Program = strings.TrimSpace(v)
			metrics[name] = ex
			externals[name] = true
			continue
		}

		if k != customMetricsAnnotation {
			klog.Errorf("hpa annotation has invalid metrics key: %s=%s", k, v)
		}
	}

	if len(externals) > 0 {
		d.populateHPAExternalMetrics(metrics, hpa, externals)
	}

	var metricsSlice []HPAMetric
	for _, m := range metrics {
		metricsSlice = append(metricsSlice, m)
	}
	return metricsSlice
}

func (d *HPADiscoverer) populateHPACustomMetrics(ctx context.Context, metrics map[string]HPAMetric, hpa *autoscaling.HorizontalPodAutoscaler, filterSet map[string]bool) {
	podSelector, err := getPodLabelSelector(ctx, d.client, hpa)
	if err != nil {
		klog.Errorf("Could not fetch pod selectors from HPA %s/%s target: %v", hpa.Namespace, hpa.Name, err)
	}

	for _, metric := range hpa.Spec.Metrics {
		if metric.Object != nil {
			name := metric.Object.Metric.Name
			if len(filterSet) > 0 && !filterSet[name] {
				continue
			}
			m := metrics[name]
			m.Metric = name
			m.Namespace = hpa.Namespace
			groupKind := schema.GroupKind{
				Kind: metric.Object.DescribedObject.Kind,
			}
			mapping, err := d.mapper.RESTMapping(groupKind, metric.Object.DescribedObject.APIVersion)
			if err != nil {
				klog.Errorf("Could not get mapping from %v to resource: %v", groupKind, err)
			}
			m.GroupResource.Group = mapping.Resource.Group
			m.GroupResource.Resource = mapping.Resource.Resource
			m.HPAResource = hpa.Spec.ScaleTargetRef
			m.TargetName = metric.Object.DescribedObject.Name
			if metric.Object.Metric.Selector != nil {
				// The requirements come from K8s so they should be pre-validated
				m.MetricSelector, _ = metav1.LabelSelectorAsSelector(metric.Object.Metric.Selector)
			}
			metrics[name] = m
			continue
		}

		if metric.Pods != nil {
			name := metric.Pods.Metric.Name
			if len(filterSet) > 0 && !filterSet[name] {
				continue
			}
			m := metrics[name]
			m.Metric = name
			m.Namespace = hpa.Namespace
			m.GroupResource.Resource = "pods"
			if metric.Pods.Metric.Selector != nil {
				// The selector comes from K8s so they should be pre-validated
				m.MetricSelector, _ = metav1.LabelSelectorAsSelector(metric.Pods.Metric.Selector)
			}
			m.HPAResource = hpa.Spec.ScaleTargetRef
			m.PodSelector = podSelector
			metrics[name] = m
			continue
		}
	}
}

func (d *HPADiscoverer) populateHPAExternalMetrics(metrics map[string]HPAMetric, hpa *autoscaling.HorizontalPodAutoscaler, filterSet map[string]bool) {
	for _, metric := range hpa.Spec.Metrics {
		if metric.External != nil {
			name := metric.External.Metric.Name
			if !filterSet[name] {
				continue
			}
			m := metrics[name]
			m.Metric = name
			m.Namespace = hpa.Namespace
			m.HPAResource = hpa.Spec.ScaleTargetRef
			if metric.External.Metric.Selector != nil {
				klog.Errorf("External metric selector (%v) is used only for identifying the metric, will be ignored in SignalFlow", metric.External.Metric.Selector)
				// The requirements come from K8s so they should be pre-validated
				m.MetricSelector, _ = metav1.LabelSelectorAsSelector(metric.External.Metric.Selector)
			}
			metrics[name] = m
			continue
		}
	}
}

// Copied and modified from https://github.com/zalando-incubator/kube-metrics-adapter/blob/9950851cad3a77ab575c78f88ddabf3df5e35039/pkg/collector/pod_collector.go
func getPodLabelSelector(ctx context.Context, client kubernetes.Interface, hpa *autoscaling.HorizontalPodAutoscaler) (labels.Selector, error) {
	var selector *metav1.LabelSelector
	switch hpa.Spec.ScaleTargetRef.Kind {
	case "Deployment":
		deployment, err := client.AppsV1().Deployments(hpa.Namespace).Get(ctx, hpa.Spec.ScaleTargetRef.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		selector = deployment.Spec.Selector
	case "StatefulSet":
		sts, err := client.AppsV1().StatefulSets(hpa.Namespace).Get(ctx, hpa.Spec.ScaleTargetRef.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		selector = sts.Spec.Selector
	default:
		return nil, fmt.Errorf("unable to get pod label selector for scale target ref '%s'", hpa.Spec.ScaleTargetRef.Kind)
	}
	return metav1.LabelSelectorAsSelector(selector)
}

func (d *HPADiscoverer) InternalMetrics() []*datapoint.Datapoint {
	return []*datapoint.Datapoint{
		sfxclient.Gauge("hpas_with_metrics", nil, atomic.LoadInt64(&d.totalHPAsWithMetrics)),
		sfxclient.Gauge("metrics_tracked", nil, atomic.LoadInt64(&d.totalMetricsTracked)),
	}
}
