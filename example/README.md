# HPA Example

Deploy the example resources in [./resources.yaml] to a Kubernetes cluster:

`kubectl apply -n hpa-example -f ./resources.yaml`

Once they are up and settled you will observe that the HPA has scaled the
Deployment to 1 replica, since there is no traffic to it.

Then run the following against the same Kubernetes cluster as the Deployment and
HPA to generate traffic:

```
$ kubectl run -i --tty load-gen2 --image=busybox --restart=Never /bin/sh
If you don't see a command prompt, try pressing enter.
/ # while true; do (wget -O -  http://myapp.webservers:80/test > /dev/null 2>&1 &) ; done
```
