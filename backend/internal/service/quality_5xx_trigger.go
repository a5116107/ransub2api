package service

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/qualityqueue"
)

const quality5xxSource = "upstream_5xx"
const quality5xxPendingKey = qualityqueue.PendingKey

type qualityProbeContextKey struct{}

// Redis bridges request-serving replicas and the existing singleton test worker.
// Only account IDs enter this bounded, coalescing queue; never upstream bodies.
type quality5xxImmediateRepository interface {
	ApplyQuality5xx(context.Context, int64) error
}

type quality5xxTrigger struct {
	immediate quality5xxImmediateRepository
	queue     qualityqueue.Queue
}

func (s *quality5xxTrigger) signalIsCurrent(accountID int64, observedAt time.Time) bool {
	if s == nil || s.queue == nil || accountID <= 0 {
		return true
	}
	score, err := s.queue.Score(context.Background(), accountID)
	if err != nil {
		// Redis loss must fail closed: an old successful probe must not release
		// quarantine when the fencing signal cannot be read.
		return false
	}
	return !score.After(observedAt)
}

func (s *quality5xxTrigger) Observe(ctx context.Context, account *Account, status int) {
	if s == nil || account == nil || account.ID <= 0 || (account.Type != AccountTypeOAuth && account.Type != AccountTypeAPIKey) || status < 500 || status > 599 || ctx.Value(qualityProbeContextKey{}) != nil {
		return
	}
	// Complete native account protection before retry/selection can continue.
	// This has no dependency on worker slots or a running probe's lease.
	if s.immediate != nil {
		actionCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		err := s.immediate.ApplyQuality5xx(actionCtx, account.ID)
		stop()
		if err != nil {
			slog.Warn("quality 5xx immediate protection failed", "account_id", account.ID, "error", err)
		}
	}
	if s.queue == nil {
		return // The database due time remains the durable probe fallback.
	}
	enqueueCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 150*time.Millisecond)
	defer cancel()
	if err := s.queue.Enqueue(enqueueCtx, account.ID, time.Now()); err != nil {
		slog.Warn("quality 5xx trigger could not be queued", "account_id", account.ID)
	}
}

func (s *ScheduledTestRunnerService) startQualityTriggers() {
	if s.qualityTrigger == nil || s.qualityTrigger.queue == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.triggerCancel = cancel
	s.triggerWG.Add(1)
	go func() {
		defer s.triggerWG.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var active sync.Map
		slots := make(chan struct{}, scheduledTestDefaultMaxWorkers)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			scanCtx, stop := context.WithTimeout(ctx, 2*time.Second)
			queue := s.qualityTrigger.queue
			pending, err := queue.Pending(scanCtx, time.Now().Add(-20*time.Minute))
			stop()
			if err != nil {
				continue
			}
			for _, entry := range pending {
				id := entry.AccountID
				if _, loaded := active.LoadOrStore(id, true); loaded {
					continue
				}
				select {
				case slots <- struct{}{}:
				default:
					active.Delete(id)
					continue
				}
				s.triggerWG.Add(1)
				go func(id int64, entry qualityqueue.Signal) {
					defer s.triggerWG.Done()
					defer func() { <-slots; active.Delete(id) }()
					runCtx, cancel := context.WithTimeout(ctx, 12*time.Minute)
					defer cancel()
					if err := s.runQualityTriggeredAccount(runCtx, id, entry.ObservedAt); err != nil {
						return
					}
					if runCtx.Err() != nil {
						return
					}
					// Do not remove a newer signal that arrived after queue expiry.
					_ = queue.Acknowledge(runCtx, entry)
				}(id, entry)
			}
		}
	}()
}

func (s *ScheduledTestRunnerService) runQualityTriggeredAccount(ctx context.Context, accountID int64, observedAt time.Time) error {
	plans, err := s.planRepo.ListByAccountID(ctx, accountID)
	if err != nil {
		return err
	}
	// runPelicanPlan authoritatively loads the account and respects its native
	// cooldown before claiming. Do not duplicate that read or create a new
	// account-wide quarantine here.
	for _, plan := range plans {
		if plan == nil || !plan.Enabled || !plan.PelicanConfig.TriggersOnUpstream5xx() {
			continue
		}
		plan.TriggerSource = quality5xxSource
		plan.TriggerObservedAt = &observedAt
		if qualitySignalCovered(plan, observedAt) {
			continue // An overlapping scheduled/event test already covered this signal.
		}
		if plan.RunningUntil != nil && plan.RunningUntil.After(time.Now()) {
			return fmt.Errorf("quality plan %d is still running", plan.ID)
		}
		if !s.runOnePlan(ctx, plan) {
			// A lost claim can mean an actual running test OR only a transient
			// advisory-lock conflict. Re-read before acknowledging this signal.
			current, err := s.planRepo.GetByID(ctx, plan.ID)
			if err != nil {
				return err
			}
			if current == nil || !current.Enabled || !current.PelicanConfig.TriggersOnUpstream5xx() {
				continue
			}
			if current.RunningUntil != nil && current.RunningUntil.After(time.Now()) {
				return fmt.Errorf("quality plan %d is still running", plan.ID)
			}
			if qualitySignalCovered(current, observedAt) {
				continue
			}
			return fmt.Errorf("quality plan %d could not be claimed", plan.ID)
		}
	}
	return nil
}

// Called immediately after real transport responses, before protocol retry or
// status mapping. Stream failures also report their classified status separately.
func (s *RateLimitService) observeQualityResponse(ctx context.Context, account *Account, response *http.Response, err error) {
	if s == nil || err != nil || response == nil {
		return
	}
	s.observeQualityStatus(ctx, account, response.StatusCode)
}

// Both transport status and semantic failure status feed the same coalescing
// queue. Probe context is preserved to prevent recursive tests.
func (s *RateLimitService) observeQualityStatus(ctx context.Context, account *Account, status int) {
	if s != nil {
		s.qualityTrigger.Observe(ctx, account, status)
	}
}

// Select an execution-only test for the account's actual model route. Never
// bypass BPS to test direct ticket state when diagnosing the BPS route itself.
func quality5xxTestConfig(account *Account, model string, cfg PelicanTestConfig) (PelicanTestConfig, bool) {
	if cfg.Availability != nil {
		// A ticket state probe and a substituted question cannot confirm the
		// configured answer route. Preserve samples, prompt and reasoning.
		return cfg, cfg.Quality != nil && cfg.Quality.TriggerOnUpstream5xx && !cfg.BPSRecoveryPending
	}
	cfg.ParallelCount = 1
	if cfg.QuestionKind == QualityRandomQuestionKind {
		// Preserve the configured external judge. Never substitute the public
		// fixed candy answer or bypass judging for a random reasoning rule.
		return cfg, !cfg.BPSRecoveryPending
	}
	if len(cfg.ModelIDs) <= 1 && openAICodexStateProbeUnsupportedReason(account, model, false) == "" {
		cfg.QuestionKind = OpenAICodexStateProbeQuestionKind
		return cfg, true
	}
	cfg.QuestionKind = "candy"
	cfg.Prompt = CandyPrompt
	applyOutcome := true
	if cfg.Quality != nil {
		policy := *cfg.Quality
		policy.ExpectedAnswer = "21"
		policy.Judge = nil
		cfg.Quality = &policy
		// Only a direct state probe may advance an owned BPS recovery rule.
		// A correct BPS candy answer cannot prove direct routing has recovered.
		applyOutcome = policy.Action != QualityActionEnableBPS && !cfg.BPSRecoveryPending
	}
	return cfg, applyOutcome
}

func qualitySignalCovered(plan *ScheduledTestPlan, observedAt time.Time) bool {
	if plan.PelicanConfig != nil && plan.PelicanConfig.Availability != nil {
		return false // An old round's finish time does not prove its fault version.
	}
	if plan.PelicanConfig != nil && plan.PelicanConfig.Quality != nil && plan.PelicanConfig.Quality.Action == QualityActionRemoveModel {
		return false
	}
	return plan.LastRunAt != nil && !plan.LastRunAt.Before(observedAt)
}
