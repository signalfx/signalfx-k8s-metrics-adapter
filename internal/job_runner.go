package internal

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/signalfx/golib/datapoint"
	"github.com/signalfx/golib/sfxclient"
	"github.com/signalfx/signalfx-go/idtool"
	"github.com/signalfx/signalfx-go/signalflow"
	"github.com/signalfx/signalfx-go/signalflow/messages"
	"k8s.io/klog"
)

type TSIDValueMetadata struct {
	TSID       idtool.ID
	Val        float64
	Metadata   *messages.MetadataProperties
	Timestamp  time.Time
	Resolution time.Duration
}

// MetricSnapshot represents the latest state of received metric data from a
// SignalFlow job.
type MetricSnapshot map[idtool.ID]*TSIDValueMetadata

// PodNameForTSID returns the pod name that is associated with the given TSID,
// if any.
func (tvm *TSIDValueMetadata) PodName() string {
	meta := tvm.Metadata
	if meta == nil || meta.CustomProperties == nil {
		return ""
	}
	return meta.CustomProperties["kubernetes_pod_name"]
}

type newDataMsg struct {
	msg        *messages.DataMessage
	handle     string
	resolution time.Duration
}

type dataRequest struct {
	program string
	respCh  chan dataResponse
}

type dataResponse struct {
	snapshot MetricSnapshot
	err      error
}

type startedMsg struct {
	comp    *signalflow.Computation
	handle  string
	program string
}

// SignalFlowJobRunner manages SignalFlow jobs.
type SignalFlowJobRunner struct {
	ctx context.Context

	client *signalflow.Client

	jobsByProgram           map[string]*signalflow.Computation
	jobsByHandle            map[string]*signalflow.Computation
	metricSnapshotsByHandle map[string]MetricSnapshot

	startedCh     chan startedMsg
	stoppedCh     chan string
	dataCh        chan newDataMsg
	dataRequestCh chan dataRequest

	CleanupOldTSIDsInterval time.Duration
	MinimumTimeseriesExpiry time.Duration

	TotalJobsStarted int64
	TotalJobsStopped int64
	TotalJobsErrored int64
}

func NewSignalFlowJobRunner(client *signalflow.Client) *SignalFlowJobRunner {
	return &SignalFlowJobRunner{
		client:                  client,
		jobsByProgram:           make(map[string]*signalflow.Computation),
		jobsByHandle:            make(map[string]*signalflow.Computation),
		metricSnapshotsByHandle: make(map[string]MetricSnapshot),
		startedCh:               make(chan startedMsg),
		stoppedCh:               make(chan string),
		dataCh:                  make(chan newDataMsg),
		dataRequestCh:           make(chan dataRequest),
		CleanupOldTSIDsInterval: 30 * time.Second,
	}
}

func (jr *SignalFlowJobRunner) ReplaceOrStartJob(program string) error {
	comp, err := jr.client.Execute(&signalflow.ExecuteRequest{
		Program: program,
	})
	if err != nil {
		atomic.AddInt64(&jr.TotalJobsErrored, 1)
		return err
	}

	comp.MetadataTimeout = 10 * time.Second
	// Wait for the handle to come through before sending the message to block
	// the loop less.
	handle := comp.Handle()
	if handle == "" {
		return fmt.Errorf("Could not get job handle within timeout: %v", comp.Err())
	}

	jr.startedCh <- startedMsg{
		comp:    comp,
		handle:  handle,
		program: program,
	}

	klog.Infof("Started SignalFlow compuation: %s (handle: %s)", program, handle)
	atomic.AddInt64(&jr.TotalJobsStarted, 1)

	go jr.watchJob(comp, program)

	return nil
}

func (jr *SignalFlowJobRunner) StopJob(program string) {
	jr.stoppedCh <- program
}

// Run does everything in a single loop to prevent heavy churn on a mutex
// when there are a lot of jobs running and updating values, and also avoids
// the possiblity of data races.  This trades off mutexes for channels.
func (jr *SignalFlowJobRunner) Run(ctx context.Context) {
	jr.ctx = ctx

	cleanupTicker := time.NewTicker(jr.CleanupOldTSIDsInterval)

	for {
		select {

		case <-jr.ctx.Done():
			cleanupTicker.Stop()
			klog.Infof("SignalFlow job runner is stopping")
			return

		case startedMsg := <-jr.startedCh:
			jr.jobsByProgram[startedMsg.program] = startedMsg.comp
			jr.jobsByHandle[startedMsg.handle] = startedMsg.comp

		case stoppedProgram := <-jr.stoppedCh:
			comp := jr.jobsByProgram[stoppedProgram]
			if comp == nil {
				klog.Errorf("Trying to stop computation that was never registered: %s", stoppedProgram)
				continue
			}

			klog.Infof("Stopping SignalFlow compuation: %s", stoppedProgram)

			err := comp.Stop()
			if err != nil {
				klog.Errorf("Failed to stop SignalFlow job %s: %v", stoppedProgram, err)
			}
			atomic.AddInt64(&jr.TotalJobsStopped, 1)

			delete(jr.jobsByProgram, stoppedProgram)
			delete(jr.jobsByHandle, comp.Handle())

		case data := <-jr.dataCh:
			snapshot := jr.metricSnapshotsByHandle[data.handle]
			if snapshot == nil {
				snapshot = make(MetricSnapshot)
				jr.metricSnapshotsByHandle[data.handle] = snapshot
			}

			comp := jr.jobsByHandle[data.handle]
			if comp == nil {
				klog.Errorf("Could not find job for data %v", data)
				continue
			}

			for _, payload := range data.msg.Payloads {
				var val float64
				switch v := payload.Value().(type) {
				case float64:
					val = v
				case int64:
					val = float64(v)
				default:
					klog.Errorf("Metric from %v has unexpected type", data)
					continue
				}
				snapshot[payload.TSID] = &TSIDValueMetadata{
					TSID:       payload.TSID,
					Val:        val,
					Metadata:   comp.TSIDMetadata(payload.TSID),
					Timestamp:  data.msg.Timestamp(),
					Resolution: data.resolution,
				}
			}

		case dataReq := <-jr.dataRequestCh:
			comp := jr.jobsByProgram[dataReq.program]
			if comp == nil {
				dataReq.respCh <- dataResponse{
					err: errors.New("no started computation found"),
				}
				continue
			}
			dataReq.respCh <- dataResponse{
				snapshot: jr.metricSnapshotsByHandle[comp.Handle()],
				err:      nil,
			}
		case <-cleanupTicker.C:
			now := time.Now()
			// Cleanup all of the tsids that haven't reported data for a while.
			for handle := range jr.metricSnapshotsByHandle {
				snapshot := jr.metricSnapshotsByHandle[handle]
				for tsid := range snapshot {
					meta := snapshot[tsid]
					expiry := jr.CleanupOldTSIDsInterval
					if meta.Resolution != 0 {
						expiry = meta.Resolution * 3
					}

					if expiry < jr.MinimumTimeseriesExpiry {
						expiry = jr.MinimumTimeseriesExpiry
					}

					if now.Sub(meta.Timestamp) > expiry {
						delete(snapshot, tsid)
						klog.V(5).Infof("Cleaning up old TSID %s from computation with handle %s", tsid, handle)
					}
				}
			}
		}
	}
}

func (jr *SignalFlowJobRunner) watchJob(comp *signalflow.Computation, program string) {
	if jr.ctx == nil {
		panic("must start job runner before running programs")
	}
	for {
		select {
		case <-jr.ctx.Done():
			jr.StopJob(program)
			return
		case msg, ok := <-comp.Data():
			if comp.Err() != nil || !ok {
				atomic.AddInt64(&jr.TotalJobsErrored, 1)

				klog.Errorf("SignalFlow job errored, restarting in a bit: %v", comp.Err())

				for {
					time.Sleep(5 * time.Second)
					err := jr.ReplaceOrStartJob(program)
					if err == nil {
						break
					}
					klog.Errorf("Could not restart SignalFlow job %s: %v", program, err)
				}
				return
			}

			jr.dataCh <- newDataMsg{
				msg:        msg,
				handle:     comp.Handle(),
				resolution: comp.Resolution(),
			}
		}
	}
}

func (jr *SignalFlowJobRunner) InternalMetrics() []*datapoint.Datapoint {
	return []*datapoint.Datapoint{
		sfxclient.CumulativeP("started_jobs", nil, &jr.TotalJobsStarted),
		sfxclient.CumulativeP("stopped_jobs", nil, &jr.TotalJobsStopped),
		sfxclient.CumulativeP("errored_jobs", nil, &jr.TotalJobsErrored),
	}
}

func (jr *SignalFlowJobRunner) LatestSnapshot(m *HPAMetric) (MetricSnapshot, error) {
	// Give it a buffer of one so that the main loop doesn't have to block
	// sending back the response
	respCh := make(chan dataResponse, 1)
	jr.dataRequestCh <- dataRequest{
		program: m.SignalFlowProgram(),
		respCh:  respCh,
	}

	resp := <-respCh

	return resp.snapshot, resp.err
}
