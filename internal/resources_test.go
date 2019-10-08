package internal

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHPAMetricPrograms(t *testing.T) {
	tests := []struct {
		metric          HPAMetric
		expectedProgram string
	}{
		{
			metric: HPAMetric{
				Namespace: "default",
				Metric:    "my_metric",
				// Selectors for the metrics themselves.
				MetricSelector: forceLabelSelector(&metav1.LabelSelector{
					MatchLabels: map[string]string{
						"verb": "GET",
					},
				}),
				PodSelector: forceLabelSelector(&metav1.LabelSelector{
					MatchLabels: map[string]string{
						"example.com/app": "myapp",
					},
				}),
			},
			expectedProgram: `data("my_metric", filter=filter("example_com_app", "myapp") and filter("verb", "GET") and filter("kubernetes_namespace", "default")).publish()`,
		},
		{
			metric: HPAMetric{
				Namespace:  "default",
				Metric:     "my_metric",
				TargetName: "my_service",
				// Selectors for the metrics themselves.
				MetricSelector: forceLabelSelector(&metav1.LabelSelector{
					MatchLabels: map[string]string{
						"verb": "GET",
					},
				}),
			},
			expectedProgram: `data("my_metric", filter=filter("kubernetes_name", "my_service") and filter("verb", "GET") and filter("kubernetes_namespace", "default")).publish()`,
		},
	}
	for i := range tests {
		test := tests[i]
		require.Equal(t, test.metric.SignalFlowProgram(), test.expectedProgram)
	}
}
