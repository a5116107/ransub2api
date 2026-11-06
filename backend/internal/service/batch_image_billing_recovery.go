package service

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

const (
	defaultBatchImageBillingRecoveryStaleAfter = 10 * time.Minute
	defaultBatchImageBillingRecoveryLimit      = 100
)

type BatchImageBillingRecoveryService struct {
	Repo       BatchImageRepository
	Billing    UsageBillingRepository
	AuthCache  APIKeyAuthCacheInvalidator
	Queue      BatchImageQueue
	StaleAfter time.Duration
	Limit      int
}

func (s *BatchImageBillingRecoveryService) ReleaseStaleUnsubmittedOnce(ctx context.Context) (int, error) {
	if s == nil || s.Repo == nil || s.Billing == nil {
		return 0, nil
	}
	staleAfter := s.StaleAfter
	if staleAfter <= 0 {
		staleAfter = defaultBatchImageBillingRecoveryStaleAfter
	}
	limit := s.Limit
	if limit <= 0 {
		limit = defaultBatchImageBillingRecoveryLimit
	}
	cutoff := time.Now().Add(-staleAfter)
	jobs, err := s.Repo.ListStaleUnsubmittedBatchImageJobs(ctx, cutoff, limit)
	if err != nil {
		return 0, err
	}
	released := 0
	var lastErr error
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return released, err
		}
		msg := "batch image submission did not reach provider before recovery cutoff"
		// Only created proves dispatch has not started. uploading without a job
		// name can mean a create response was lost; age is not rejection proof.
		// The conditional update also checks a concurrent submit transition.
		applied, err := s.Repo.FailStaleUnsubmittedBatchImageJob(ctx, job.BatchID, cutoff, "SUBMIT_STALE_BEFORE_PROVIDER", msg)
		if err != nil {
			// applied=true 时 UPDATE 已提交（仅审计事件写入失败）：必须继续释放，
			// 否则 job 已转 failed、不再出现在 stale 列表，冻结余额会永久泄漏。
			if !applied {
				lastErr = err
				continue
			}
			logger.L().Warn("batch_image.recovery_fail_event_append_failed",
				zap.String("batch_id", job.BatchID),
				zap.Error(err),
			)
		}
		if !applied {
			continue
		}
		job.Status = BatchImageJobStatusFailed
		if err := releaseBatchImageBalanceHold(ctx, s.Billing, job, batchImageDerefString(job.RequestHash)); err != nil {
			// job 已转 failed、不会再进入 stale 列表：必须给释放失败留下
			// 自动重试路径（入队后由 worker 的 releaseTerminalHold 兜底），
			// 否则冻结余额永久泄漏。
			logger.L().Warn("batch_image.recovery_release_failed",
				zap.String("batch_id", job.BatchID),
				zap.Error(err),
			)
			s.enqueueReleaseRetry(ctx, job.BatchID)
			lastErr = err
			continue
		}
		if s.AuthCache != nil && job.UserID > 0 {
			s.AuthCache.InvalidateAuthCacheByUserID(ctx, job.UserID)
		}
		released++
	}
	return released, lastErr
}

func (s *BatchImageBillingRecoveryService) enqueueReleaseRetry(ctx context.Context, batchID string) {
	if s == nil || s.Queue == nil {
		return
	}
	if err := s.Queue.Enqueue(ctx, batchID); err != nil && !errors.Is(err, ErrBatchImageAlreadyQueued) {
		logger.L().Warn("batch_image.recovery_release_retry_enqueue_failed",
			zap.String("batch_id", batchID),
			zap.Error(err),
		)
	}
}
