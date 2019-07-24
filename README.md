# SignalFx Kubernetes Metrics Adapter

** THIS IS A WORK IN PROGRESS **  The repo is here for a placeholder and
initial work is happening in the `initial` branch.  When it is ready for use,
this README will be updated and a release will be made.

This is a custom metrics adapter that allows using SignalFx metrics in a
[Kubernetes Horizontal Pod
Autoscaler](https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/).

It supports both _custom_ and _external_ metrics.  It pulls the metrics from
the SignalFx backend using the [SignalFlow Go client
library](https://github.com/signalfx/signalfx-go/tree/master/signalflow).

