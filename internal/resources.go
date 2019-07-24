package internal

import (
	"fmt"
	"strings"

	autoscaling "k8s.io/api/autoscaling/v2beta1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
)

// HPAMetric is used to provide a standard definition of a metric that can be
// used to link together metrics defined in the HPAs and those requested by the
// K8s autoscaler via API requests.
type HPAMetric struct {
	// UID of the HPA that is associated with this metric
	UID types.UID
	// The type of K8s resource that the metric pertains to
	GroupResource schema.GroupResource
	// Which namespace the metric pertains to, will be blank for external
	// metrics
	Namespace string
	// The name of the metric in the HPA
	Metric string

	// For external metrics, the SignalFlow program specified in the HPA
	// annotations.
	Program string
	// Selectors for the metrics themselves.
	MetricSelector labels.Selector
	// The selector on the HPA target resource to select pods.  This is what
	// will come in the request for metrics from K8s.
	PodSelector labels.Selector
	// For specific Object custom metrics, the name of the target resource.
	// The type of the resource is specified in the GroupResource field.
	TargetName string
	// The "scaleTargetRef" of the HPA, i.e. the thing that the HPA is
	// "attached" to that is being scaled up or down in response to the metrics.
	HPAResource autoscaling.CrossVersionObjectReference
}

// IsExternal returns true if this metric is an external (not custom) metric.
func (m *HPAMetric) IsExternal() bool {
	return m.Program != ""
}

// Key is like a hash function except it just makes a big string
func (m *HPAMetric) Key() string {
	key := m.Metric + "|" + m.Namespace + "|" + m.GroupResource.String() + "|" + m.TargetName + "|"
	if m.PodSelector != nil && !m.PodSelector.Empty() {
		key += m.PodSelector.String() + "|"
	}
	if m.MetricSelector != nil && !m.MetricSelector.Empty() {
		key += m.MetricSelector.String() + "|"
	}
	return key
}

func (m *HPAMetric) SignalFlowProgram() string {
	if m.Program != "" {
		return m.Program
	}

	var filters []string
	if m.TargetName != "" {
		filters = append(filters, fmt.Sprintf(`filter("kubernetes_name", "%s")`, m.TargetName))
	}

	if m.PodSelector != nil && !m.PodSelector.Empty() {
		podFilters, err := filtersFromSelector(m.PodSelector)
		if err != nil {
			klog.Errorf("Error making pod filters for metric %s: %v", m.Metric, err)
		}
		filters = append(filters, podFilters...)
	}
	if m.MetricSelector != nil && !m.MetricSelector.Empty() {
		metricFilters, err := filtersFromSelector(m.MetricSelector)
		if err != nil {
			klog.Errorf("Error making metric filters for metric %s: %v", m.Metric, err)
		}
		filters = append(filters, metricFilters...)
	}
	if len(m.Namespace) > 0 {
		filters = append(filters, fmt.Sprintf(`filter("kubernetes_namespace", "%s")`, m.Namespace))
	}

	var program string
	if len(filters) > 0 {
		program = fmt.Sprintf(`data("%s", filter=%s)`, m.Metric, strings.Join(filters, " and "))
	} else {
		program = fmt.Sprintf(`data("%s")`, m.Metric)
	}

	return program + ".publish()"
}

func filtersFromSelector(selector labels.Selector) ([]string, error) {
	var filters []string

	reqs, selectable := selector.Requirements()
	if !selectable {
		return nil, fmt.Errorf("selector '%v' is not selectable", selector)
	}
	for _, req := range reqs {
		if req.Operator() != selection.Equals {
			return nil, fmt.Errorf("unsupported selector operator in selector '%v'", selector)
		}
		filters = append(filters, fmt.Sprintf(`filter("%s", "%s")`, req.Key(), req.Values().List()[0]))
	}
	return filters, nil
}
