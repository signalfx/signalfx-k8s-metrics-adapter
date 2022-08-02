# HPA Example

## Disclaimer
This example is validated using the latest SignalFX HPA Adapter on kubernetes v1.20. <br/>
The adapter implements the autoscaling api `v2beta1`, this version is not supported in kubernetes v1.22 and above. <br/>
We'll also be using Splunk OpenTelemetry distribution to emit app metrics to our backend

## Summary
In this example, we'll be using nginx to showcase the SignalFx HPA adapter. <br/>
The goal is for HPA to monitor the number of http requests `nginx_requests` and to automatically add extra nginx pods 
when the original instance is under load. <br/>
We'll be covering all the supported HPA configurations; Custom Metrics (Pods & Object Metrics) and External Metrics.

## Example

### Setup

Create a namespace where our test app will reside

```
kubectl create namespace hpa-example
```

Deploy the test app [webservers](./webservers.yaml) to a supported Kubernetes cluster:

```
kubectl apply -n hpa-example -f ./webservers.yaml
```

Deploy Splunk OTEL. This example [values](./values.yaml) configure the OTEL agent to detect and auto create nginx receivers

```
helm install --values values.yaml --set splunkObservability.accessToken='<ingest_token>' --set clusterName='<cluster_name>' --set splunkObservability.realm='<org_realm>' --set gateway.enabled='false' --generate-name splunk-otel-collector-chart/splunk-otel-collector
```

Install the HPA adapter

```
helm install --set imageTag=0.5.0 --set accessToken=<api_token> --set streamURL=wss://stream.<org_realm>.signalfx.com/v2/signalflow signalfx/signalfx-k8s-metrics-adapter --generate-name
```

### Test

##### 1. Object Custom Metrics

- Deploy the [Object Custom Metrics HPA](./custom-object-hpa.yaml)
```
kubectl apply -f ./custom-object-hpa.yaml -n hpa-example
```
- Generate load using these [steps](#generate-load)
- Validate that HPA is pulling the correct metric value
```
kubectl describe hpa -n hpa-example
```
- You can also run the API straight against the adapter to verify that it returns the correct metrics/timeseries
```
kubectl get --raw "/apis/custom.metrics.k8s.io/v1beta2/namespaces/hpa-example/services/webservers/nginx_requests?metricLabelSelector=plugin%3Dnginx"
```
- Stop the load, wait ~5 minutes (default k8s scale back value) and the number of PODs should scale back down to 1
- Cleanup
```
kubectl delete -f ./custom-object-hpa.yaml -n hpa-example
```

##### 2. POD Custom Metrics

- Deploy the [POD Custom Metrics HPA](./custom-pod-hpa.yaml)
```
kubectl apply -f ./custom-pod-hpa.yaml -n hpa-example
```
- Generate load using these [steps](#generate-load) 
- Validate that HPA is pulling the correct metric value
```
kubectl describe hpa -n hpa-example
```
- You can also run the API straight against the adapter to verify that it returns the correct metrics/timeseries
```
kubectl get --raw "/apis/custom.metrics.k8s.io/v1beta2/namespaces/hpa-example/pods/%2A/nginx_requests?labelSelector=app%3Dwebservers&metricLabelSelector=plugin%3Dnginx"
```
**Note**: We've noticed that some k8s versions were scaling even when the metric value returned by the adapter was below the target (1). 
This looks like a bug in some versions of k8s and specific to POD Custom Metrics
- Stop the load, wait ~5 minutes (default k8s scale back value) and the number of PODs should scale back down to 1
- Cleanup 
```
kubectl delete -f ./custom-pod-hpa.yaml -n hpa-example
```

##### 3. External Metrics

- Deploy the [External Metrics HPA](./external-hpa.yaml) 
(edit the filter and set the <cluster_name>) to be equal to the cluster name you have defined when installing OTEL
```
kubectl apply -f ./external-hpa.yaml -n hpa-example
```
- Generate load using these [steps](#generate-load)
- Validate that HPA is pulling the correct metric value
```
kubectl describe hpa -n hpa-example
```
- You can also run the API straight against the adapter to verify that it returns the correct metrics/timeseries
```
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/hpa-example/nginx_requests"
```
- Stop the load, wait ~5 minutes (default k8s scale back value) and the number of PODs should scale back down to 1
- Cleanup
```
kubectl delete -f ./external-hpa.yaml -n hpa-example
```

### Generate Load

Run the following against the same Kubernetes cluster as the Deployment and HPA to generate traffic:

```
$ kubectl run -i --tty load-gen2 --image=busybox --restart=Never /bin/sh
If you don't see a command prompt, try pressing enter.
/ # while true; do (wget -O -  http://webservers.hpa-example:80/test > /dev/null 2>&1 &) ; done
```
