package service

import (
	"reflect"
	"testing"
)

func TestParseInboundSniffing(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantEnable bool
		wantDest   []string
	}{
		{name: "enabled", raw: `{"enabled":true,"destOverride":["http","tls","quic"]}`, wantEnable: true, wantDest: []string{"http", "tls", "quic"}},
		{name: "disabled", raw: `{"enabled":false,"destOverride":["tls"]}`, wantDest: []string{"tls"}},
		{name: "empty object", raw: `{}`},
		{name: "empty string", raw: ``},
		{name: "malformed", raw: `{`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotEnable, gotDest := parseInboundSniffing(tt.raw)
			if gotEnable != tt.wantEnable {
				t.Fatalf("enabled = %v, want %v", gotEnable, tt.wantEnable)
			}
			if !reflect.DeepEqual(gotDest, tt.wantDest) {
				t.Fatalf("destOverride = %v, want %v", gotDest, tt.wantDest)
			}
		})
	}
}
