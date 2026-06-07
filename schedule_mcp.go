package social

import (
	"encoding/json"
	"fmt"
	"time"

	"smeldr.dev/core"
)

// ─── scheduleModule — MCPModule for PublicationSchedule ──────────────────────

// scheduleModule implements [smeldr.MCPModule] for [PublicationSchedule].
// Obtain it via [Social.ScheduleModule].
type scheduleModule struct {
	social *Social
}

// MCPMeta returns the MCP registration metadata for PublicationSchedule.
// TypeName "PublicationSchedule" produces tools named create_publication_schedule,
// list_publication_schedules, get_publication_schedule,
// update_publication_schedule, delete_publication_schedule.
func (m *scheduleModule) MCPMeta() smeldr.MCPMeta {
	return smeldr.MCPMeta{
		Prefix:     "/social/schedules",
		TypeName:   "PublicationSchedule",
		Operations: []smeldr.MCPOperation{smeldr.MCPRead, smeldr.MCPWrite},
	}
}

// MCPSchema returns the field schema for PublicationSchedule create/update.
func (m *scheduleModule) MCPSchema() []smeldr.MCPField {
	return []smeldr.MCPField{
		{
			Name:        "CredentialID",
			JSONName:    "credential_id",
			Type:        "string",
			Required:    true,
			Description: "ID of the SocialCredential this schedule belongs to. Each credential may have at most one schedule.",
		},
		{
			Name:        "Slots",
			JSONName:    "slots",
			Type:        "string",
			Required:    true,
			Description: `JSON array of slot objects. Each slot: {"weekday": 1, "time": "09:00", "timezone": "Europe/London"}. weekday: 0=Sunday…6=Saturday.`,
		},
		{
			Name:        "Status",
			JSONName:    "status",
			Type:        "string",
			Required:    false,
			Enum:        []string{"active", "paused"},
			Description: "active (default) fires slots and publishes queued posts. paused suspends the schedule without deleting it.",
		},
	}
}

// MCPList returns all PublicationSchedules as []any.
func (m *scheduleModule) MCPList(_ smeldr.Context, _ ...smeldr.Status) ([]any, error) {
	schedules, err := listSchedules(m.social.db)
	if err != nil {
		return nil, err
	}
	out := make([]any, len(schedules))
	for i, s := range schedules {
		out[i] = s
	}
	return out, nil
}

// MCPGet returns the PublicationSchedule with the given slug (= ID).
func (m *scheduleModule) MCPGet(_ smeldr.Context, slug string) (any, error) {
	s, err := getSchedule(m.social.db, slug)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// MCPCreate creates a new PublicationSchedule for the given credential.
// The slots field must be a valid JSON array of Slot objects.
func (m *scheduleModule) MCPCreate(_ smeldr.Context, fields map[string]any) (any, error) {
	credentialID, _ := fields["credential_id"].(string)
	if credentialID == "" {
		return nil, smeldr.Err("credential_id", "required")
	}
	slotsRaw, _ := fields["slots"].(string)
	if slotsRaw == "" {
		return nil, smeldr.Err("slots", "required — JSON array of slot objects")
	}

	var slots []Slot
	if err := json.Unmarshal([]byte(slotsRaw), &slots); err != nil {
		return nil, smeldr.Err("slots", fmt.Sprintf("must be a valid JSON array: %v", err))
	}
	if err := validateSlots(slots); err != nil {
		return nil, err
	}

	status := ScheduleStatusActive
	if v, _ := fields["status"].(string); v == string(ScheduleStatusPaused) {
		status = ScheduleStatusPaused
	}

	now := time.Now().UTC()
	s := PublicationSchedule{
		ID:           smeldr.NewID(),
		CredentialID: credentialID,
		Slots:        slots,
		Status:       status,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := insertSchedule(m.social.db, s); err != nil {
		return nil, err
	}
	return s, nil
}

// MCPUpdate applies a partial update to the PublicationSchedule with the given slug.
// Supports updating slots and status.
func (m *scheduleModule) MCPUpdate(_ smeldr.Context, slug string, fields map[string]any) (any, error) {
	s, err := getSchedule(m.social.db, slug)
	if err != nil {
		return nil, err
	}

	if slotsRaw, ok := fields["slots"].(string); ok {
		var slots []Slot
		if err := json.Unmarshal([]byte(slotsRaw), &slots); err != nil {
			return nil, smeldr.Err("slots", fmt.Sprintf("must be a valid JSON array: %v", err))
		}
		if err := validateSlots(slots); err != nil {
			return nil, err
		}
		s.Slots = slots
	}

	if v, ok := fields["status"].(string); ok {
		switch ScheduleStatus(v) {
		case ScheduleStatusActive, ScheduleStatusPaused:
			s.Status = ScheduleStatus(v)
		default:
			return nil, smeldr.Err("status", "must be 'active' or 'paused'")
		}
	}

	if err := updateSchedule(m.social.db, s); err != nil {
		return nil, err
	}
	return s, nil
}

// MCPPublish is not supported for PublicationSchedule — it has no lifecycle.
func (m *scheduleModule) MCPPublish(_ smeldr.Context, _ string) error {
	return smeldr.ErrBadRequest
}

// MCPSchedule is not supported for PublicationSchedule.
func (m *scheduleModule) MCPSchedule(_ smeldr.Context, _ string, _ time.Time) error {
	return smeldr.ErrBadRequest
}

// MCPArchive is not supported for PublicationSchedule.
func (m *scheduleModule) MCPArchive(_ smeldr.Context, _ string) error {
	return smeldr.ErrBadRequest
}

// MCPDelete permanently deletes the PublicationSchedule with the given slug.
func (m *scheduleModule) MCPDelete(_ smeldr.Context, slug string) error {
	return deleteSchedule(m.social.db, slug)
}

// validateSlots checks that each slot has a valid weekday, HH:MM time, and IANA timezone.
func validateSlots(slots []Slot) error {
	for i, slot := range slots {
		if slot.Timezone == "" {
			return smeldr.Err("slots", fmt.Sprintf("slot %d: timezone is required (IANA name, e.g. \"Europe/Copenhagen\")", i))
		}
		if slot.Weekday < 0 || slot.Weekday > 6 {
			return smeldr.Err("slots", fmt.Sprintf("slot %d: weekday must be 0–6 (0=Sunday)", i))
		}
		var h, min int
		if _, err := fmt.Sscanf(slot.Time, "%d:%d", &h, &min); err != nil || h < 0 || h > 23 || min < 0 || min > 59 {
			return smeldr.Err("slots", fmt.Sprintf("slot %d: time must be HH:MM (24-hour), got %q", i, slot.Time))
		}
		if _, err := time.LoadLocation(slot.Timezone); err != nil {
			return smeldr.Err("slots", fmt.Sprintf("slot %d: timezone %q is not a valid IANA name", i, slot.Timezone))
		}
	}
	return nil
}
