package forgesocial

import (
	"context"
	"errors"
	"log"
	"time"

	forge "forge-cms.dev/forge"
)

// schedulerFallbackInterval is the maximum time the scheduler waits between
// polls when no posts are scheduled.
const schedulerFallbackInterval = 60 * time.Second

// retryDelays defines the backoff durations for each retry attempt (0-indexed).
// Attempts beyond len(retryDelays)-1 are terminal.
var retryDelays = []time.Duration{
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
	1 * time.Hour,
}

// scheduler manages the goroutine that polls for due ScheduledPosts and
// publishes them. It is owned by Social and started via Register.
type scheduler struct {
	social *Social
	cancel context.CancelFunc
	done   chan struct{}
}

func newScheduler(social *Social) *scheduler {
	return &scheduler{social: social, done: make(chan struct{})}
}

// start launches the scheduler goroutine.
func (sc *scheduler) start() {
	ctx, cancel := context.WithCancel(context.Background())
	sc.cancel = cancel
	go sc.run(ctx)
}

// stop cancels the scheduler context and waits for the goroutine to exit.
// It is a no-op if start has not been called.
func (sc *scheduler) stop() {
	if sc.cancel == nil {
		return // start was never called
	}
	sc.cancel()
	<-sc.done
}

// run is the main scheduler loop. It sleeps until the next due post (or the
// fallback interval), then processes all due posts.
func (sc *scheduler) run(ctx context.Context) {
	defer close(sc.done)

	for {
		interval := sc.nextInterval()

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}

		sc.processDue(ctx)
	}
}

// nextInterval returns how long to sleep before the next scheduler poll.
// It queries the DB for the earliest scheduled_at; if that time is in the
// past (or now), it returns 0 to trigger immediate processing.
func (sc *scheduler) nextInterval() time.Duration {
	t, err := nextScheduledAt(sc.social.db)
	if err != nil || t == nil {
		return schedulerFallbackInterval
	}
	d := time.Until(*t)
	if d <= 0 {
		return 0
	}
	if d > schedulerFallbackInterval {
		return schedulerFallbackInterval
	}
	return d
}

// processDue fetches all due posts and publishes each one.
// It also processes queued posts for any active PublicationSchedule whose
// slots have fired since the last tick.
func (sc *scheduler) processDue(ctx context.Context) {
	posts, err := duePosts(sc.social.db)
	if err != nil {
		log.Printf("forgesocial: scheduler: query due posts: %v", err)
		return
	}
	for _, p := range posts {
		if ctx.Err() != nil {
			return
		}
		sc.publishWithRetry(ctx, p)
	}

	sc.processSlotQueue(ctx)

	_ = purgeExpiredOAuthStates(sc.social.db)
}

// processSlotQueue iterates over all active PublicationSchedules, determines
// which slots have fired since the schedule's last_tick_at, and for each
// fired slot dequeues the oldest queued post for that credential and publishes it.
// At most len(slots) posts are published per tick to avoid flooding after downtime.
func (sc *scheduler) processSlotQueue(ctx context.Context) {
	now := time.Now().UTC()

	schedules, err := listActiveSchedules(sc.social.db)
	if err != nil {
		log.Printf("forgesocial: scheduler: list active schedules: %v", err)
		return
	}

	for _, sched := range schedules {
		if ctx.Err() != nil {
			return
		}

		// Determine the window start: last_tick_at if set, else created_at.
		from := sched.CreatedAt
		if sched.LastTickAt != nil {
			from = *sched.LastTickAt
		}

		// Always advance last_tick_at, even if no slot fires.
		if err := updateScheduleLastTick(sc.social.db, sched.ID, now); err != nil {
			log.Printf("forgesocial: scheduler: update last_tick_at for schedule %s: %v", sched.ID, err)
		}

		if len(sched.Slots) == 0 {
			continue
		}

		fireCounts := firedSlotsBetween(sched, from, now)

		// Cap: at most len(slots) posts published per tick.
		published := 0
		cap := len(sched.Slots)

		for _, count := range fireCounts {
			for i := 0; i < count && published < cap; i++ {
				if ctx.Err() != nil {
					return
				}
				p, err := dequeueOldestQueued(sc.social.db, sched.CredentialID)
				if errors.Is(err, forge.ErrNotFound) {
					break // queue empty for this credential
				}
				if err != nil {
					log.Printf("forgesocial: scheduler: dequeue for credential %s: %v", sched.CredentialID, err)
					break
				}
				sc.publishWithRetry(ctx, p)
				published++
			}
		}
	}
}

// publishWithRetry attempts to publish post p. If the publish fails with a
// transient error, it records the attempt and re-queues via backoff; terminal
// errors mark the post as failed immediately.
func (sc *scheduler) publishWithRetry(ctx context.Context, p ScheduledPost) {
	s := sc.social

	attempts, err := deliveryAttemptCount(s.db, p.ID)
	if err != nil {
		log.Printf("forgesocial: scheduler: count attempts for %s: %v", p.ID, err)
		return
	}

	// Fetch decrypted credential.
	cred, err := s.creds.getCredential(p.CredentialID)
	if err != nil {
		log.Printf("forgesocial: scheduler: get credential for post %s: %v", p.ID, err)
		_ = markPostFailed(s.db, p.ID, "credential not found: "+err.Error())
		return
	}

	platformID, publishErr := s.callPlatformPublish(ctx, p, cred)

	if publishErr == nil {
		// Success.
		_ = logDeliveryAttempt(s.db, p.ID, attempts+1, 200, "")
		if err := markPostPublished(s.db, p.ID, platformID); err != nil {
			log.Printf("forgesocial: scheduler: mark published %s: %v", p.ID, err)
		}
		return
	}

	// Log the failed attempt.
	statusCode := 0
	var pe *publishError
	var rle *rateLimitError
	switch {
	case errors.As(publishErr, &pe):
		statusCode = pe.statusCode
	case errors.As(publishErr, &rle):
		statusCode = 429
	}
	_ = logDeliveryAttempt(s.db, p.ID, attempts+1, statusCode, publishErr.Error())

	// Terminal error — stop retrying.
	if (errors.As(publishErr, &pe) && pe.IsTerminal()) || attempts+1 >= len(retryDelays)+1 {
		log.Printf("forgesocial: scheduler: terminal failure for post %s: %v", p.ID, publishErr)
		_ = markPostFailed(s.db, p.ID, publishErr.Error())
		return
	}

	// Transient error — re-schedule using backoff.
	var delay time.Duration
	if errors.As(publishErr, &rle) {
		delay = rle.retryAfter
	} else {
		idx := attempts
		if idx >= len(retryDelays) {
			idx = len(retryDelays) - 1
		}
		delay = retryDelays[idx]
	}

	retryAt := time.Now().UTC().Add(delay)
	p.ScheduledAt = &retryAt
	if err := updatePost(s.db, p); err != nil {
		log.Printf("forgesocial: scheduler: update retry time for %s: %v", p.ID, err)
	}
}

// publishNow publishes post p immediately, bypassing the scheduler.
// It fetches the credential, dispatches to the correct platform client,
// and updates the post status. Used by MCPPublish.
func (s *Social) publishNow(ctx context.Context, p ScheduledPost) error {
	cred, err := s.creds.getCredential(p.CredentialID)
	if err != nil {
		return err
	}
	platformID, err := s.callPlatformPublish(ctx, p, cred)
	if err != nil {
		_ = markPostFailed(s.db, p.ID, err.Error())
		return err
	}
	return markPostPublished(s.db, p.ID, platformID)
}

// callPlatformPublish dispatches a publish call to the correct platform client
// based on the post's Platform field.
func (s *Social) callPlatformPublish(ctx context.Context, p ScheduledPost, cred PlatformCredential) (string, error) {
	switch p.Platform {
	case "mastodon":
		return s.mastodon.publish(ctx, p, cred)
	case "linkedin":
		if s.linkedin == nil {
			return "", &publishError{msg: "LinkedIn is not configured on this server", terminal: true}
		}
		return s.linkedin.publish(ctx, p, cred)
	default:
		return "", &publishError{msg: "unknown platform: " + p.Platform, terminal: true}
	}
}
