package service

import (
	"encoding/json"
	"strings"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/util/common"
)

var dashboardWidgetIDs = map[string]struct{}{
	"safety":    {},
	"resources": {},
	"services":  {},
	"network":   {},
	"info":      {},
}

var defaultDashboardLayout = []string{"safety", "resources", "services", "network", "info"}

type DashboardPreferenceService struct{}

type DashboardPreferenceView struct {
	Layout         []string `json:"layout"`
	HiddenWidgets  []string `json:"hiddenWidgets"`
	RefreshSeconds int      `json:"refreshSeconds"`
}

type DashboardPreferenceInput struct {
	Layout         []string `json:"layout" form:"layout"`
	HiddenWidgets  []string `json:"hiddenWidgets" form:"hiddenWidgets"`
	RefreshSeconds int      `json:"refreshSeconds" form:"refreshSeconds"`
}

func (DashboardPreferenceService) Get(userID int) (*DashboardPreferenceView, error) {
	if userID <= 0 {
		return nil, common.NewError("dashboard preferences require an administrator")
	}
	var row model.DashboardPreference
	if err := database.GetDB().Where("user_id = ?", userID).First(&row).Error; err != nil {
		if database.IsNotFound(err) {
			view := defaultDashboardPreference()
			return &view, nil
		}
		return nil, err
	}
	view := dashboardPreferenceView(&row)
	return &view, nil
}

func (DashboardPreferenceService) Save(userID int, input DashboardPreferenceInput) (*DashboardPreferenceView, error) {
	if userID <= 0 {
		return nil, common.NewError("dashboard preferences require an administrator")
	}
	layout, hidden, refresh, err := normalizeDashboardPreference(input)
	if err != nil {
		return nil, err
	}
	layoutJSON, _ := json.Marshal(layout)
	hiddenJSON, _ := json.Marshal(hidden)
	row := model.DashboardPreference{UserID: userID}
	db := database.GetDB()
	if err := db.Where("user_id = ?", userID).First(&row).Error; err != nil && !database.IsNotFound(err) {
		return nil, err
	}
	row.Layout, row.HiddenWidgets, row.RefreshSeconds = string(layoutJSON), string(hiddenJSON), refresh
	if row.ID == 0 {
		if err := db.Create(&row).Error; err != nil {
			return nil, err
		}
	} else if err := db.Save(&row).Error; err != nil {
		return nil, err
	}
	view := dashboardPreferenceView(&row)
	return &view, nil
}

func defaultDashboardPreference() DashboardPreferenceView {
	layout := append([]string(nil), defaultDashboardLayout...)
	return DashboardPreferenceView{Layout: layout, HiddenWidgets: []string{}, RefreshSeconds: 2}
}

func normalizeDashboardPreference(input DashboardPreferenceInput) ([]string, []string, int, error) {
	layout := normalizeDashboardWidgetIDs(input.Layout)
	if len(layout) == 0 {
		layout = append([]string(nil), defaultDashboardLayout...)
	}
	// Ensure new widgets arrive for all users after later upgrades without
	// discarding the order they have already chosen.
	for _, id := range defaultDashboardLayout {
		if !containsDashboardWidget(layout, id) {
			layout = append(layout, id)
		}
	}
	hidden := normalizeDashboardWidgetIDs(input.HiddenWidgets)
	refresh := input.RefreshSeconds
	if refresh == 0 {
		refresh = 2
	}
	if refresh < 2 || refresh > 60 {
		return nil, nil, 0, common.NewError("dashboard refresh interval must be between 2 and 60 seconds")
	}
	return layout, hidden, refresh, nil
}

func normalizeDashboardWidgetIDs(ids []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.ToLower(strings.TrimSpace(id))
		if _, known := dashboardWidgetIDs[id]; !known {
			continue
		}
		if _, exists := seen[id]; !exists {
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

func containsDashboardWidget(ids []string, wanted string) bool {
	for _, id := range ids {
		if id == wanted {
			return true
		}
	}
	return false
}

func dashboardPreferenceView(row *model.DashboardPreference) DashboardPreferenceView {
	view := defaultDashboardPreference()
	var layout, hidden []string
	if json.Unmarshal([]byte(row.Layout), &layout) == nil {
		view.Layout = normalizeDashboardWidgetIDs(layout)
	}
	for _, id := range defaultDashboardLayout {
		if !containsDashboardWidget(view.Layout, id) {
			view.Layout = append(view.Layout, id)
		}
	}
	if json.Unmarshal([]byte(row.HiddenWidgets), &hidden) == nil {
		view.HiddenWidgets = normalizeDashboardWidgetIDs(hidden)
	}
	if row.RefreshSeconds >= 2 && row.RefreshSeconds <= 60 {
		view.RefreshSeconds = row.RefreshSeconds
	}
	return view
}
