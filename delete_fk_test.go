package social

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"smeldr.dev/core"

	_ "modernc.org/sqlite"
)

func openInternalDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := CreateTables(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedPostWithLog(t *testing.T, db smeldr.DB, credID, postID string) {
	t.Helper()
	now := time.Now().UTC()
	if err := insertPost(db, ScheduledPost{ID: postID, Platform: "mastodon", CredentialID: credID, Body: "hi",
		Status: PostStatusDraft, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := logDeliveryAttempt(db, postID, 1, 500, "boom"); err != nil {
		t.Fatal(err)
	}
}

// deletePost removes the post and its delivery log in one go.
func TestDeletePost_RemovesItsDeliveryLog(t *testing.T) {
	db := openInternalDB(t)
	seedPostWithLog(t, db, "c1", "p1")
	if err := deletePost(db, "p1"); err != nil {
		t.Fatalf("deletePost: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM smeldr_social_delivery_log WHERE post_id = 'p1'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("delivery log rows = %d (%v); want 0", n, err)
	}
	if err := deletePost(db, "p1"); !errors.Is(err, smeldr.ErrNotFound) {
		t.Errorf("second delete = %v; want ErrNotFound", err)
	}
}

// A credential that posts still use is refused with ErrConflict (409 over
// HTTP), naming how many; once they are gone it is deleted.
func TestDeleteCredential_RefusedWhilePostsUseIt(t *testing.T) {
	db := openInternalDB(t)
	cs := newCredentialStore(db, []byte("test-secret-32-bytes-long-padded!"))
	id, err := cs.upsertCredentialByInstance("mastodon", "https://m.example", "me", "tok", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	seedPostWithLog(t, db, id, "p1")
	err = cs.deleteCredential(id)
	if !errors.Is(err, smeldr.ErrConflict) || !strings.Contains(err.Error(), "1 post(s)") {
		t.Fatalf("deleteCredential = %v; want ErrConflict naming 1 post", err)
	}
	w := httptest.NewRecorder()
	smeldr.WriteError(w, httptest.NewRequest(http.MethodDelete, "/", nil), err)
	if w.Code != http.StatusConflict {
		t.Errorf("HTTP status = %d; want 409", w.Code)
	}
	if err := deletePost(db, "p1"); err != nil {
		t.Fatal(err)
	}
	if err := cs.deleteCredential(id); err != nil {
		t.Errorf("deleteCredential after the post is gone = %v", err)
	}
	if err := cs.deleteCredential(id); !errors.Is(err, smeldr.ErrNotFound) {
		t.Errorf("second delete = %v; want ErrNotFound", err)
	}
}

// noTxDB hides BeginTx, so inTx runs on the database itself.
type noTxDB struct{ smeldr.DB }

// failExecDB fails every ExecContext.
type failExecDB struct{ *sql.DB }

func (failExecDB) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, errors.New("exec failed")
}

func TestInTx_Paths(t *testing.T) {
	db := openInternalDB(t)
	seedPostWithLog(t, db, "c1", "p1")
	if err := deletePost(noTxDB{db}, "p1"); err != nil {
		t.Errorf("deletePost without transactions = %v", err)
	}
	if err := deletePost(failExecDB{db}, "x"); err == nil {
		t.Error("deletePost with a failing exec = nil error")
	}
	closed, _ := sql.Open("sqlite", ":memory:")
	closed.Close()
	if err := inTx(context.Background(), closed, func(smeldr.DB) error { return nil }); err == nil {
		t.Error("inTx on a closed db = nil error; want the begin error")
	}
	want := errors.New("fn failed")
	if err := inTx(context.Background(), db, func(smeldr.DB) error { return want }); !errors.Is(err, want) {
		t.Errorf("inTx = %v; want fn's error", err)
	}
	cs := newCredentialStore(closed, []byte("test-secret-32-bytes-long-padded!"))
	if err := cs.deleteCredential("x"); err == nil {
		t.Error("deleteCredential on a closed db = nil error")
	}
}
