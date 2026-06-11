package social

import (
	"context"
	"net/http"

	"smeldr.dev/core"
)

// Export internal functions for white-box testing.
// This file is compiled into the package (not the test binary) so that
// external test packages (package social_test) can call them.

// ValidateAgentURLForTest calls validateAgentURL and panics on error.
// Used in tests to exercise SSRF rejection without going through AddRoutes.
func ValidateAgentURLForTest(url string) {
	if err := validateAgentURL(url); err != nil {
		panic(err.Error())
	}
}

// ValidateRouteForTest calls validateRoute. Used in tests to exercise
// route validation panics directly.
func ValidateRouteForTest(r Route) {
	validateRoute(r)
}

// RouteJobStoreForTest wraps routeJobStore for test access.
type RouteJobStoreForTest struct {
	inner *routeJobStore
}

// NewRouteJobStoreForTest creates a RouteJobStoreForTest backed by db.
func NewRouteJobStoreForTest(db smeldr.DB) *RouteJobStoreForTest {
	return &RouteJobStoreForTest{inner: &routeJobStore{db: db}}
}

// EnqueueForTest enqueues one job for the given route, signal, and event.
func (s *RouteJobStoreForTest) EnqueueForTest(r Route, sig smeldr.Signal, ev smeldr.SignalEvent) {
	s.inner.enqueue(r, sig, ev)
}

// DueJobsForTest returns all currently-due jobs.
func (s *RouteJobStoreForTest) DueJobsForTest(ctx context.Context) ([]routeJob, error) {
	return s.inner.dueJobs(ctx)
}

// MarkDeliveredForTest marks the job as delivered.
func (s *RouteJobStoreForTest) MarkDeliveredForTest(ctx context.Context, id string) error {
	return s.inner.markDelivered(ctx, id)
}

// RunDeliveryForTest runs one delivery pass: fetches due jobs from db and
// delivers each one using the provided http.Client and secret.
// Designed for use in tests with httptest.Server clients.
func RunDeliveryForTest(db smeldr.DB, secret []byte, hc *http.Client) {
	store := &routeJobStore{db: db}
	router := &Router{
		jobs:   store,
		secret: secret,
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}

	jobs, err := store.dueJobs(context.Background())
	if err != nil {
		return
	}
	for _, j := range jobs {
		router.deliver(hc, j)
	}
}

// GetFailedJobsForTest returns all route jobs with status='failed'.
func GetFailedJobsForTest(db smeldr.DB) []routeJob {
	rows, err := db.QueryContext(context.Background(),
		`SELECT id, signal, content_type, agent_url, payload, status, attempts, next_attempt, last_error, created_at
		 FROM smeldr_social_route_jobs WHERE status = 'failed'`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var jobs []routeJob
	for rows.Next() {
		var j routeJob
		_ = rows.Scan(&j.ID, &j.Signal, &j.ContentType, &j.AgentURL,
			&j.Payload, &j.Status, &j.Attempts, &j.NextAttempt, &j.LastError, &j.CreatedAt)
		jobs = append(jobs, j)
	}
	return jobs
}
