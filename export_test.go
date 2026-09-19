package social

import (
	"context"
	"net/http"
	"time"

	"smeldr.dev/core"
)

// PostHandlerForTest returns an http.Handler that routes the five REST
// endpoints to their handlers. Used in post_http_test.go to test handlers
// without a full smeldr.App.
func (s *Social) PostHandlerForTest() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /social/posts", s.handlePostCreate)
	mux.HandleFunc("GET /social/posts", s.handlePostList)
	mux.HandleFunc("GET /social/posts/{id}", s.handlePostGet)
	mux.HandleFunc("PUT /social/posts/{id}", s.handlePostUpdate)
	mux.HandleFunc("DELETE /social/posts/{id}", s.handlePostDelete)
	return mux
}

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

// OrderClauseForStatusesForTest calls orderClauseForStatuses.
func OrderClauseForStatusesForTest(statuses []PostStatus) string {
	return orderClauseForStatuses(statuses)
}

// InsertPostForTest calls insertPost.
func InsertPostForTest(db smeldr.DB, p ScheduledPost) error {
	return insertPost(db, p)
}

// ListPostsForTest calls listPosts.
func ListPostsForTest(db smeldr.DB, statuses ...PostStatus) ([]ScheduledPost, error) {
	return listPosts(db, statuses...)
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
func (s *RouteJobStoreForTest) EnqueueForTest(r Route, sig smeldr.LifecycleEvent, ev smeldr.SignalEvent) {
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

// RunWorkerPollOnceForTest performs a single poll+deliver pass identical to
// one iteration of runWorker's loop body — without the infinite loop or the
// goroutine — so a test can observe the poll-error branch directly. It
// returns the error from the dueJobs poll (if any); a nil error means the
// poll succeeded (any due jobs were also delivered).
func RunWorkerPollOnceForTest(db smeldr.DB, secret []byte, hc *http.Client) error {
	store := &routeJobStore{db: db}
	router := &Router{
		jobs:   store,
		secret: secret,
	}
	jobs, err := store.dueJobs(context.Background())
	if err != nil {
		return err
	}
	for _, j := range jobs {
		router.deliver(hc, j)
	}
	return nil
}

// ParseRouteRetryAfterForTest calls parseRouteRetryAfter.
func ParseRouteRetryAfterForTest(v string) time.Duration {
	return parseRouteRetryAfter(v)
}

// HandleForTest builds a throwaway *Router wired with route and returns the
// signal-bus callback exactly as [Social.AddRoutes] would register it via
// [Router.handle], so a test can invoke it directly without a full
// [smeldr.App].
func HandleForTest(db smeldr.DB, route Route) func(context.Context, smeldr.SignalEvent) error {
	router := &Router{
		routes: map[smeldr.LifecycleEvent][]Route{route.Signal: {route}},
		jobs:   &routeJobStore{db: db},
	}
	return router.handle(route.Signal)
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
