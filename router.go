package forgesocial

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	forge "forge-cms.dev/forge"
)

// Router holds the registered routes and the route delivery worker.
// It is created by [AddRoutes] and owned by [Social].
type Router struct {
	routes map[forge.Signal][]Route // keyed by signal; read-only after AddRoutes
	jobs   *routeJobStore
	secret []byte
	stopCh chan struct{} // closed by stopWorker to signal the goroutine to exit
	done   chan struct{} // closed by the goroutine when it exits
}

// routeJob is a pending or completed outbound delivery job.
type routeJob struct {
	ID          string
	Signal      string
	ContentType string
	AgentURL    string
	Payload     string // raw JSON
	Status      string // pending / delivered / failed
	Attempts    int
	NextAttempt *time.Time
	LastError   string
	CreatedAt   time.Time
}

// routeJobStore wraps DB operations for forge_social_route_jobs.
type routeJobStore struct {
	db forge.DB
}

// enqueue inserts a pending delivery job for route. sig is the matched signal.
// Errors are logged without returning — partial fan-out is preferred over aborting.
func (s *routeJobStore) enqueue(route Route, sig forge.Signal, ev forge.SignalEvent) {
	payload, err := json.Marshal(ev)
	if err != nil {
		log.Printf("forgesocial: router: marshal signal event: %v", err)
		return
	}
	now := time.Now().UTC()
	_, err = s.db.ExecContext(context.Background(),
		`INSERT INTO forge_social_route_jobs
		 (id, signal, content_type, agent_url, payload, status, attempts, next_attempt, last_error, created_at)
		 VALUES (?, ?, ?, ?, ?, 'pending', 0, ?, '', ?)`,
		forge.NewID(),
		string(sig),
		route.ContentType,
		route.AgentURL,
		string(payload),
		now, // next_attempt = now (due immediately)
		now,
	)
	if err != nil {
		log.Printf("forgesocial: router: enqueue job for %s/%s → %s: %v",
			route.Signal, route.ContentType, route.AgentURL, err)
	}
}

// dueJobs returns up to 50 pending jobs that are due for delivery.
func (s *routeJobStore) dueJobs(ctx context.Context) ([]routeJob, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, signal, content_type, agent_url, payload, status, attempts, next_attempt, last_error, created_at
		 FROM forge_social_route_jobs
		 WHERE status = 'pending' AND (next_attempt IS NULL OR next_attempt <= ?)
		 ORDER BY next_attempt ASC
		 LIMIT 50`,
		time.Now().UTC(),
	)
	if err != nil {
		return nil, fmt.Errorf("forgesocial: route jobs query: %w", err)
	}
	defer rows.Close()

	var jobs []routeJob
	for rows.Next() {
		var j routeJob
		if err := rows.Scan(&j.ID, &j.Signal, &j.ContentType, &j.AgentURL,
			&j.Payload, &j.Status, &j.Attempts, &j.NextAttempt, &j.LastError, &j.CreatedAt); err != nil {
			return nil, fmt.Errorf("forgesocial: route jobs scan: %w", err)
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// markDelivered marks a job as successfully delivered.
func (s *routeJobStore) markDelivered(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE forge_social_route_jobs SET status = 'delivered', last_error = '' WHERE id = ?`, id)
	return err
}

// markFailed marks a job as terminally failed.
func (s *routeJobStore) markFailed(ctx context.Context, id string, msg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE forge_social_route_jobs SET status = 'failed', last_error = ? WHERE id = ?`, msg, id)
	return err
}

// scheduleRetry increments attempts and sets next_attempt for the next retry.
func (s *routeJobStore) scheduleRetry(ctx context.Context, id string, attempts int, nextAt time.Time, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE forge_social_route_jobs SET attempts = ?, next_attempt = ?, last_error = ? WHERE id = ?`,
		attempts, nextAt, errMsg, id)
	return err
}

// logAttempt records one delivery attempt in forge_social_route_log.
func (s *routeJobStore) logAttempt(ctx context.Context, jobID string, attempt, statusCode int, errMsg string) {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO forge_social_route_log (id, job_id, attempt, status_code, error, attempted_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		forge.NewID(), jobID, attempt, statusCode, errMsg, time.Now().UTC(),
	)
	if err != nil {
		log.Printf("forgesocial: router: log attempt for job %s: %v", jobID, err)
	}
}

// handle returns a signal bus callback for the given signal. It is called from
// the forge App's signal bus goroutine and must return quickly. It looks up
// matching routes for (sig, ev.Type) and enqueues one DB job per match.
func (r *Router) handle(sig forge.Signal) func(context.Context, forge.SignalEvent) error {
	return func(_ context.Context, ev forge.SignalEvent) error {
		candidates := r.routes[sig]
		for _, route := range candidates {
			if route.ContentType == ev.Type {
				r.jobs.enqueue(route, sig, ev)
			}
		}
		return nil
	}
}

// AddRoutes registers routes on the Social instance and wires each unique
// signal to the Forge App's signal bus. It also starts the route delivery
// worker goroutine.
//
// Call AddRoutes before [app.Run]. Panics if any route has an invalid
// ContentType or AgentURL (see [Route] for validation rules).
//
// Layer 1 (AddRoutes) and Layer 2 (Register) are independent — you may call
// either, both, or neither.
func (s *Social) AddRoutes(app *forge.App, routes ...Route) {
	if len(routes) == 0 {
		return
	}

	// Validate all routes up front so startup fails fast on misconfiguration.
	for _, r := range routes {
		validateRoute(r)
	}

	// Build the signal→routes map. Multiple routes with the same signal are
	// collected into a single slice for O(1) dispatch lookup.
	bySignal := make(map[forge.Signal][]Route)
	for _, r := range routes {
		bySignal[r.Signal] = append(bySignal[r.Signal], r)
	}

	router := &Router{
		routes: bySignal,
		jobs:   &routeJobStore{db: s.db},
		secret: s.cfg.Secret,
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}
	s.router = router

	// Register one signal handler per unique signal. The handler fans out to
	// all matching (signal, contentType) routes.
	for sig := range bySignal {
		app.OnSignal(sig, router.handle(sig))
	}

	// Start the delivery worker goroutine.
	go router.runWorker()
}
