package entity

import "testing"

func TestValidatePanelAutoRestartCron(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		wantErr bool
	}{
		{name: "six hourly", expr: "@every 6h"},
		{name: "daily at four", expr: "0 0 4 * * *"},
		{name: "weekly", expr: "@weekly"},
		{name: "too frequent interval", expr: "@every 30m", wantErr: true},
		{name: "too frequent cron", expr: "0 */30 * * * *", wantErr: true},
		{name: "invalid expression", expr: "not a cron", wantErr: true},
		{name: "empty expression", expr: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePanelAutoRestartCron(tt.expr)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidatePanelAutoRestartCron(%q) error = %v, wantErr %v", tt.expr, err, tt.wantErr)
			}
		})
	}
}
