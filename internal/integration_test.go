package internal

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/kubernetes-incubator/custom-metrics-apiserver/pkg/dynamicmapper"
	"github.com/kubernetes-incubator/custom-metrics-apiserver/pkg/provider"
	"github.com/signalfx/golib/pointer"
	"github.com/signalfx/signalfx-go/idtool"
	"github.com/signalfx/signalfx-go/signalflow"
	"github.com/signalfx/signalfx-go/signalflow/messages"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v2beta1"
	apiv1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/metrics/pkg/apis/custom_metrics"
	"k8s.io/metrics/pkg/apis/external_metrics"

	_ "k8s.io/client-go/plugin/pkg/client/auth"
)

var testNamespace = "metrics-adpater-inttest"
var timeout = 10 * time.Second

var commonDeployment = &appsv1.Deployment{
	ObjectMeta: metav1.ObjectMeta{
		Name: "demo-deployment",
	},
	Spec: appsv1.DeploymentSpec{
		Replicas: &[]int32{0}[0],
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app": "queue",
			},
		},
		Template: apiv1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"app": "queue",
				},
			},
			Spec: apiv1.PodSpec{
				Containers: []apiv1.Container{
					{
						Name:  "web",
						Image: "nginx:1.12",
						Ports: []apiv1.ContainerPort{
							{
								Name:          "http",
								Protocol:      apiv1.ProtocolTCP,
								ContainerPort: 80,
							},
						},
					},
				},
			},
		},
	},
}

// Tests inspired by
// https://github.com/zalando-incubator/kube-metrics-adapter/blob/9950851cad3a77ab575c78f88ddabf3df5e35039/pkg/provider/hpa_test.go.

func forceLabelSelector(selector *metav1.LabelSelector) labels.Selector {
	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		panic("bad selector: " + err.Error())
	}
	return sel
}

func prepNamespace(t *testing.T, k8sClient kubernetes.Interface) func() {
	cleanup := func() {
		_ = k8sClient.CoreV1().Namespaces().Delete(testNamespace, &metav1.DeleteOptions{
			GracePeriodSeconds: pointer.Int64(0),
			PropagationPolicy:  (*metav1.DeletionPropagation)(pointer.String("Foreground")),
		})
	}

	cleanup()
	var err error
	waitFor(30*time.Second, func() bool {
		_, err = k8sClient.CoreV1().Namespaces().Create(&apiv1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: testNamespace,
			},
		})
		return err == nil
	})
	require.Nil(t, err)

	return cleanup
}

func providerWithBackend(t *testing.T) (*SignalFxProvider, kubernetes.Interface, *signalflow.FakeBackend, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())

	backend := signalflow.NewRunningFakeBackend()
	client, _ := backend.Client()

	jobRunner := NewSignalFlowJobRunner(client)
	jobRunner.CleanupOldTSIDsInterval = 2 * time.Second
	go jobRunner.Run(ctx)

	registry := NewRegistry(jobRunner)

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	// if you want to change the loading rules (which files in which order), you can do so here

	configOverrides := &clientcmd.ConfigOverrides{}
	// if you want to change override values or bind them to flags, there are methods to help you

	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	config, err := kubeConfig.ClientConfig()
	require.Nil(t, err)

	k8sClient, err := kubernetes.NewForConfig(config)
	require.Nil(t, err)

	cleanup := prepNamespace(t, k8sClient)

	mapper, err := dynamicmapper.NewRESTMapper(k8sClient.Discovery(), 120*time.Second)
	require.Nil(t, err)
	discoverer := NewHPADiscoverer(k8sClient, registry.HandleHPAUpdated, registry.HandleHPADeleted, mapper)
	go discoverer.Discover(ctx)

	return NewSignalFxProvider(registry, mapper), k8sClient, backend, func() {
		cleanup()
		cancel()
	}
}

func TestPodMetrics(t *testing.T) {
	prov, k8sClient, fakeSignalFlow, cancel := providerWithBackend(t)
	defer cancel()

	deployment := commonDeployment
	hpa := &autoscalingv1.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name: "myapp",
			Annotations: map[string]string{
				"signalfx.com.custom.metrics": "",
				//"signalfx.com.external.metric/cpu": `data("cpu.utilization").publish()`,
			},
		},
		Spec: autoscalingv1.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
				Kind:       "Deployment",
				Name:       "demo-deployment",
				APIVersion: "apps/v1",
			},
			MinReplicas: &[]int32{1}[0],
			MaxReplicas: 1,
			Metrics: []autoscalingv1.MetricSpec{
				{
					Type: autoscalingv1.PodsMetricSourceType,
					Pods: &autoscalingv1.PodsMetricSource{
						MetricName:         "jobs_queued",
						TargetAverageValue: resource.MustParse("1k"),
					},
				},
			},
		},
	}
	var err error
	_, err = k8sClient.AppsV1().Deployments(testNamespace).Create(deployment)
	require.Nil(t, err)

	hpa, err = k8sClient.AutoscalingV2beta1().HorizontalPodAutoscalers(testNamespace).Create(hpa)
	require.Nil(t, err)

	tsids := []idtool.ID{idtool.ID(rand.Int63()), idtool.ID(rand.Int63())}
	for i, podName := range []string{"pod1", "pod2"} {
		fakeSignalFlow.AddTSIDMetadata(tsids[i], &messages.MetadataProperties{
			Metric: "jobs_queued",
			CustomProperties: map[string]string{
				"kubernetes_pod_name":  podName,
				"kubernetes_namespace": "default",
			},
		})
	}

	for i, val := range []float64{5, 10} {
		fakeSignalFlow.SetTSIDFloatData(tsids[i], val)
	}

	expectedProgram := fmt.Sprintf(`data("jobs_queued", filter=filter("app", "queue") and filter("kubernetes_namespace", "%s")).publish()`, testNamespace)
	fakeSignalFlow.AddProgramTSIDs(expectedProgram, tsids)

	var metricList []provider.CustomMetricInfo
	waitFor(5*time.Second, func() bool {
		metricList = prov.ListAllMetrics()
		found := false
		for i := range metricList {
			found = found || metricList[i].Metric == "jobs_queued"
		}
		return found
	})
	expectedCustomMetricInfo := provider.CustomMetricInfo{
		Metric:     "jobs_queued",
		Namespaced: true,
		GroupResource: schema.GroupResource{
			Group:    "",
			Resource: "pods",
		},
	}
	require.Contains(t, metricList, expectedCustomMetricInfo)

	get := func(metric string) (*custom_metrics.MetricValueList, error) {
		return prov.GetMetricBySelector(
			testNamespace,
			forceLabelSelector(&metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "queue",
				},
			}),
			provider.CustomMetricInfo{
				Metric: metric,
				GroupResource: schema.GroupResource{
					Resource: "pods",
				}},
			nil)
	}

	var metrics *custom_metrics.MetricValueList
	waitFor(5*time.Second, func() bool {
		metrics, err = get("jobs_queued")
		return err == nil && len(metrics.Items) > 0 && fakeSignalFlow.RunningJobsForProgram(expectedProgram) == 1
	})
	require.Nil(t, err)

	require.Len(t, metrics.Items, 2)
	sort.Slice(metrics.Items, func(i, j int) bool {
		return metrics.Items[i].DescribedObject.Name < metrics.Items[j].DescribedObject.Name
	})

	require.Equal(t, metrics.Items[0].Metric, custom_metrics.MetricIdentifier{
		Name: "jobs_queued",
	})
	require.Equal(t, metrics.Items[0].DescribedObject, custom_metrics.ObjectReference{
		Kind:       "Pod",
		Namespace:  testNamespace,
		Name:       "pod1",
		APIVersion: "v1",
	})
	require.Equal(t, metrics.Items[0].Value, resource.MustParse("5000m"))

	require.Equal(t, metrics.Items[1].DescribedObject, custom_metrics.ObjectReference{
		Kind:       "Pod",
		Namespace:  testNamespace,
		Name:       "pod2",
		APIVersion: "v1",
	})
	require.Equal(t, metrics.Items[1].Value, resource.MustParse("10000m"))

	// MODIFY THE HPA

	tsids = []idtool.ID{idtool.ID(rand.Int63()), idtool.ID(rand.Int63())}
	for i, podName := range []string{"pod1", "pod2"} {
		fakeSignalFlow.AddTSIDMetadata(tsids[i], &messages.MetadataProperties{
			Metric: "jobs_processed",
			CustomProperties: map[string]string{
				"kubernetes_pod_name":  podName,
				"kubernetes_namespace": "default",
			},
		})
	}

	for i, val := range []float64{15, 20} {
		fakeSignalFlow.SetTSIDFloatData(tsids[i], val)
	}

	newExpectedProgram := fmt.Sprintf(`data("jobs_processed", filter=filter("app", "queue") and filter("kubernetes_namespace", "%s")).publish()`, testNamespace)
	fakeSignalFlow.AddProgramTSIDs(newExpectedProgram, tsids)

	// Change the HPA metric and make sure it stops the old job and starts the
	// new one.
	hpa, err = updateHPA(k8sClient, hpa.Name, func(hpa *autoscalingv1.HorizontalPodAutoscaler) {
		hpa.Spec.Metrics[0].Pods.MetricName = "jobs_processed"
	})
	require.Nil(t, err)

	require.True(t, waitFor(5*time.Second, func() bool {
		// Should stop the old job
		return fakeSignalFlow.RunningJobsForProgram(expectedProgram) == 0 &&
			fakeSignalFlow.RunningJobsForProgram(newExpectedProgram) == 1
	}))

	waitFor(5*time.Second, func() bool {
		metrics, err = get("jobs_processed")
		return err == nil && len(metrics.Items) > 0 && fakeSignalFlow.RunningJobsForProgram(expectedProgram) == 1
	})
	require.Nil(t, err)

	require.Len(t, metrics.Items, 2)
	require.Equal(t, metrics.Items[0].Metric, custom_metrics.MetricIdentifier{
		Name: "jobs_processed",
	})
	require.Equal(t, metrics.Items[0].DescribedObject, custom_metrics.ObjectReference{
		Kind:       "Pod",
		Namespace:  testNamespace,
		Name:       "pod1",
		APIVersion: "v1",
	})
	require.Equal(t, metrics.Items[0].Value, resource.MustParse("15000m"))

	require.Equal(t, metrics.Items[1].DescribedObject, custom_metrics.ObjectReference{
		Kind:       "Pod",
		Namespace:  testNamespace,
		Name:       "pod2",
		APIVersion: "v1",
	})
	require.Equal(t, metrics.Items[1].Value, resource.MustParse("20000m"))

	expectedCustomMetricInfo = provider.CustomMetricInfo{
		Metric:     "jobs_processed",
		Namespaced: true,
		GroupResource: schema.GroupResource{
			Group:    "",
			Resource: "pods",
		},
	}
	metricList = prov.ListAllMetrics()
	require.Contains(t, metricList, expectedCustomMetricInfo)

	fakeSignalFlow.RemoveTSIDData(tsids[0])

	require.True(t, waitFor(10*time.Second, func() bool {
		metrics, err = get("jobs_processed")
		return len(metrics.Items) == 1
	}))
	require.Nil(t, err)

	// DELETE THE HPA

	err = k8sClient.AutoscalingV2beta1().HorizontalPodAutoscalers(testNamespace).Delete(hpa.Name, &metav1.DeleteOptions{})
	require.Nil(t, err)

	require.True(t, waitFor(5*time.Second, func() bool {
		// Should stop the old job
		return fakeSignalFlow.RunningJobsForProgram(expectedProgram) == 0 &&
			fakeSignalFlow.RunningJobsForProgram(newExpectedProgram) == 0
	}))

	require.NotContains(t, prov.ListAllMetrics(), expectedCustomMetricInfo)
}

func TestObjectMetrics(t *testing.T) {
	prov, k8sClient, fakeSignalFlow, cancel := providerWithBackend(t)
	defer cancel()

	deployment := commonDeployment
	hpa := &autoscalingv1.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name: "myapp",
			Annotations: map[string]string{
				"signalfx.com.custom.metrics": "",
				//"signalfx.com.external.metric/cpu": `data("cpu.utilization").publish()`,
			},
		},
		Spec: autoscalingv1.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
				Kind:       "Deployment",
				Name:       "demo-deployment",
				APIVersion: "apps/v1",
			},
			MinReplicas: &[]int32{1}[0],
			MaxReplicas: 1,
			Metrics: []autoscalingv1.MetricSpec{
				{
					Type: autoscalingv1.ObjectMetricSourceType,
					Object: &autoscalingv1.ObjectMetricSource{
						Target: autoscalingv1.CrossVersionObjectReference{
							Kind:       "Service",
							Name:       "queue_service",
							APIVersion: "v1",
						},
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"verb": "POST",
							},
						},
						MetricName:  "requests_queued",
						TargetValue: resource.MustParse("1k"),
					},
				},
			},
		},
	}
	_, err := k8sClient.AppsV1().Deployments(testNamespace).Create(deployment)
	require.Nil(t, err)

	_, err = k8sClient.AutoscalingV2beta1().HorizontalPodAutoscalers(testNamespace).Create(hpa)
	require.Nil(t, err)

	tsid := idtool.ID(rand.Int63())
	fakeSignalFlow.AddTSIDMetadata(tsid, &messages.MetadataProperties{
		Metric: "jobs_queued",
		CustomProperties: map[string]string{
			"kubernetes_namespace": "default",
		},
	})

	fakeSignalFlow.SetTSIDFloatData(tsid, 500.0)

	expectedProgram := fmt.Sprintf(`data("requests_queued", filter=filter("kubernetes_name", "queue_service") and filter("verb", "POST") and filter("kubernetes_namespace", "%s")).publish()`, testNamespace)
	fakeSignalFlow.AddProgramTSIDs(expectedProgram, []idtool.ID{tsid})

	var metric *custom_metrics.MetricValue
	waitFor(5*time.Second, func() bool {
		metric, err = prov.GetMetricByName(
			types.NamespacedName{
				Namespace: testNamespace,
				Name:      "queue_service",
			},
			provider.CustomMetricInfo{
				Metric: "requests_queued",
				GroupResource: schema.GroupResource{
					Resource: "services",
				}},
			forceLabelSelector(&metav1.LabelSelector{
				MatchLabels: map[string]string{
					"verb": "POST",
				},
			}))
		return err == nil
	})
	require.Nil(t, err)

	require.Equal(t, metric.Metric, custom_metrics.MetricIdentifier{
		Name: "requests_queued",
	})
	require.Equal(t, metric.DescribedObject, custom_metrics.ObjectReference{
		Kind:       "Service",
		Namespace:  testNamespace,
		Name:       "queue_service",
		APIVersion: "v1",
	})
	require.Equal(t, metric.Value, resource.MustParse("500000m"))
}

func TestExternalMetrics(t *testing.T) {
	prov, k8sClient, fakeSignalFlow, cancel := providerWithBackend(t)
	defer cancel()

	targetValue := resource.MustParse("500m")

	deployment := commonDeployment
	hpa := &autoscalingv1.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name: "myapp",
			Annotations: map[string]string{
				"signalfx.com.external.metric/cputest": `data("cpu.utilization").publish()`,
			},
		},
		Spec: autoscalingv1.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
				Kind:       "Deployment",
				Name:       "demo-deployment",
				APIVersion: "apps/v1",
			},
			MinReplicas: &[]int32{1}[0],
			MaxReplicas: 1,
			Metrics: []autoscalingv1.MetricSpec{
				{
					Type: autoscalingv1.ExternalMetricSourceType,
					External: &autoscalingv1.ExternalMetricSource{
						MetricName: "cputest",
						MetricSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"app": "myapp",
							},
						},
						TargetValue: &targetValue,
					},
				},
			},
		},
	}

	tsid := idtool.ID(rand.Int63())
	fakeSignalFlow.AddTSIDMetadata(tsid, &messages.MetadataProperties{
		Metric: "cputest",
		CustomProperties: map[string]string{
			"kubernetes_namespace": "default",
		},
	})

	fakeSignalFlow.SetTSIDFloatData(tsid, 500.0)

	expectedProgram := `data("cpu.utilization").publish()`
	fakeSignalFlow.AddProgramTSIDs(expectedProgram, []idtool.ID{tsid})

	_, err := k8sClient.AppsV1().Deployments(testNamespace).Create(deployment)
	require.Nil(t, err)

	_, err = k8sClient.AutoscalingV2beta1().HorizontalPodAutoscalers(testNamespace).Create(hpa)
	require.Nil(t, err)

	var metric *external_metrics.ExternalMetricValueList
	waitFor(5*time.Second, func() bool {
		metric, err = prov.GetExternalMetric(
			testNamespace,
			forceLabelSelector(&metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "myapp",
				},
			}),
			provider.ExternalMetricInfo{
				Metric: "cputest",
			})
		return err == nil && len(metric.Items) > 0
	})
	require.Nil(t, err)
	require.Len(t, metric.Items, 1)

	require.Equal(t, metric.Items[0].MetricName, "cputest")
	require.Equal(t, metric.Items[0].Value, resource.MustParse("500000m"))

	metricList := prov.ListAllExternalMetrics()
	expectedExternalMetricInfo := provider.ExternalMetricInfo{
		Metric: "cputest",
	}
	require.Contains(t, metricList, expectedExternalMetricInfo)
}

func TestCustomMetricWhitelist(t *testing.T) {
	prov, k8sClient, fakeSignalFlow, cancel := providerWithBackend(t)
	defer cancel()

	deployment := commonDeployment
	hpa := &autoscalingv1.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name: "myapp",
			Annotations: map[string]string{
				"signalfx.com.custom.metrics": "metricA,metricB",
			},
		},
		Spec: autoscalingv1.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
				Kind:       "Deployment",
				Name:       "demo-deployment",
				APIVersion: "apps/v1",
			},
			MinReplicas: &[]int32{1}[0],
			MaxReplicas: 1,
			Metrics: []autoscalingv1.MetricSpec{
				{
					Type: autoscalingv1.PodsMetricSourceType,
					Pods: &autoscalingv1.PodsMetricSource{
						MetricName:         "metricA",
						TargetAverageValue: resource.MustParse("1k"),
					},
				},
				{
					Type: autoscalingv1.PodsMetricSourceType,
					Pods: &autoscalingv1.PodsMetricSource{
						MetricName:         "metricB",
						TargetAverageValue: resource.MustParse("1k"),
					},
				},
				{
					Type: autoscalingv1.PodsMetricSourceType,
					Pods: &autoscalingv1.PodsMetricSource{
						MetricName:         "metricC",
						TargetAverageValue: resource.MustParse("1k"),
					},
				},
				{
					Type: autoscalingv1.PodsMetricSourceType,
					Pods: &autoscalingv1.PodsMetricSource{
						MetricName:         "metricD",
						TargetAverageValue: resource.MustParse("1k"),
					},
				},
			},
		},
	}

	var err error
	_, err = k8sClient.AppsV1().Deployments(testNamespace).Create(deployment)
	require.Nil(t, err)

	_, err = k8sClient.AutoscalingV2beta1().HorizontalPodAutoscalers(testNamespace).Create(hpa)
	require.Nil(t, err)

	tsids := []idtool.ID{idtool.ID(rand.Int63()), idtool.ID(rand.Int63())}
	for i, podName := range []string{"pod1", "pod2"} {
		fakeSignalFlow.AddTSIDMetadata(tsids[i], &messages.MetadataProperties{
			Metric: "jobs_queued",
			CustomProperties: map[string]string{
				"kubernetes_pod_name":  podName,
				"kubernetes_namespace": "default",
			},
		})
	}

	for i, val := range []float64{5, 10} {
		fakeSignalFlow.SetTSIDFloatData(tsids[i], val)
	}

	metricAProgram := fmt.Sprintf(`data("metricA", filter=filter("app", "queue") and filter("kubernetes_namespace", "%s")).publish()`, testNamespace)
	metricBProgram := fmt.Sprintf(`data("metricB", filter=filter("app", "queue") and filter("kubernetes_namespace", "%s")).publish()`, testNamespace)
	fakeSignalFlow.AddProgramTSIDs(metricAProgram, tsids[0:1])
	fakeSignalFlow.AddProgramTSIDs(metricBProgram, tsids[1:2])

	var metrics *custom_metrics.MetricValueList
	waitFor(5*time.Second, func() bool {
		metrics, err = prov.GetMetricBySelector(
			testNamespace,
			forceLabelSelector(&metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "queue",
				},
			}),
			provider.CustomMetricInfo{
				Metric: "metricA",
				GroupResource: schema.GroupResource{
					Resource: "pods",
				}},
			nil)
		return err == nil && len(metrics.Items) > 0 && fakeSignalFlow.RunningJobsForProgram(metricAProgram) == 1
	})
	require.Nil(t, err)

	metricList := prov.ListAllMetrics()
	expectedCustomMetricInfos := []provider.CustomMetricInfo{
		{
			Metric:     "metricA",
			Namespaced: true,
			GroupResource: schema.GroupResource{
				Group:    "",
				Resource: "pods",
			},
		},
		{
			Metric:     "metricB",
			Namespaced: true,
			GroupResource: schema.GroupResource{
				Group:    "",
				Resource: "pods",
			},
		},
	}
	require.Subset(t, metricList, expectedCustomMetricInfos)

	require.NotSubset(t, metricList, []provider.CustomMetricInfo{
		{
			Metric:     "metricC",
			Namespaced: true,
			GroupResource: schema.GroupResource{
				Group:    "",
				Resource: "pods",
			},
		},
		{
			Metric:     "metricD",
			Namespaced: true,
			GroupResource: schema.GroupResource{
				Group:    "",
				Resource: "pods",
			},
		},
	})

	require.Len(t, metrics.Items, 1)
	require.Equal(t, metrics.Items[0].Metric, custom_metrics.MetricIdentifier{
		Name: "metricA",
	})
	require.Equal(t, metrics.Items[0].DescribedObject, custom_metrics.ObjectReference{
		Kind:       "Pod",
		Namespace:  testNamespace,
		Name:       "pod1",
		APIVersion: "v1",
	})
	require.Equal(t, metrics.Items[0].Value, resource.MustParse("5000m"))
}

func TestRestartJobOnSignalFlowError(t *testing.T) {
	prov, k8sClient, fakeSignalFlow, cancel := providerWithBackend(t)
	defer cancel()

	targetValue := resource.MustParse("500m")

	deployment := commonDeployment
	hpa := &autoscalingv1.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name: "myapp",
			Annotations: map[string]string{
				"signalfx.com.external.metric/cputest": `data("cpu.utilization").publish()`,
			},
		},
		Spec: autoscalingv1.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
				Kind:       "Deployment",
				Name:       "demo-deployment",
				APIVersion: "apps/v1",
			},
			MinReplicas: &[]int32{1}[0],
			MaxReplicas: 1,
			Metrics: []autoscalingv1.MetricSpec{
				{
					Type: autoscalingv1.ExternalMetricSourceType,
					External: &autoscalingv1.ExternalMetricSource{
						MetricName: "cputest",
						MetricSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"app": "myapp",
							},
						},
						TargetValue: &targetValue,
					},
				},
			},
		},
	}

	tsid := idtool.ID(rand.Int63())
	fakeSignalFlow.AddTSIDMetadata(tsid, &messages.MetadataProperties{
		Metric: "cputest",
		CustomProperties: map[string]string{
			"kubernetes_namespace": "default",
		},
	})

	fakeSignalFlow.SetTSIDFloatData(tsid, 500.0)

	expectedProgram := `data("cpu.utilization").publish()`
	fakeSignalFlow.AddProgramTSIDs(expectedProgram, []idtool.ID{tsid})

	_, err := k8sClient.AppsV1().Deployments(testNamespace).Create(deployment)
	require.Nil(t, err)

	_, err = k8sClient.AutoscalingV2beta1().HorizontalPodAutoscalers(testNamespace).Create(hpa)
	require.Nil(t, err)

	var metric *external_metrics.ExternalMetricValueList
	waitFor(5*time.Second, func() bool {
		metric, err = prov.GetExternalMetric(
			testNamespace,
			forceLabelSelector(&metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "myapp",
				},
			}),
			provider.ExternalMetricInfo{
				Metric: "cputest",
			})
		return err == nil && len(metric.Items) > 0
	})
	require.Nil(t, err)
	require.Len(t, metric.Items, 1)

	fakeSignalFlow.KillExistingConnections()

	// Change the metric values to prove the job runner actually reconnected.
	fakeSignalFlow.SetTSIDFloatData(tsid, 1000.0)

	require.True(t, waitFor(timeout*2, func() bool {
		metric, err = prov.GetExternalMetric(
			testNamespace,
			forceLabelSelector(&metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "myapp",
				},
			}),
			provider.ExternalMetricInfo{
				Metric: "cputest",
			})
		return len(metric.Items) == 1 && metric.Items[0].Value == resource.MustParse("1000000m")
	}))
}

func TestMultipleHPAs(t *testing.T) {
	prov, k8sClient, fakeSignalFlow, cancel := providerWithBackend(t)
	defer cancel()

	numHPAs := 10

	var expectedPrograms []string
	//var hpas []*autoscalingv1.HorizontalPodAutoscaler
	for i := 0; i < numHPAs; i++ {
		deployment := commonDeployment.DeepCopy()
		deployment.Name = fmt.Sprintf("deploy-%d", i)
		appLabelValue := fmt.Sprintf("app%d", i)
		deployment.Spec.Selector.MatchLabels = map[string]string{
			"app": appLabelValue,
		}
		deployment.Spec.Template.ObjectMeta.Labels = deployment.Spec.Selector.MatchLabels

		hpa := &autoscalingv1.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("myapp%d", i),
				Annotations: map[string]string{
					"signalfx.com.custom.metrics": "",
				},
			},
			Spec: autoscalingv1.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
					Kind:       "Deployment",
					Name:       deployment.Name,
					APIVersion: "apps/v1",
				},
				MinReplicas: &[]int32{1}[0],
				MaxReplicas: 1,
				Metrics: []autoscalingv1.MetricSpec{
					{
						Type: autoscalingv1.PodsMetricSourceType,
						Pods: &autoscalingv1.PodsMetricSource{
							MetricName:         "jobs_queued",
							TargetAverageValue: resource.MustParse("1k"),
						},
					},
				},
			},
		}
		_, err := k8sClient.AppsV1().Deployments(testNamespace).Create(deployment)
		require.Nil(t, err)

		_, err = k8sClient.AutoscalingV2beta1().HorizontalPodAutoscalers(testNamespace).Create(hpa)
		require.Nil(t, err)

		tsids := []idtool.ID{idtool.ID(rand.Int63()), idtool.ID(rand.Int63())}
		for i, podName := range []string{deployment.Name + "-pod1", deployment.Name + "-pod2"} {
			fakeSignalFlow.AddTSIDMetadata(tsids[i], &messages.MetadataProperties{
				Metric: "jobs_queued",
				CustomProperties: map[string]string{
					"kubernetes_pod_name":  podName,
					"kubernetes_namespace": "default",
				},
			})
		}

		for i, val := range []float64{float64(i), float64(i + 1)} {
			fakeSignalFlow.SetTSIDFloatData(tsids[i], val)
		}

		expectedPrograms = append(expectedPrograms,
			fmt.Sprintf(`data("jobs_queued", filter=filter("app", "%s") and filter("kubernetes_namespace", "%s")).publish()`, appLabelValue, testNamespace))
		fakeSignalFlow.AddProgramTSIDs(expectedPrograms[i], tsids)

	}

	for i := 0; i < numHPAs; i++ {
		var metrics *custom_metrics.MetricValueList
		var err error
		waitFor(timeout*2, func() bool {
			metrics, err = prov.GetMetricBySelector(
				testNamespace,
				forceLabelSelector(&metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": fmt.Sprintf("app%d", i),
					},
				}),
				provider.CustomMetricInfo{
					Metric: "jobs_queued",
					GroupResource: schema.GroupResource{
						Resource: "pods",
					}},
				nil)
			return err == nil && len(metrics.Items) > 0 && fakeSignalFlow.RunningJobsForProgram(expectedPrograms[i]) == 1
		})
		require.Nil(t, err)

		require.Len(t, metrics.Items, 2)
		sort.Slice(metrics.Items, func(i, j int) bool {
			return metrics.Items[i].DescribedObject.Name < metrics.Items[j].DescribedObject.Name
		})

		require.Equal(t, metrics.Items[0].Metric, custom_metrics.MetricIdentifier{
			Name: "jobs_queued",
		})
		require.Equal(t, metrics.Items[0].DescribedObject, custom_metrics.ObjectReference{
			Kind:       "Pod",
			Namespace:  testNamespace,
			Name:       fmt.Sprintf("deploy-%d-pod1", i),
			APIVersion: "v1",
		})
		require.Equal(t, metrics.Items[0].Value, resource.MustParse(fmt.Sprintf("%d000m", i)))

		require.Equal(t, metrics.Items[1].DescribedObject, custom_metrics.ObjectReference{
			Kind:       "Pod",
			Namespace:  testNamespace,
			Name:       fmt.Sprintf("deploy-%d-pod2", i),
			APIVersion: "v1",
		})
		require.Equal(t, metrics.Items[1].Value, resource.MustParse(fmt.Sprintf("%d000m", i+1)))
	}

	// Only one instance of the custom metric should be present for a single
	// metric name per namespace.
	metricList := prov.ListAllMetrics()
	found := false
	for i := range metricList {
		if metricList[i].Metric == "jobs_queued" {
			require.False(t, found, "should only have one jobs_queued metric")
			found = true
		}
	}

	expectedCustomMetricInfo := provider.CustomMetricInfo{
		Metric:     "jobs_queued",
		Namespaced: true,
		GroupResource: schema.GroupResource{
			Group:    "",
			Resource: "pods",
		},
	}
	require.Contains(t, metricList, expectedCustomMetricInfo)
}

func waitFor(timeout time.Duration, f func() bool) bool {
	timer := time.NewTimer(timeout)
	for {
		success := f()
		if success {
			return true
		}
		select {
		case <-timer.C:
			return false
		default:
			time.Sleep(1000 * time.Millisecond)
		}
	}
}

// Retries on 409 errors
func updateHPA(k8sClient kubernetes.Interface, name string, updater func(hpa *autoscalingv1.HorizontalPodAutoscaler)) (*autoscalingv1.HorizontalPodAutoscaler, error) {
	for {
		hpa, err := k8sClient.AutoscalingV2beta1().HorizontalPodAutoscalers(testNamespace).Get(name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}

		updater(hpa)

		newHPA, err := k8sClient.AutoscalingV2beta1().HorizontalPodAutoscalers(testNamespace).Update(hpa)
		if err != nil {
			if se, ok := err.(*apierrors.StatusError); ok {
				if se.Status().Code == 409 {
					continue
				}
			}
			return nil, err
		}
		return newHPA, nil
	}
}
