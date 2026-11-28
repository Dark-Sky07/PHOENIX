package service

import (
	"strconv"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestListPagedFiltersClientsByExplicitResellerOwnership(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()

	first := model.Reseller{Name: "First reseller", Username: "first"}
	second := model.Reseller{Name: "Second reseller", Username: "second"}
	for _, row := range []any{&first, &second} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}

	firstClient := model.ClientRecord{Email: "first-client@example.test", Enable: true}
	secondClient := model.ClientRecord{Email: "second-client@example.test", Enable: true}
	adminClient := model.ClientRecord{Email: "admin-client@example.test", Enable: true}
	for _, row := range []any{&firstClient, &secondClient, &adminClient} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	// Whitespace/case in an older mapping must still resolve to the canonical
	// client identity. The filter is ownership-based, not inbound-based.
	for _, row := range []any{
		&model.ResellerClient{ResellerId: first.Id, Email: " FIRST-CLIENT@EXAMPLE.TEST "},
		&model.ResellerClient{ResellerId: second.Id, Email: secondClient.Email},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}

	svc := &ClientService{}
	inboundSvc := &InboundService{}

	firstPage, err := svc.ListPaged(inboundSvc, nil, ClientPageParams{
		Page: 1, PageSize: 25, Reseller: " " + strconv.Itoa(first.Id) + " ",
	})
	if err != nil {
		t.Fatalf("list first reseller clients: %v", err)
	}
	if firstPage.Total != 1 || firstPage.Filtered != 1 || len(firstPage.Items) != 1 {
		t.Fatalf("first reseller page = total %d filtered %d items %#v", firstPage.Total, firstPage.Filtered, firstPage.Items)
	}
	if got := firstPage.Items[0].Email; got != firstClient.Email {
		t.Fatalf("first reseller client = %q, want %q", got, firstClient.Email)
	}

	bothPage, err := svc.ListPaged(inboundSvc, nil, ClientPageParams{
		Page: 1, PageSize: 25, Reseller: strconv.Itoa(first.Id) + "," + strconv.Itoa(second.Id),
	})
	if err != nil {
		t.Fatalf("list both reseller clients: %v", err)
	}
	if bothPage.Total != 2 || bothPage.Filtered != 2 || len(bothPage.Items) != 2 {
		t.Fatalf("both reseller page = total %d filtered %d items %#v", bothPage.Total, bothPage.Filtered, bothPage.Items)
	}
	for _, item := range bothPage.Items {
		if item.Email == adminClient.Email {
			t.Fatal("admin-owned client appeared in reseller-filtered page")
		}
	}

	pagedResult, err := svc.ListPaged(inboundSvc, nil, ClientPageParams{
		Page: 2, PageSize: 1, Reseller: strconv.Itoa(first.Id) + "," + strconv.Itoa(second.Id),
	})
	if err != nil {
		t.Fatalf("page reseller clients: %v", err)
	}
	if pagedResult.Total != 2 || pagedResult.Filtered != 2 || len(pagedResult.Items) != 1 {
		t.Fatalf("reseller page 2 = total %d filtered %d items %#v", pagedResult.Total, pagedResult.Filtered, pagedResult.Items)
	}
	if pagedResult.Items[0].Email == adminClient.Email {
		t.Fatal("admin-owned client appeared on a later reseller-filtered page")
	}

	// A reseller session's controller supplies ScopeEmails. Its query must not
	// be narrowed by an attacker-provided reseller ID, which would otherwise
	// disclose another reseller's ownership through the filtered count.
	scope := []string{firstClient.Email}
	scopedPage, err := svc.ListPaged(inboundSvc, nil, ClientPageParams{
		Page: 1, PageSize: 25, ScopeEmails: &scope, Reseller: strconv.Itoa(second.Id),
	})
	if err != nil {
		t.Fatalf("list scoped reseller clients: %v", err)
	}
	if scopedPage.Total != 1 || len(scopedPage.Items) != 1 || scopedPage.Items[0].Email != firstClient.Email {
		t.Fatalf("scoped reseller page must ignore foreign reseller selector, got %#v", scopedPage)
	}

	nonePage, err := svc.ListPaged(inboundSvc, nil, ClientPageParams{
		Page: 1, PageSize: 25, Reseller: "999999",
	})
	if err != nil {
		t.Fatalf("list missing reseller clients: %v", err)
	}
	if nonePage.Total != 0 || nonePage.Filtered != 0 || len(nonePage.Items) != 0 {
		t.Fatalf("missing reseller filter returned %#v", nonePage)
	}
}
