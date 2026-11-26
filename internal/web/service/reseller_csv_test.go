package service

import (
	"encoding/csv"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func csvLines(t *testing.T, csv string) []string {
	t.Helper()
	body := strings.TrimPrefix(csv, "\xEF\xBB\xBF")
	body = strings.TrimSuffix(body, "\r\n")
	return strings.Split(body, "\r\n")
}

func TestRenderResellerClientsCSV(t *testing.T) {
	expiry := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	report := &ResellerReport{
		Clients: []ResellerClientRow{
			{
				Email:      `ali, "the reseller" @x`,
				Comment:    "مشتری ویژه\nline two",
				Group:      "gold",
				Enable:     true,
				TotalBytes: 5 * bytesPerGB,
				ExpiryTime: expiry.UnixMilli(),
				Up:         bytesPerGB,
				Down:       2 * bytesPerGB,
				Used:       3 * bytesPerGB,
				Cost:       1.5,
				InboundIds: []int{3, 7},
				SubID:      "sub-1",
			},
			{
				// unlimited quota, no expiry, no inbound ids
				Email: "plain@x",
				Used:  bytesPerGB / 2,
			},
		},
	}
	csv := string(RenderResellerClientsCSV(report))

	if !strings.HasPrefix(csv, "\xEF\xBB\xBFemail,comment,group,enabled,quota_gb") {
		t.Fatalf("missing BOM or header: %q", csv[:60])
	}

	lines := csvLines(t, csv)
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want header + 2 rows", len(lines))
	}

	// Comma, quotes and the embedded newline force RFC 4180 quoting; the
	// semicolon-separated inbound ids and plain fields stay unquoted.
	first := lines[1]
	if !strings.HasPrefix(first, `"ali, ""the reseller"" @x","مشتری ویژه`+"\n"+`line two",gold,true,5.000,3.000,1.000,2.000,USD,1.50,2026-12-01,3;7,sub-1`) {
		t.Fatalf("first row = %q", first)
	}

	// Unlimited quota and never-expiring client render as empty fields.
	if second := lines[2]; second != "plain@x,,,false,,0.500,0.000,0.000,USD,0.00,,," {
		t.Fatalf("second row = %q", second)
	}
}

func TestRenderResellerClientsCSVEmpty(t *testing.T) {
	csv := string(RenderResellerClientsCSV(nil))
	if csv != "\xEF\xBB\xBFemail,comment,group,enabled,quota_gb,used_gb,up_gb,down_gb,currency,cost,expiry,inbound_ids,sub_id\r\n" {
		t.Fatalf("nil report = %q", csv)
	}
	if csv := string(RenderResellerClientsCSV(&ResellerReport{})); strings.Count(csv, "\r\n") != 1 {
		t.Fatalf("empty report must be header only, got %q", csv)
	}
}

func TestRenderResellerListCSV(t *testing.T) {
	stats := []*ResellerStat{
		{
			Reseller: &model.Reseller{
				Id:          7,
				Username:    "reseller1",
				Name:        "Ali",
				Comment:     "note, with comma",
				Enable:      true,
				ClientLimit: 10,
				ExpiryTime:  time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC).UnixMilli(),
				PricePerGB:  0.5,
				Deposit:     20,
			},
			ClientCount:      4,
			OnlineCount:      2,
			UsedTraffic:      2 * bytesPerGB,
			AllocatedTraffic: 5 * bytesPerGB,
			Cost:             1,
		},
		{
			// unlimited everything
			Reseller: &model.Reseller{Id: 8, Username: "reseller2", Enable: false},
		},
		nil,
	}
	lines := csvLines(t, string(RenderResellerListCSV(stats)))
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want header + 2 rows (nil stat skipped)", len(lines))
	}
	if lines[0] != "id,username,name,comment,enabled,clients,client_limit,online,used_gb,allocated_gb,traffic_limit_gb,currency,balance,price_per_gb,expiry" {
		t.Fatalf("header = %q", lines[0])
	}
	if lines[1] != `7,reseller1,Ali,"note, with comma",true,4,10,2,2.000,5.000,,USD,19.00,0.50,2027-03-01` {
		t.Fatalf("first row = %q", lines[1])
	}
	if lines[2] != "8,reseller2,,,false,0,,0,0.000,0.000,,USD,0.00,0.00," {
		t.Fatalf("second row = %q", lines[2])
	}
}

func TestResellerCSVNeutralizesSpreadsheetFormulas(t *testing.T) {
	parse := func(data []byte) [][]string {
		t.Helper()
		r := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(data), "\xEF\xBB\xBF")))
		rows, err := r.ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	for _, value := range []string{"=1+1", "+cmd", "-1+2", "@SUM(A1)", "  =1+1", "\u00a0=1+1", "\tplain", "\rplain", "\nplain", "\ufeff=1+1", `=HYPERLINK("https://example.test/a,b")`} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			rows := parse(RenderResellerClientsCSV(&ResellerReport{Clients: []ResellerClientRow{{Email: value, Comment: value, Group: value, SubID: value}}}))
			for _, col := range []int{0, 1, 2, 12} {
				if rows[1][col] != "'"+value {
					t.Fatalf("column %d unsafe: %q", col, rows[1][col])
				}
			}
			rows = parse(RenderResellerListCSV([]*ResellerStat{{Reseller: &model.Reseller{Username: value, Name: value, Comment: value}}}))
			for _, col := range []int{1, 2, 3} {
				if rows[1][col] != "'"+value {
					t.Fatalf("column %d unsafe: %q", col, rows[1][col])
				}
			}
		})
	}
}

func TestResellerCSVLeavesOrdinaryTextUnchanged(t *testing.T) {
	for _, value := range []string{"  safe text", "مشتری ویژه\nline two", "user@example.test", "note, with comma"} {
		reader := csv.NewReader(strings.NewReader(csvEscape(value)))
		row, err := reader.Read()
		if err != nil || len(row) != 1 || row[0] != value {
			t.Fatalf("value %q changed to %q: %v", value, row, err)
		}
	}
}
