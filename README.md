# SignalFx Kubernetes Metrics Adapter

This is a custom metrics adapter that allows using SignalFx metrics in a
[Kubernetes Horizontal Pod
Autoscaler (HPA)](https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/).
It currently supports both HPAs attached to Deployments and Stateful Sets.

**This is currently in beta and has not been thoroughly tested in production environments yet. Use with caution.**

It supports both _custom_ and _external_ metrics.  It pulls the metrics from
the SignalFx backend using the [SignalFlow Go client
library](https://github.com/signalfx/signalfx-go/tree/master/signalflow).

Metric names within a single HPA must be unique, as they are used by the
adapter as an identifier for that metric for simplicity sake.  For example, you
cannot have a metric of type `Pods` named the same thing as a metric of type
`Object`, etc.  Moreover, external metrics names **must be unique per
namespace** due to how Kubernetes queries the adapter for them.

## Example HPA

```yaml

apiVersion: autoscaling/v2beta1
kind: HorizontalPodAutoscaler
metadata:
  annotations:
    signalfx.com.custom.metrics: ""
    # Imagine the job_lag metric is derived external to the pods and is not
    # associated with any particular pod.
    signalfx.com.external.metric/job_lag_seconds: data("job_lag_seconds").publish()
  name: backend-proxy
  namespace: webservers
spec:
  maxReplicas: 10
  metrics:
  - pods:
      metricName: nginx_requests
      selector:
        matchLabels:
          plugin: nginx
      targetAverageValue: "100"
    type: Pods
  - external:
      metricName: job_lag_seconds
      targetAverageValue: "10"
    type: External
  minReplicas: 5
  scaleTargetRef:
    apiVersion: extensions/v1beta1
    kind: Deployment
    name: backend-proxy
```

## Custom Metrics
Custom metrics include both `Pod` metrics (those about individual pods) and
`Object` metrics (those about specific Kubernetes resources within the same
namespace as the HPA target).

To make the adapter serve metrics relevant to an HPA, you should add the
annotation `signalfx.com.custom.metrics: <whitelist>` to the HPA itself.  An
optional whitelist (the value of the annotation) can restrict the
SignalFx-backed custom metrics to only those specified (comma separated).  If
you want all custom metrics in the HPA to be backed by SignalFx, simply leave
the value of the annotation blank.  If this annotation is not provided, the
adapter will not serve any custom metrics relevant to the HPA.

### Pod Metrics
For `Pod` type metrics, the SignalFlow program used is derived like so:

```python
data("<Pod metric name>",
     filter=filter("<pod selector key 1>", "<pod selector value 1>")
     [and filter("<pod selector key n>", "<pod selector value n>")...]
     [and filter("<metric selector key n>", "<metric selector value n>")...]
     and filter("kubernetes_namespace", "<namespace of HPA>")
    ).publish()
```

Where `<pod selector key/value n>` is the set of pod label selectors on the
HPA's target resource.  `<metric selector key/value n>` are any metric
selectors provided on the Pod metric itself.

### Object Metrics
For `Object` type metrics (those that pertain to non-Pod Kuberetes resources
in the same namespace as the HPA), thw SignalFlow program used is derived like
so:

```python
data("<Object metric name>",
     filter=filter("kubernetes_name", "<Object.Target.Name>")
     [and filter("<metric selector key n>", "<metric selector value n>")...]
     and filter("kubernetes_namespace", "<namespace of HPA>")
    ).publish()
```
Where `<metric selector key/value n>` are any metric selectors provided on the
Object metric itself.  The `kubernetes_name` dimension is expected to exist on
the time series in SignalFx.  This might be configurable in a future release,
but if this is too inflexible for your use case, you can use External Metrics
instead.

### SignalFx Smart Agent Dependency
Custom metrics are dependent on the [`kubernetes-cluster`
monitor](https://github.com/signalfx/signalfx-agent/blob/master/docs/monitors/kubernetes-cluster.md)
in the SignalFx Smart Agent, especially for custom metrics in your HPAs.  The
main dependency is on the pod labels that that monitor puts on the
`kubernetes_pod_uid` dimension.  It also depends on timeseries having a
`kubernetes_pod_name` dimension so that the adapter can report back the correct
pod names to the autoscaler for Pod metrics.

Metric selectors are converted verbatim to SignalFlow `filter` clauses.


## External Metrics

[External metrics](https://v1-12.docs.kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale-walkthrough/#autoscaling-on-metrics-not-related-to-kubernetes-objects)
allow you to define arbitrary SignalFlow programs to get any metrics you want
from SignalFx.  The SignalFlow programs are defined in special annotations on
the HPA resource itself.  The annotations are of the form
`signalfx.com.external.metric/<metric name>: <signalflow program>`.  For
example, if you have an HPA on a work queue service and wanted to scale based
on the maximum size of the queue in any single worker pod:

```yaml
apiVersion: autoscaling/v2beta1
kind: HorizontalPodAutoscaler
metadata:
  annotations:
    signalfx.com.external.metric/queue_size: data("myapp_queue_size").max().publish()
  name: backend-proxy
  namespace: webservers
spec:
  maxReplicas: 10
  metrics:
  - external:
      metricName: queue_size
      targetValue: "500"
    type: External
  minReplicas: 5
  scaleTargetRef:
    apiVersion: extensions/v1beta1
    kind: Deployment
    name: backend-proxy
```

External metrics have no dependencies on any of the dimensions/properties
managed by the `kubernetes-cluster` monitor, and thus can be used without the
Smart Agent deployed in the cluster.

**External metric names must be unique per namespace due to how Kubernetes
queries for them.**  You can technically reuse external metric names within a
single namespace by providing unique metric selectors to the metrics in the HPA
spec, but this is not recommended as it is confusing.

## Installation

This adapter should be installed within the cluster that you want to use it
for.  It can go in any namespace, as it serves metrics across the entire
cluster.

The simplest way to install is via the [Helm Chart](./deployments/helm/signalfx-k8s-metrics-adatper).

#### Configuring the stream endpoint

In order for the adapter to communicate with SignalFx, we need to make sure you connect to the correct SignalFx realm.
To determine what realm you are in, check your profile page in the SignalFx web application (click the avatar in the upper right and click My Profile).

If you are not in the `us0` realm, you will need to configure the `streamURL` configuration option shown below to include your realm in the URL when installing the adapter:

`streamURL: wss://stream.<REALM>.signalfx.com/v2/signalflow`


If you are running this as a standalone app, not installed via helm, you can pass the the realm in as a command line argument when starting the adapter:

`./adapter --signalfx-realm us0`
or
`./adapter --stream-url wss://stream.<REALM>.signalfx.com/v2/signalflow`


## Development

To run the server locally against a K8s cluster you have configured in a local
kubeconfig file:

`./adapter --lister-kubeconfig ~/.kube/cluster-1 --authentication-kubeconfig ~/.kube/cluster-1 --authorization-kubeconfig ~/.kube/cluster-1 --secure-port 6443 --bind-address 127.0.0.1 --signalfx-realm us0`

### Integration Tests
The main form of automated testing for this project is integration tests that
run against a real K8s cluster.  It will by default use the same cluster that
`kubectl` would use on that shell, so set `KUBECONFIG` and the current context
appropriately before running.

### Release Checklist
 - Tag new version as `v<new_version`.
 - Push `quay.io/signalfx/k8s-metrics-adapter:<new_version>` to Quay.io repo
 - Make Github release describing changes

## Helpful Resources

[https://github.com/kubernetes-incubator/custom-metrics-apiserver] -- The
library that this adapter uses underneath to handle most of the metrics API
server boilerplate.

[https://github.com/zalando-incubator/kube-metrics-adapter] -- An adapter that
supports many different metric sources.  The annotation config method of the
SignalFx adapter was inspired from that project.

[Custom Metrics API](https://github.com/kubernetes/community/blob/master/contributors/design-proposals/instrumentation/custom-metrics-api.md) - The design doc from Kubernetes for the custom metrics API.

## TODO

 - Support non-standard dimensions/properties for pod/object metrics
 - Get CircleCI setup
