# SignalFx Kubernetes Metric Adapter Helm Chart

See [values.yaml] for a list of values to use to configure this chart.

To deploy, run the following:

`$ helm repo add signalfx https://dl.signalfx.com/helm-repo`

Then to ensure the latest state of the repository, run:

`$ helm repo update`

Then you can install the adapter using the chart name `signalfx/signalfx-k8s-metrics-adapter`.
`$ helm install --set imageTag=<ADAPTER_VERSION> --set accessToken=MY_ORG_ACCESS_TOKEN signalfx/signalfx-k8s-metrics-adapter`

You can see the adapter versions at
https://github.com/signalfx/signalfx-k8s-metrics-adapter/releases.

### Configuring the stream endpoint

In order for the adapter to communicate with SignalFx, we need to make sure you connect to the correct SignalFx realm.
To determine what realm you are in, check your profile page in the SignalFx web application (click the avatar in the upper right and click My Profile).

If you are not in the `us0` realm, you will need to configure the `streamURL` configuration option shown below to include your realm in the URL.

`$ helm install --set imageTag=<ADAPTER_VERSION> --set accessToken=MY_ORG_ACCESS_TOKEN --set streamURL=wss://stream.<REALM>.signalfx.com/v2/signalflow signalfx/k8s-metrics-adapter`
