/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/signalfx/golib/datapoint"
	"github.com/signalfx/golib/sfxclient"
	"github.com/signalfx/signalfx-go/signalflow/v2"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/component-base/logs"
	"k8s.io/klog"

	"github.com/signalfx/signalfx-k8s-metrics-adapter/internal"
	basecmd "sigs.k8s.io/custom-metrics-apiserver/pkg/cmd"
	"sigs.k8s.io/custom-metrics-apiserver/pkg/provider"

	// Get extra auth methods for kube client
	_ "k8s.io/client-go/plugin/pkg/client/auth"
)

type SignalFxAdapter struct {
	basecmd.AdapterBase
	discoverer *internal.HPADiscoverer
	provider   *internal.SignalFxProvider
}

func (a *SignalFxAdapter) makeProviderOrDie(registry *internal.Registry) provider.MetricsProvider {
	k8sConf, err := a.ClientConfig()
	if err != nil {
		klog.Fatalf("could not get k8s client config: %v", err)
	}
	k8sClient, err := kubernetes.NewForConfig(k8sConf)
	if err != nil {
		klog.Fatalf("could not make k8s client: %v", err)
	}

	mapper, err := a.RESTMapper()
	if err != nil {
		klog.Fatalf("unable to construct discovery REST mapper: %v", err)
	}

	a.discoverer = internal.NewHPADiscoverer(k8sClient, registry.HandleHPAUpdated, registry.HandleHPADeleted, mapper)
	go a.discoverer.Discover(context.Background())

	a.provider = internal.NewSignalFxProvider(registry, mapper)
	return a.provider
}

func (a *SignalFxAdapter) InternalMetrics() []*datapoint.Datapoint {
	var out []*datapoint.Datapoint
	out = append(out, a.discoverer.InternalMetrics()...)
	out = append(out, a.provider.InternalMetrics()...)
	return out
}

func makeSignalFlowClient(streamURL, realm, accessToken string) (*signalflow.Client, error) {
	klog.Infof("Using SignalFlow server %s", streamURL)

	options := []signalflow.ClientParam{
		signalflow.AccessToken(accessToken),
	}

	if streamURL != "" {
		options = append(options, signalflow.StreamURL(streamURL))
	} else if realm != "" {
		options = append(options, signalflow.StreamURLForRealm(realm))
	}

	return signalflow.NewClient(options...)
}

func main() {
	logs.InitLogs()
	defer logs.FlushLogs()

	cmd := &SignalFxAdapter{}

	var streamURL string
	cmd.Flags().StringVar(&streamURL, "stream-url", "",
		"URL of the SignalFx SignalFlow websocket server that serves the metrics.  Takes precident over the 'realm' option if provided.")
	var realm string
	cmd.Flags().StringVar(&realm, "signalfx-realm", "us0",
		"The SignalFx realm in which your organization lives.  Ignored if -stream-url is provided")
	var internalMetricsPort int
	cmd.Flags().IntVar(&internalMetricsPort, "internal-metrics-port", 9100,
		"The port at which to expose internal metrics about this adapter -- these metrics can be scraped by the SignalFx Smart Agent")

	var minimumTimeseriesExpiry time.Duration
	cmd.Flags().DurationVar(&minimumTimeseriesExpiry, "minimum-ts-expiry", 30*time.Second, "The minimum duration in which no data is received for a timeseries before that timeseries is expired and no longer reports data to K8s.  The default is 3 times the reported data resolution in the SignalFlow job.")

	var metadataTimeout time.Duration
	cmd.Flags().DurationVar(&metadataTimeout, "metadata-timeout", 10*time.Second, "The amount of time to wait for a submitted job to receive its metadata from the server.")

	klog.InitFlags(nil) // initalize klog flags

	cmd.Flags().AddGoFlagSet(flag.CommandLine) // make sure we get the klog flags
	err := cmd.Flags().Parse(os.Args)
	if err != nil {
		log.Fatalf("Failed to parse flags: %v", err)
	}

	// This should usually be provided by a secret mapped to an envvar.
	accessToken := os.Getenv("SIGNALFX_ACCESS_TOKEN")

	if os.Getenv("ENABLE_PROFILING") != "" {
		startProfileServer()
	}

	flowClient, err := makeSignalFlowClient(streamURL, realm, accessToken)
	if err != nil {
		klog.Fatalf("Error creating SignalFlow client: %v", err)
		return
	}

	jobRunner := internal.NewSignalFlowJobRunner(flowClient)
	jobRunner.MetadataTimeout = metadataTimeout
	jobRunner.MinimumTimeseriesExpiry = minimumTimeseriesExpiry
	go jobRunner.Run(context.Background())

	registry := internal.NewRegistry(jobRunner)

	provider := cmd.makeProviderOrDie(registry)
	cmd.WithCustomMetrics(provider)
	cmd.WithExternalMetrics(provider)

	serveInternalMetrics(internalMetricsPort, sfxclient.CollectorFunc(cmd.InternalMetrics))

	go func() {
		// Open port for POSTing fake metrics
		klog.Fatal(http.ListenAndServe(":8080", nil))
	}()
	if err := cmd.Run(wait.NeverStop); err != nil {
		klog.Fatalf("unable to run custom metrics adapter: %v", err)
	}
}

// TODO: move this to a common library in golib
func serveInternalMetrics(port int, collectors ...sfxclient.Collector) {
	handler := func(rw http.ResponseWriter, req *http.Request) {
		out := []*datapoint.Datapoint{}
		for _, coll := range collectors {
			out = append(out, coll.Datapoints()...)
		}
		jsonOut, err := json.Marshal(out)
		if err != nil {
			klog.Errorf("Could not serialize internal metrics to JSON: %v", err)
			rw.WriteHeader(500)
			_, _ = rw.Write([]byte(err.Error()))
			return
		}

		rw.Header().Add("Content-Type", "application/json")
		rw.WriteHeader(200)

		_, _ = rw.Write(jsonOut)
	}

	server := &http.Server{
		Addr:         fmt.Sprintf("0.0.0.0:%d", port),
		Handler:      http.HandlerFunc(handler),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 0, //5 * time.Second,
	}

	go func() {
		klog.Infof("Serving internal metrics at 0.0.0.0:%d", port)
		err := server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			klog.Errorf("Problem with internal metrics server: %v", err)
		}
	}()
}

func startProfileServer() {
	// We don't use that much memory so the default mem sampling rate is
	// too small to be very useful. Setting to 1 profiles ALL allocations
	runtime.MemProfileRate = 1
	runtime.SetCPUProfileRate(-1)
	runtime.SetCPUProfileRate(2000)

	go func() {
		log.Println(http.ListenAndServe(fmt.Sprintf("0.0.0.0:9487"), nil))
	}()
}
