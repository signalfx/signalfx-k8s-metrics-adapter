# SignalFx Kubernetes Metric Adapter Helm Chart

See [values.yaml] for a list of values to use to configure this chart.

To deploy, run the following:

`$ helm repo add signalfx https://dl.signalfx.com/helm-repo`

Then to ensure the latest state of the repository, run:

`$ helm repo update`

Then you can install the agent using the chart name `signalfx/signalfx-agent`.
`$ helm install --set imageTag=<ADAPTER_VERSION> --set accessToken=MY_ORG_ACCESS_TOKEN signalfx/k8s-metrics-adapter`

You can see the adapter versions at
https://github.com/signalfx/signalfx-k8s-metrics-adapter/releases.
