package xray

import (
	"context"
	"errors"
	"testing"

	statsService "github.com/xtls/xray-core/app/stats/command"
	"google.golang.org/grpc"
)

type fakeHealthStatsClient struct {
	statsService.StatsServiceClient
	response *statsService.SysStatsResponse
	err      error
}

func (f fakeHealthStatsClient) GetSysStats(context.Context, *statsService.SysStatsRequest, ...grpc.CallOption) (*statsService.SysStatsResponse, error) {
	return f.response, f.err
}

func TestXrayAPIHealthCheckUsesReadOnlySystemStats(t *testing.T) {
	fake := fakeHealthStatsClient{response: &statsService.SysStatsResponse{}}
	var client statsService.StatsServiceClient = fake
	api := XrayAPI{grpcClient: &grpc.ClientConn{}, StatsServiceClient: &client}
	if err := api.HealthCheck(context.Background()); err != nil {
		t.Fatalf("health check failed: %v", err)
	}

	fake.err = errors.New("connection refused")
	client = fake
	if err := api.HealthCheck(context.Background()); err == nil {
		t.Fatal("unreachable API was reported healthy")
	}
}

func TestXrayAPIHealthCheckRejectsUninitializedAndEmptyResponses(t *testing.T) {
	if err := (&XrayAPI{}).HealthCheck(context.Background()); err == nil {
		t.Fatal("uninitialized API was reported healthy")
	}
	fake := fakeHealthStatsClient{}
	var client statsService.StatsServiceClient = fake
	api := XrayAPI{grpcClient: &grpc.ClientConn{}, StatsServiceClient: &client}
	if err := api.HealthCheck(nil); err == nil {
		t.Fatal("empty system stats response was reported healthy")
	}
}
