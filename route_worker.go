package forgesocial

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// routeWorkerInterval is how often the delivery worker polls for due jobs when
// it has nothing else to do.
const routeWorkerInterval = 15 * time.Second

// routeRetryDelays mirrors the Layer 2 backoff table.
// Attempts beyond len(routeRetryDelays)-1 are terminal.
var routeRetryDelays = []time.Duration{
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
	1 * time.Hour,
}

// runWorker is the route delivery worker goroutine. It polls for due jobs and
// delivers them. It exits when r.stopCh is closed.
// Call via go router.runWorker().
func (r *Router) runWorker() {
	defer close(r.done)

	hc := &http.Client{Timeout: 15 * time.Second}

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		jobs, err := r.jobs.dueJobs(ctx)
		cancel()
		if err != nil {
			log.Printf("forgesocial: route worker: poll: %v", err)
		}

		for _, j := range jobs {
			r.deliver(hc, j)
		}

		select {
		case <-r.stopCh:
			return
		case <-time.After(routeWorkerInterval):
		}
	}
}

// stopWorker closes stopCh to signal the worker goroutine to stop,
// then waits for the goroutine to exit.
func (r *Router) stopWorker() {
	close(r.stopCh)
	<-r.done
}

// deliver attempts to POST the job's payload to its agent URL. It updates the
// job status in the DB and logs the attempt.
func (r *Router) deliver(hc *http.Client, j routeJob) {
	ctx := context.Background()
	attempt := j.Attempts + 1

	payload := []byte(j.Payload)
	sig := routeHMAC(payload, r.secret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.AgentURL,
		bytes.NewReader(payload))
	if err != nil {
		// Permanent — URL is malformed (should never happen after validateRoute).
		r.jobs.logAttempt(ctx, j.ID, attempt, 0, err.Error())
		_ = r.jobs.markFailed(ctx, j.ID, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forge-Signature", "sha256="+sig)

	resp, err := hc.Do(req)
	if err != nil {
		// Network error — transient.
		msg := err.Error()
		r.jobs.logAttempt(ctx, j.ID, attempt, 0, msg)
		r.applyRetry(ctx, j, attempt, msg)
		return
	}
	defer resp.Body.Close()
	// Drain body to allow connection reuse.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	code := resp.StatusCode

	switch {
	case code >= 200 && code < 300:
		r.jobs.logAttempt(ctx, j.ID, attempt, code, "")
		if err := r.jobs.markDelivered(ctx, j.ID); err != nil {
			log.Printf("forgesocial: route worker: mark delivered %s: %v", j.ID, err)
		}

	case code == 429:
		// Rate limited — honour Retry-After if present.
		delay := parseRouteRetryAfter(resp.Header.Get("Retry-After"))
		if delay <= 0 {
			delay = routeRetryDelays[min(attempt-1, len(routeRetryDelays)-1)]
		}
		msg := fmt.Sprintf("429 Too Many Requests")
		r.jobs.logAttempt(ctx, j.ID, attempt, code, msg)
		nextAt := time.Now().UTC().Add(delay)
		_ = r.jobs.scheduleRetry(ctx, j.ID, attempt, nextAt, msg)

	case code >= 400 && code < 500:
		// Client error — terminal (agent rejected the payload).
		msg := fmt.Sprintf("HTTP %d", code)
		r.jobs.logAttempt(ctx, j.ID, attempt, code, msg)
		_ = r.jobs.markFailed(ctx, j.ID, msg)

	default:
		// 5xx or unexpected — transient.
		msg := fmt.Sprintf("HTTP %d", code)
		r.jobs.logAttempt(ctx, j.ID, attempt, code, msg)
		r.applyRetry(ctx, j, attempt, msg)
	}
}

// applyRetry schedules the next retry for j, or marks it terminal if all
// retry attempts are exhausted.
func (r *Router) applyRetry(ctx context.Context, j routeJob, attempt int, errMsg string) {
	if attempt > len(routeRetryDelays) {
		_ = r.jobs.markFailed(ctx, j.ID, "max retries exceeded: "+errMsg)
		return
	}
	delay := routeRetryDelays[attempt-1]
	nextAt := time.Now().UTC().Add(delay)
	_ = r.jobs.scheduleRetry(ctx, j.ID, attempt, nextAt, errMsg)
}

// routeHMAC returns the hex-encoded HMAC-SHA256 of payload using secret.
func routeHMAC(payload, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return fmt.Sprintf("%x", mac.Sum(nil))
}

// parseRouteRetryAfter parses the Retry-After header value. It handles both
// integer seconds and HTTP-date formats. Returns 0 if unparseable.
func parseRouteRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	// Try HTTP-date format.
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}
