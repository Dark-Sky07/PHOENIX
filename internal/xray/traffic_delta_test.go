package xray

import (
	"context"
	"errors"
	"testing"

	statsService "github.com/xtls/xray-core/app/stats/command"
	"google.golang.org/grpc"
)

func TestTrafficCounterDeltaNewClientsAndReset(t *testing.T) {
	api := &XrayAPI{}
	if _, ok := api.trafficCounterDelta("user>>>existing>>>traffic>>>uplink", 100); ok {
		t.Fatal("initial bootstrap must not duplicate previous core traffic")
	}
	api.statsBaselineReady = true
	if got, ok := api.trafficCounterDelta("user>>>new>>>traffic>>>uplink", 75); !ok || got != 75 {
		t.Fatalf("first interval lost: %d %v", got, ok)
	}
	if got, ok := api.trafficCounterDelta("user>>>new>>>traffic>>>uplink", 90); !ok || got != 15 {
		t.Fatalf("subsequent delta: %d %v", got, ok)
	}
	if got, ok := api.trafficCounterDelta("user>>>new>>>traffic>>>uplink", 12); !ok || got != 12 {
		t.Fatalf("reset interval lost: %d %v", got, ok)
	}
	if _, ok := api.trafficCounterDelta("user>>>new>>>traffic>>>uplink", -1); ok {
		t.Fatal("negative counter accepted")
	}
	if got, ok := api.trafficCounterDelta("user>>>new>>>traffic>>>uplink", 14); !ok || got != 2 {
		t.Fatalf("negative counter changed baseline: %d %v", got, ok)
	}
}

func TestTrafficBaselineResetOnNewCore(t *testing.T) {
	api := &XrayAPI{}
	api.trafficCounterDelta("user>>>alice>>>traffic>>>uplink", 200)
	api.statsBaselineReady = true
	api.ResetTrafficBaseline(true)
	if got, ok := api.trafficCounterDelta("user>>>alice>>>traffic>>>uplink", 500); !ok || got != 500 {
		t.Fatalf("new core inherited stale baseline: got %d include %v", got, ok)
	}
	api.ResetTrafficBaseline(false)
	if _, ok := api.trafficCounterDelta("user>>>alice>>>traffic>>>uplink", 800); ok {
		t.Fatal("reattaching to an existing core must establish a baseline")
	}
}

type fakeTrafficStatsClient struct {
	statsService.StatsServiceClient
	response *statsService.QueryStatsResponse
	err      error
	sawReset bool
}

func (f *fakeTrafficStatsClient) QueryStats(_ context.Context, req *statsService.QueryStatsRequest, _ ...grpc.CallOption) (*statsService.QueryStatsResponse, error) {
	f.sawReset = f.sawReset || req.GetReset_()
	if f.err != nil {
		return nil, f.err
	}
	return f.response, nil
}

func TestGetTrafficCapturesFirstManagedIntervalAndRetainsBaselineOnError(t *testing.T) {
	fake := &fakeTrafficStatsClient{}
	var client statsService.StatsServiceClient = fake
	api := &XrayAPI{grpcClient: &grpc.ClientConn{}, StatsServiceClient: &client}
	api.ResetTrafficBaseline(true)
	fake.response = &statsService.QueryStatsResponse{Stat: []*statsService.Stat{
		{Name: "user>>>alice@example.test>>>traffic>>>uplink", Value: 100},
		{Name: "user>>>alice@example.test>>>traffic>>>downlink", Value: 70},
	}}
	_, rows, err := api.GetTraffic()
	if err != nil || len(rows) != 1 || rows[0].Up != 100 || rows[0].Down != 70 {
		t.Fatalf("first poll = %+v, %v", rows, err)
	}
	fake.err = errors.New("temporary stats failure")
	if _, _, err := api.GetTraffic(); err == nil {
		t.Fatal("expected failed poll")
	}
	fake.err = nil
	fake.response.Stat[0].Value = 120
	fake.response.Stat[1].Value = 85
	_, rows, err = api.GetTraffic()
	if err != nil || len(rows) != 1 || rows[0].Up != 20 || rows[0].Down != 15 {
		t.Fatalf("retry delta = %+v, %v", rows, err)
	}
	_, rows, err = api.GetTraffic()
	if err != nil || len(rows) != 1 || rows[0].Up != 0 || rows[0].Down != 0 {
		t.Fatalf("unchanged counters = %+v, %v", rows, err)
	}
	if fake.sawReset {
		t.Fatal("stats poll reset runtime counters")
	}
}
