package job

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/tuic"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/web/websocket"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

const defaultTuicSpeedSampleInterval = 10 * time.Second

type TuicJob struct {
	inboundService  service.InboundService
	runMu           sync.Mutex
	lastSpeedSample time.Time
}

func NewTuicJob() *TuicJob {
	return new(TuicJob)
}

func (j *TuicJob) Run() {
	j.runMu.Lock()
	defer j.runMu.Unlock()

	tuicJournalMu.Lock()
	if sharedTuicPoll.batch == nil {
		if err := j.replayTuicJournal(); err != nil {
			tuicJournalMu.Unlock()
			logger.Warning("tuic job: recover traffic journal failed; reconciliation deferred:", err)
			return
		}
	}
	mgr := tuic.GetManager()
	clientDeltas, _, clientsDisabled, pollErr := collectAndCommitTuic(&sharedTuicPoll, func() []tuic.ClientTrafficDelta {
		_, deltas := mgr.CollectAllTraffic()
		return deltas
	}, &j.inboundService)
	if pollErr != nil {
		tuicJournalMu.Unlock()
		logger.Warning("tuic job: poll or accounting failed; retaining fence:", pollErr)
		return
	}
	if clientsDisabled {
		websocket.BroadcastInvalidate(websocket.MessageTypeInbounds)
	}
	// Reconciliation can stop a daemon and drain final counters into the
	// manager. Put a second shared marker around the actual runtime mutation
	// and its immediate drain, not just around the preceding periodic sample.
	// Re-read desired state while the allocation lock is held.
	var desired []tuic.Instance
	var desiredErr error
	reconciledDeltas, _, reconcileDisabled, reconcileErr := collectAndCommitTuic(&sharedTuicPoll, func() []tuic.ClientTrafficDelta {
		desired, desiredErr = j.inboundService.DesiredTuicInstances()
		if desiredErr == nil {
			mgr.Reconcile(desired)
		}
		_, deltas := mgr.CollectAllTraffic()
		return deltas
	}, &j.inboundService)
	tuicJournalMu.Unlock()
	if reconcileErr != nil {
		logger.Warning("tuic job: reconcile accounting failed; retaining fence:", reconcileErr)
		return
	}
	if reconcileDisabled {
		websocket.BroadcastInvalidate(websocket.MessageTypeInbounds)
	}
	if desiredErr != nil {
		logger.Warning("tuic job: get desired instances failed:", desiredErr)
		return
	}
	clientDeltas = append(clientDeltas, reconciledDeltas...)
	activeTags := make([]string, 0, len(desired))
	for _, inst := range desired {
		activeTags = append(activeTags, inst.Tag)
	}
	onlineEmails, _ := mgr.GetActiveClients(30 * time.Second)

	clientTraffics := aggregateTuicClientTraffic(clientDeltas, onlineEmails)
	sampledAt := time.Now()
	sampleInterval := tuicSpeedSampleInterval(j.lastSpeedSample, sampledAt)
	// Inbound totals are metered through the Xray relay; this is only client
	// traffic, now committed before periodic daemon reconciliation.
	websocket.BroadcastTraffic(tuicSpeedPayload(clientTraffics, sampleInterval))
	j.lastSpeedSample = sampledAt

	if len(onlineEmails) > 0 {
		if err := j.inboundService.BumpClientsLastOnline(onlineEmails); err != nil {
			logger.Warning("tuic job: bump last online for tuic clients failed:", err)
		}
	}

	j.inboundService.RefreshLocalOnlineClients(onlineEmails, activeTags)
}

func tuicSpeedSampleInterval(previous, current time.Time) time.Duration {
	if previous.IsZero() || !current.After(previous) {
		return defaultTuicSpeedSampleInterval
	}
	return current.Sub(previous)
}

func tuicSpeedPayload(clientTraffics []*xray.ClientTraffic, sampleInterval time.Duration) map[string]any {
	intervalMs := sampleInterval.Milliseconds()
	if intervalMs < 1 {
		intervalMs = 1
	}
	return map[string]any{
		"clientTraffics":          clientTraffics,
		"clientTrafficSource":     "tuic",
		"clientTrafficIntervalMs": intervalMs,
	}
}

// FlushStoppedTraffic persists counters drained when the TUIC manager stops its
// listeners. Call it after scheduled jobs have stopped and before the traffic
// writer shuts down.
func (j *TuicJob) FlushStoppedTraffic() error {
	return j.flushTuicJournal()
}

// Saturate counters rather than wrap a large valid total into a small delta.
// A negative input remains negative so accounting rejects the batch.
func addTuicTrafficBytes(current, delta int64) int64 {
	if current < 0 || delta < 0 {
		return -1
	}
	if current > math.MaxInt64-delta {
		return math.MaxInt64
	}
	return current + delta
}

func aggregateTuicClientTraffic(clientDeltas []tuic.ClientTrafficDelta, onlineEmails []string) []*xray.ClientTraffic {
	clientTrafficMap := make(map[string]*xray.ClientTraffic, len(clientDeltas)+len(onlineEmails))
	for _, cd := range clientDeltas {
		key := cd.Email
		if cd.TrafficID > 0 {
			key = fmt.Sprintf("traffic:%d", cd.TrafficID)
		}
		if cd.TrafficID == 0 && cd.InboundID > 0 && cd.UUID != "" {
			key = fmt.Sprintf("tuic:%d:%s", cd.InboundID, cd.UUID)
		}
		traffic := clientTrafficMap[key]
		if traffic == nil {
			traffic = &xray.ClientTraffic{Email: cd.Email, TuicTrafficID: cd.TrafficID, TuicUUID: cd.UUID, TuicInboundId: cd.InboundID}
			clientTrafficMap[key] = traffic
		}
		traffic.Up = addTuicTrafficBytes(traffic.Up, cd.Up)
		traffic.Down = addTuicTrafficBytes(traffic.Down, cd.Down)
	}
	for _, email := range onlineEmails {
		if _, exists := clientTrafficMap[email]; !exists {
			clientTrafficMap[email] = &xray.ClientTraffic{
				Email: email,
				Up:    0,
				Down:  0,
			}
		}
	}

	clientTraffics := make([]*xray.ClientTraffic, 0, len(clientTrafficMap))
	for _, ct := range clientTrafficMap {
		clientTraffics = append(clientTraffics, ct)
	}
	return clientTraffics
}
