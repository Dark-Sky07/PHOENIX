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

func TestGetTrafficRejectsMalformedPollWithoutAdvancingBaselines(t *testing.T) {
	const uplink = "user>>>alice@example.test>>>traffic>>>uplink"
	const downlink = "user>>>alice@example.test>>>traffic>>>downlink"
	for _, tc := range []struct {
		name string
		bad  []*statsService.Stat
	}{
		{"negative", []*statsService.Stat{{Name: uplink, Value: 120}, {Name: downlink, Value: -1}}},
		{"duplicate", []*statsService.Stat{{Name: uplink, Value: 120}, {Name: uplink, Value: 110}}},
		{"nil entry", []*statsService.Stat{{Name: uplink, Value: 120}, nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeTrafficStatsClient{}
			var client statsService.StatsServiceClient = fake
			api := &XrayAPI{grpcClient: &grpc.ClientConn{}, StatsServiceClient: &client}
			api.ResetTrafficBaseline(true)
			fake.response = &statsService.QueryStatsResponse{Stat: []*statsService.Stat{{Name: uplink, Value: 100}, {Name: downlink, Value: 50}}}
			if _, _, err := api.GetTraffic(); err != nil {
				t.Fatal(err)
			}
			fake.response = &statsService.QueryStatsResponse{Stat: tc.bad}
			if _, _, err := api.GetTraffic(); err == nil {
				t.Fatal("invalid poll accepted")
			}
			fake.response = &statsService.QueryStatsResponse{Stat: []*statsService.Stat{{Name: uplink, Value: 130}, {Name: downlink, Value: 60}}}
			_, rows, err := api.GetTraffic()
			if err != nil || len(rows) != 1 || rows[0].Up != 30 || rows[0].Down != 10 {
				t.Fatalf("retry lost or double counted traffic: %+v %v", rows, err)
			}
		})
	}
}

func TestGetTrafficIgnoresNonTrafficStats(t *testing.T) {
	fake := &fakeTrafficStatsClient{response: &statsService.QueryStatsResponse{Stat: []*statsService.Stat{{Name: "prefix user>>>alice>>>traffic>>>uplink suffix", Value: 7}, {Name: "some-other-metric", Value: 42}}}}
	var client statsService.StatsServiceClient = fake
	api := &XrayAPI{grpcClient: &grpc.ClientConn{}, StatsServiceClient: &client}
	api.ResetTrafficBaseline(true)
	traffic, clients, err := api.GetTraffic()
	if err != nil || len(traffic) != 0 || len(clients) != 0 || len(api.StatsLastValues) != 0 {
		t.Fatalf("unexpected stats: %+v %+v %v", traffic, clients, err)
	}
}

func TestGetTrafficRejectsUserCounterDecreaseWithoutAdvancingAnyBaseline(t *testing.T) {
	const up = "user>>>alice@example.test>>>traffic>>>uplink"
	const down = "user>>>alice@example.test>>>traffic>>>downlink"
	fake := &fakeTrafficStatsClient{}
	var client statsService.StatsServiceClient = fake
	api := &XrayAPI{grpcClient: &grpc.ClientConn{}, StatsServiceClient: &client}
	api.ResetTrafficBaseline(true)
	fake.response = &statsService.QueryStatsResponse{Stat: []*statsService.Stat{{Name: up, Value: 100}, {Name: down, Value: 50}}}
	if _, _, err := api.GetTraffic(); err != nil {
		t.Fatal(err)
	}
	fake.response.Stat[0].Value = 120
	fake.response.Stat[1].Value = 4
	_, _, err := api.GetTraffic()
	var decrease *UserTrafficCounterDecrease
	if !errors.As(err, &decrease) || decrease.Email != "alice@example.test" || decrease.Direction != "downlink" || decrease.Previous != 50 || decrease.Observed != 4 {
		t.Fatalf("decrease=%+v err=%v", decrease, err)
	}
	if api.StatsLastValues[up] != 100 || api.StatsLastValues[down] != 50 {
		t.Fatalf("partial baseline advance: %+v", api.StatsLastValues)
	}
}

func TestGetTrafficRejectsDisappearingUserCounterWithoutAdvancing(t *testing.T) {
	const up = "user>>>alice@example.test>>>traffic>>>uplink"
	const down = "user>>>alice@example.test>>>traffic>>>downlink"
	fake := &fakeTrafficStatsClient{}
	var client statsService.StatsServiceClient = fake
	api := &XrayAPI{grpcClient: &grpc.ClientConn{}, StatsServiceClient: &client}
	api.ResetTrafficBaseline(true)
	fake.response = &statsService.QueryStatsResponse{Stat: []*statsService.Stat{{Name: up, Value: 100}, {Name: down, Value: 50}}}
	if _, _, err := api.GetTraffic(); err != nil {
		t.Fatal(err)
	}
	fake.response.Stat = []*statsService.Stat{{Name: up, Value: 120}}
	_, _, err := api.GetTraffic()
	var missing *UserTrafficCounterMissing
	if !errors.As(err, &missing) || missing.Email != "alice@example.test" || missing.Direction != "downlink" || missing.Previous != 50 {
		t.Fatalf("missing=%+v err=%v", missing, err)
	}
	if api.StatsLastValues[up] != 100 || api.StatsLastValues[down] != 50 {
		t.Fatalf("advanced baselines: %+v", api.StatsLastValues)
	}
	fake.response.Stat = []*statsService.Stat{{Name: up, Value: 130}, {Name: down, Value: 60}}
	_, rows, err := api.GetTraffic()
	if err != nil || len(rows) != 1 || rows[0].Up != 30 || rows[0].Down != 10 {
		t.Fatalf("retry %+v %v", rows, err)
	}
}
