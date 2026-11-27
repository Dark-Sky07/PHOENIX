package job

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeXrayHealthService struct {
	crashed      bool
	running      bool
	startedAt    time.Time
	observed     bool
	observeErr   error
	apiErrors    []error
	apiCalls     int
	recorded     bool
	recordErr    error
	recordCalls  int
	restartCalls []bool
	restartErr   error
}

func (f *fakeXrayHealthService) DidXrayCrash() bool { return f.crashed }

func (f *fakeXrayHealthService) ObserveUnexpectedXrayExit() (bool, error) {
	return f.observed, f.observeErr
}

func (f *fakeXrayHealthService) RestartXray(force bool) error {
	f.restartCalls = append(f.restartCalls, force)
	return f.restartErr
}

func (f *fakeXrayHealthService) IsXrayRunning() bool { return f.running }

func (f *fakeXrayHealthService) XrayStartedAt() time.Time { return f.startedAt }

func (f *fakeXrayHealthService) CheckXrayAPI(context.Context) error {
	index := f.apiCalls
	f.apiCalls++
	if index >= len(f.apiErrors) {
		return nil
	}
	return f.apiErrors[index]
}

func (f *fakeXrayHealthService) RecordUnreachableXrayAPI() (bool, error) {
	f.recordCalls++
	return f.recorded, f.recordErr
}

func TestCheckXrayRunningJobRecoversPersistentlyUnreachableAPIOnlyAfterEvidence(t *testing.T) {
	unreachable := errors.New("connection refused")
	fake := &fakeXrayHealthService{
		running:   true,
		apiErrors: []error{unreachable, unreachable, unreachable},
		recorded:  true,
	}
	job := &CheckXrayRunningJob{xrayService: fake}

	job.Run()
	job.Run()
	if fake.recordCalls != 0 || len(fake.restartCalls) != 0 {
		t.Fatalf("recovered before threshold: record=%d restart=%v", fake.recordCalls, fake.restartCalls)
	}
	job.Run()

	if fake.apiCalls != xrayAPIUnreachableThreshold {
		t.Fatalf("API checks=%d, want %d", fake.apiCalls, xrayAPIUnreachableThreshold)
	}
	if fake.recordCalls != 1 {
		t.Fatalf("incident records=%d, want 1", fake.recordCalls)
	}
	if len(fake.restartCalls) != 1 || !fake.restartCalls[0] {
		t.Fatalf("restart calls=%v, want one forced recovery", fake.restartCalls)
	}
}

func TestCheckXrayRunningJobDoesNotRecoverWhenAPIRecoversBeforeConfirmation(t *testing.T) {
	unreachable := errors.New("connection refused")
	fake := &fakeXrayHealthService{
		running:   true,
		apiErrors: []error{unreachable, unreachable, nil, unreachable},
		recorded:  true,
	}
	job := &CheckXrayRunningJob{xrayService: fake}
	for range 4 {
		job.Run()
	}
	if fake.recordCalls != 0 || len(fake.restartCalls) != 0 {
		t.Fatalf("transient API outage was recovered as persistent: record=%d restart=%v", fake.recordCalls, fake.restartCalls)
	}
}

func TestCheckXrayRunningJobRetainsExistingCrashRecovery(t *testing.T) {
	fake := &fakeXrayHealthService{crashed: true, observed: true}
	job := &CheckXrayRunningJob{xrayService: fake}
	job.Run()
	job.Run()
	if len(fake.restartCalls) != 1 || fake.restartCalls[0] {
		t.Fatalf("restart calls=%v, want one non-forced crash restart", fake.restartCalls)
	}
	if fake.apiCalls != 0 {
		t.Fatalf("crashed core API was probed %d times", fake.apiCalls)
	}
}

func TestCheckXrayRunningJobDoesNotCreateIncidentWhileNewCoreStarts(t *testing.T) {
	unreachable := errors.New("connection refused")
	fake := &fakeXrayHealthService{
		running:   true,
		startedAt: time.Now(),
		apiErrors: []error{unreachable, unreachable, unreachable},
		recorded:  true,
	}
	job := &CheckXrayRunningJob{xrayService: fake}
	for range xrayAPIUnreachableThreshold + 1 {
		job.Run()
	}
	if fake.apiCalls != 0 {
		t.Fatalf("fresh core API was probed %d times during startup grace", fake.apiCalls)
	}
	if fake.recordCalls != 0 || len(fake.restartCalls) != 0 {
		t.Fatalf("fresh core created an incident or restarted: record=%d restart=%v", fake.recordCalls, fake.restartCalls)
	}
}

func TestCheckXrayRunningJobRecoversUnreachableAPIAfterStartupGrace(t *testing.T) {
	unreachable := errors.New("connection refused")
	fake := &fakeXrayHealthService{
		running:   true,
		startedAt: time.Now().Add(-xrayAPIStartupGrace - time.Second),
		apiErrors: []error{unreachable, unreachable, unreachable},
		recorded:  true,
	}
	job := &CheckXrayRunningJob{xrayService: fake}
	for range xrayAPIUnreachableThreshold {
		job.Run()
	}
	if fake.recordCalls != 1 || len(fake.restartCalls) != 1 || !fake.restartCalls[0] {
		t.Fatalf("stale core was not recovered after startup grace: record=%d restart=%v", fake.recordCalls, fake.restartCalls)
	}
}

func TestCheckXrayRunningJobDoesNotRecordMaintenanceReplacementAsOutage(t *testing.T) {
	dbDir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dbDir)
	if err := os.WriteFile(filepath.Join(dbDir, "panel-update-in-progress"), []byte("test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	unreachable := errors.New("connection refused")
	fake := &fakeXrayHealthService{
		running:   true,
		startedAt: time.Now().Add(-xrayAPIStartupGrace - time.Second),
		apiErrors: []error{unreachable, unreachable, unreachable},
		recorded:  true,
	}
	job := &CheckXrayRunningJob{xrayService: fake}
	for range xrayAPIUnreachableThreshold + 1 {
		job.Run()
	}
	if fake.apiCalls != 0 || fake.recordCalls != 0 || len(fake.restartCalls) != 0 {
		t.Fatalf("maintenance replacement was treated as outage: api=%d record=%d restart=%v", fake.apiCalls, fake.recordCalls, fake.restartCalls)
	}
}
