// Package job provides background job implementations for the PHOENIX web panel,
// including traffic monitoring, system checks, and periodic maintenance tasks.
package job

import (
	"context"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
)

const xrayAPIUnreachableThreshold = 3

// xrayHealthService is deliberately narrow so the outage/recovery path is
// testable without starting a real core. The concrete service still owns all
// process and accounting locks.
type xrayHealthService interface {
	DidXrayCrash() bool
	ObserveUnexpectedXrayExit() (bool, error)
	RestartXray(bool) error
	IsXrayRunning() bool
	CheckXrayAPI(context.Context) error
	RecordUnreachableXrayAPI() (bool, error)
}

// CheckXrayRunningJob monitors the managed process and its gRPC control API.
// A process can remain alive while its loopback API listener has disappeared;
// treating that as healthy left accounting and panel operations repeatedly
// failing with connection refused.
type CheckXrayRunningJob struct {
	xrayService  xrayHealthService
	checkTime    int
	apiCheckTime int
}

// NewCheckXrayRunningJob creates a new Xray health check job instance.
func NewCheckXrayRunningJob() *CheckXrayRunningJob {
	return &CheckXrayRunningJob{xrayService: &service.XrayService{}}
}

// Run checks whether Xray has crashed and restarts it after confirming it is
// down twice. It also confirms that a still-running core answers a cheap,
// read-only API request. Before recovering a persistently unreachable API it
// durably records an accounting incident, so a restart cannot silently discard
// the unobservable interval since the last traffic poll.
func (j *CheckXrayRunningJob) Run() {
	if j.xrayService == nil {
		return
	}
	if j.xrayService.DidXrayCrash() {
		j.apiCheckTime = 0
		observed, err := j.xrayService.ObserveUnexpectedXrayExit()
		if err != nil {
			logger.Error("Record unexpected Xray exit failed:", err)
			return
		}
		if !observed {
			j.checkTime = 0
			return
		}
		j.checkTime++
		// Only restart after two consecutive observations.
		if j.checkTime > 1 {
			err := j.xrayService.RestartXray(false)
			j.checkTime = 0
			if err != nil {
				logger.Error("Restart xray failed:", err)
			}
		}
		return
	}

	j.checkTime = 0
	if !j.xrayService.IsXrayRunning() {
		j.apiCheckTime = 0
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	err := j.xrayService.CheckXrayAPI(ctx)
	cancel()
	if err == nil {
		j.apiCheckTime = 0
		return
	}
	j.apiCheckTime++
	if j.apiCheckTime < xrayAPIUnreachableThreshold {
		return
	}
	j.apiCheckTime = 0

	recorded, recordErr := j.xrayService.RecordUnreachableXrayAPI()
	if recordErr != nil {
		logger.Error("Record unreachable Xray API failed:", recordErr)
		return
	}
	if !recorded {
		// The core recovered between checks, was stopped intentionally, or was
		// replaced; no incident is needed and no restart is performed.
		return
	}
	logger.Error("Xray API remained unreachable; durable reconciliation evidence was retained before recovery")
	if err := j.xrayService.RestartXray(true); err != nil {
		logger.Error("Recover unreachable Xray API failed:", err)
	}
}
