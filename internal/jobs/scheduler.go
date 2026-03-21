package jobs

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Scheduler runs background jobs on configurable intervals.
type Scheduler struct {
	rollups   *RollupService
	retention *RetentionService
	logger    *slog.Logger
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

func NewScheduler(rollups *RollupService, retention *RetentionService, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		rollups:   rollups,
		retention: retention,
		logger:    logger,
	}
}

// Start launches all background job goroutines.
func (s *Scheduler) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	s.runPeriodic(ctx, "session-rollup", 5*time.Minute, s.rollups.RunSessionRollup)
	s.runPeriodic(ctx, "hourly-rollup", 1*time.Hour, s.rollups.RunHourlyRollup)
	s.runPeriodic(ctx, "daily-rollup", 24*time.Hour, s.rollups.RunDailyRollup)
	s.runPeriodic(ctx, "retention-cleanup", 24*time.Hour, s.retention.RunCleanup)
}

// Stop cancels all jobs and waits for them to finish.
func (s *Scheduler) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

func (s *Scheduler) runPeriodic(ctx context.Context, name string, interval time.Duration, fn func(context.Context) error) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// Run once on startup after a short delay
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := fn(ctx); err != nil {
				s.logger.Error("job failed", "job", name, "err", err)
			}
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := fn(ctx); err != nil {
					s.logger.Error("job failed", "job", name, "err", err)
				}
			}
		}
	}()
}
