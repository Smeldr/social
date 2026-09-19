package social

// schedule_mcp_test.go covers schedule_mcp.go: scheduleModule's smeldr.MCPModule
// implementation for PublicationSchedule, and the standalone validateSlots helper.

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"smeldr.dev/core"

	_ "modernc.org/sqlite"
)

// scheduleTestAdminCtx returns a smeldr.Context with Admin role for MCP calls.
func scheduleTestAdminCtx() smeldr.Context {
	return smeldr.NewTestContext(smeldr.User{
		ID:    "test-admin",
		Name:  "Test Admin",
		Roles: []smeldr.Role{smeldr.Admin},
	})
}

// newScheduleTestSocial opens a fresh in-memory DB and returns a *Social
// backed by it, for scheduleModule tests.
func newScheduleTestSocial(t *testing.T) (*Social, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s := New(db, Config{Secret: refreshTestSecret})
	return s, db
}

const validSlotsJSON = `[{"weekday":1,"time":"09:00","timezone":"Europe/Copenhagen"}]`

// ─── MCPCreate ──────────────────────────────────────────────────────────────

func TestScheduleModule_MCPCreate(t *testing.T) {
	ctx := scheduleTestAdminCtx()

	t.Run("missing credential_id", func(t *testing.T) {
		s, _ := newScheduleTestSocial(t)
		sm := s.ScheduleModule()
		_, err := sm.MCPCreate(ctx, map[string]any{"slots": validSlotsJSON})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("missing slots", func(t *testing.T) {
		s, _ := newScheduleTestSocial(t)
		sm := s.ScheduleModule()
		_, err := sm.MCPCreate(ctx, map[string]any{"credential_id": "cred-1"})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("malformed slots JSON", func(t *testing.T) {
		s, _ := newScheduleTestSocial(t)
		sm := s.ScheduleModule()
		_, err := sm.MCPCreate(ctx, map[string]any{
			"credential_id": "cred-1",
			"slots":         "not-json",
		})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("validateSlots rejects", func(t *testing.T) {
		s, _ := newScheduleTestSocial(t)
		sm := s.ScheduleModule()
		_, err := sm.MCPCreate(ctx, map[string]any{
			"credential_id": "cred-1",
			"slots":         `[{"weekday":9,"time":"09:00","timezone":"UTC"}]`,
		})
		if err == nil {
			t.Fatal("expected error for weekday out of range")
		}
	})

	t.Run("DB insert error", func(t *testing.T) {
		s, db := newScheduleTestSocial(t)
		if _, err := db.Exec("DROP TABLE smeldr_social_publication_schedules"); err != nil {
			t.Fatalf("drop table: %v", err)
		}
		sm := s.ScheduleModule()
		_, err := sm.MCPCreate(ctx, map[string]any{
			"credential_id": "cred-1",
			"slots":         validSlotsJSON,
		})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("success", func(t *testing.T) {
		s, _ := newScheduleTestSocial(t)
		sm := s.ScheduleModule()
		result, err := sm.MCPCreate(ctx, map[string]any{
			"credential_id": "cred-create-1",
			"slots":         validSlotsJSON,
		})
		if err != nil {
			t.Fatalf("MCPCreate: %v", err)
		}
		ps, ok := result.(PublicationSchedule)
		if !ok {
			t.Fatalf("result type = %T", result)
		}
		if ps.CredentialID != "cred-create-1" {
			t.Errorf("CredentialID = %q", ps.CredentialID)
		}
		if ps.Status != ScheduleStatusActive {
			t.Errorf("Status = %q, want active (default)", ps.Status)
		}
		if len(ps.Slots) != 1 {
			t.Errorf("Slots len = %d, want 1", len(ps.Slots))
		}
	})

	t.Run("success with explicit paused status", func(t *testing.T) {
		s, _ := newScheduleTestSocial(t)
		sm := s.ScheduleModule()
		result, err := sm.MCPCreate(ctx, map[string]any{
			"credential_id": "cred-create-paused",
			"slots":         validSlotsJSON,
			"status":        "paused",
		})
		if err != nil {
			t.Fatalf("MCPCreate: %v", err)
		}
		if got := result.(PublicationSchedule).Status; got != ScheduleStatusPaused {
			t.Errorf("Status = %q, want paused", got)
		}
	})
}

// ─── MCPUpdate ──────────────────────────────────────────────────────────────

func TestScheduleModule_MCPUpdate(t *testing.T) {
	ctx := scheduleTestAdminCtx()

	t.Run("get error on unknown ID", func(t *testing.T) {
		s, _ := newScheduleTestSocial(t)
		sm := s.ScheduleModule()
		_, err := sm.MCPUpdate(ctx, "does-not-exist", map[string]any{"slots": validSlotsJSON})
		if !errors.Is(err, smeldr.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("malformed slots JSON", func(t *testing.T) {
		s, _ := newScheduleTestSocial(t)
		sm := s.ScheduleModule()
		created, err := sm.MCPCreate(ctx, map[string]any{"credential_id": "cred-up-1", "slots": validSlotsJSON})
		if err != nil {
			t.Fatalf("MCPCreate: %v", err)
		}
		id := created.(PublicationSchedule).ID
		_, err = sm.MCPUpdate(ctx, id, map[string]any{"slots": "not-json"})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("validateSlots rejects", func(t *testing.T) {
		s, _ := newScheduleTestSocial(t)
		sm := s.ScheduleModule()
		created, err := sm.MCPCreate(ctx, map[string]any{"credential_id": "cred-up-2", "slots": validSlotsJSON})
		if err != nil {
			t.Fatalf("MCPCreate: %v", err)
		}
		id := created.(PublicationSchedule).ID
		_, err = sm.MCPUpdate(ctx, id, map[string]any{
			"slots": `[{"weekday":1,"time":"25:99","timezone":"UTC"}]`,
		})
		if err == nil {
			t.Fatal("expected error for malformed time")
		}
	})

	t.Run("invalid status value", func(t *testing.T) {
		s, _ := newScheduleTestSocial(t)
		sm := s.ScheduleModule()
		created, err := sm.MCPCreate(ctx, map[string]any{"credential_id": "cred-up-3", "slots": validSlotsJSON})
		if err != nil {
			t.Fatalf("MCPCreate: %v", err)
		}
		id := created.(PublicationSchedule).ID
		_, err = sm.MCPUpdate(ctx, id, map[string]any{"status": "bogus"})
		if err == nil {
			t.Fatal("expected error for invalid status")
		}
	})

	t.Run("get error on unknown ID before update is attempted", func(t *testing.T) {
		// Dropping the table entirely would fail MCPUpdate's own initial
		// getSchedule call, never reaching updateSchedule — see the
		// dedicated "DB update error" test below for isolating that call.
		s, db := newScheduleTestSocial(t)
		sm := s.ScheduleModule()
		if _, err := db.Exec("DROP TABLE smeldr_social_publication_schedules"); err != nil {
			t.Fatalf("drop table: %v", err)
		}
		_, err := sm.MCPUpdate(ctx, "any-id", map[string]any{"status": "paused"})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("DB update error", func(t *testing.T) {
		s, db := newScheduleTestSocial(t)
		sm := s.ScheduleModule()
		created, err := sm.MCPCreate(ctx, map[string]any{"credential_id": "cred-up-4", "slots": validSlotsJSON})
		if err != nil {
			t.Fatalf("MCPCreate: %v", err)
		}
		id := created.(PublicationSchedule).ID
		// Fail only the Nth ExecContext call so getSchedule's own
		// QueryRowContext still succeeds and updateSchedule's UPDATE is the
		// one that fails — isolating updateSchedule's own error branch.
		s.db = &nthExecFailDB{DB: db, fail: 1}
		_, err = sm.MCPUpdate(ctx, id, map[string]any{"status": "paused"})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("success", func(t *testing.T) {
		s, _ := newScheduleTestSocial(t)
		sm := s.ScheduleModule()
		created, err := sm.MCPCreate(ctx, map[string]any{"credential_id": "cred-up-5", "slots": validSlotsJSON})
		if err != nil {
			t.Fatalf("MCPCreate: %v", err)
		}
		id := created.(PublicationSchedule).ID

		result, err := sm.MCPUpdate(ctx, id, map[string]any{
			"slots":  `[{"weekday":3,"time":"14:15","timezone":"UTC"}]`,
			"status": "paused",
		})
		if err != nil {
			t.Fatalf("MCPUpdate: %v", err)
		}
		ps := result.(PublicationSchedule)
		if ps.Status != ScheduleStatusPaused {
			t.Errorf("Status = %q, want paused", ps.Status)
		}
		if len(ps.Slots) != 1 || ps.Slots[0].Weekday != 3 {
			t.Errorf("Slots = %+v", ps.Slots)
		}

		got, err := sm.MCPGet(ctx, id)
		if err != nil {
			t.Fatalf("MCPGet after update: %v", err)
		}
		if got.(PublicationSchedule).Status != ScheduleStatusPaused {
			t.Error("update not persisted")
		}
	})
}

// ─── MCPList / MCPGet / MCPDelete ──────────────────────────────────────────

func TestScheduleModule_MCPList(t *testing.T) {
	s, _ := newScheduleTestSocial(t)
	sm := s.ScheduleModule()
	ctx := scheduleTestAdminCtx()

	if _, err := sm.MCPCreate(ctx, map[string]any{"credential_id": "cred-l-1", "slots": validSlotsJSON}); err != nil {
		t.Fatalf("MCPCreate 1: %v", err)
	}
	if _, err := sm.MCPCreate(ctx, map[string]any{"credential_id": "cred-l-2", "slots": validSlotsJSON}); err != nil {
		t.Fatalf("MCPCreate 2: %v", err)
	}

	items, err := sm.MCPList(ctx)
	if err != nil {
		t.Fatalf("MCPList: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("len = %d, want 2", len(items))
	}
}

func TestScheduleModule_MCPList_Error(t *testing.T) {
	s, db := newScheduleTestSocial(t)
	sm := s.ScheduleModule()
	if _, err := db.Exec("DROP TABLE smeldr_social_publication_schedules"); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, err := sm.MCPList(scheduleTestAdminCtx()); err == nil {
		t.Fatal("expected error")
	}
}

func TestScheduleModule_MCPGet(t *testing.T) {
	s, _ := newScheduleTestSocial(t)
	sm := s.ScheduleModule()
	ctx := scheduleTestAdminCtx()

	t.Run("not found", func(t *testing.T) {
		_, err := sm.MCPGet(ctx, "does-not-exist")
		if !errors.Is(err, smeldr.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("success", func(t *testing.T) {
		created, err := sm.MCPCreate(ctx, map[string]any{"credential_id": "cred-g-1", "slots": validSlotsJSON})
		if err != nil {
			t.Fatalf("MCPCreate: %v", err)
		}
		id := created.(PublicationSchedule).ID
		got, err := sm.MCPGet(ctx, id)
		if err != nil {
			t.Fatalf("MCPGet: %v", err)
		}
		if got.(PublicationSchedule).ID != id {
			t.Error("ID mismatch")
		}
	})
}

func TestScheduleModule_MCPDelete(t *testing.T) {
	s, _ := newScheduleTestSocial(t)
	sm := s.ScheduleModule()
	ctx := scheduleTestAdminCtx()

	created, err := sm.MCPCreate(ctx, map[string]any{"credential_id": "cred-d-1", "slots": validSlotsJSON})
	if err != nil {
		t.Fatalf("MCPCreate: %v", err)
	}
	id := created.(PublicationSchedule).ID

	if err := sm.MCPDelete(ctx, id); err != nil {
		t.Fatalf("MCPDelete: %v", err)
	}
	if _, err := sm.MCPGet(ctx, id); !errors.Is(err, smeldr.ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}

// ─── MCPPublish / MCPSchedule / MCPArchive — unsupported ──────────────────

func TestScheduleModule_UnsupportedLifecycleOps(t *testing.T) {
	s, _ := newScheduleTestSocial(t)
	sm := s.ScheduleModule()
	ctx := scheduleTestAdminCtx()

	if err := sm.MCPPublish(ctx, "any-id", ""); !errors.Is(err, smeldr.ErrBadRequest) {
		t.Errorf("MCPPublish err = %v, want ErrBadRequest", err)
	}
	if err := sm.MCPSchedule(ctx, "any-id", time.Now(), ""); !errors.Is(err, smeldr.ErrBadRequest) {
		t.Errorf("MCPSchedule err = %v, want ErrBadRequest", err)
	}
	if err := sm.MCPArchive(ctx, "any-id", ""); !errors.Is(err, smeldr.ErrBadRequest) {
		t.Errorf("MCPArchive err = %v, want ErrBadRequest", err)
	}
}

// ─── validateSlots ──────────────────────────────────────────────────────────

func TestValidateSlots(t *testing.T) {
	t.Run("empty timezone", func(t *testing.T) {
		err := validateSlots([]Slot{{Weekday: 1, Time: "09:00", Timezone: ""}})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("weekday out of range", func(t *testing.T) {
		err := validateSlots([]Slot{{Weekday: 7, Time: "09:00", Timezone: "UTC"}})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("weekday negative", func(t *testing.T) {
		err := validateSlots([]Slot{{Weekday: -1, Time: "09:00", Timezone: "UTC"}})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("malformed HH:MM", func(t *testing.T) {
		err := validateSlots([]Slot{{Weekday: 1, Time: "not-a-time", Timezone: "UTC"}})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("HH:MM out of numeric range", func(t *testing.T) {
		err := validateSlots([]Slot{{Weekday: 1, Time: "25:99", Timezone: "UTC"}})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("invalid IANA timezone", func(t *testing.T) {
		err := validateSlots([]Slot{{Weekday: 1, Time: "09:00", Timezone: "Not/ARealZone"}})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("valid slot", func(t *testing.T) {
		err := validateSlots([]Slot{{Weekday: 1, Time: "09:00", Timezone: "Europe/Copenhagen"}})
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// ─── MCPMeta / MCPSchema — smoke test ──────────────────────────────────────

func TestScheduleModule_MetaAndSchema(t *testing.T) {
	s, _ := newScheduleTestSocial(t)
	sm := s.ScheduleModule()

	meta := sm.MCPMeta()
	if meta.TypeName == "" {
		t.Error("MCPMeta.TypeName is empty")
	}
	if meta.Prefix == "" {
		t.Error("MCPMeta.Prefix is empty")
	}
	if len(meta.Operations) == 0 {
		t.Error("MCPMeta.Operations is empty")
	}

	schema := sm.MCPSchema()
	if len(schema) == 0 {
		t.Fatal("MCPSchema is empty")
	}
	foundCredentialID := false
	for _, f := range schema {
		if f.JSONName == "credential_id" {
			foundCredentialID = true
		}
	}
	if !foundCredentialID {
		t.Error("MCPSchema missing credential_id field")
	}
}
