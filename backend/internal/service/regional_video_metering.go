package service

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/regional"
	"github.com/Wei-Shaw/sub2api/internal/regional/ledger"
	"github.com/tidwall/gjson"
)

type regionalVideoTaskKey struct{}

var ErrRegionalVideoSettlement = errors.New("original video funds settlement unavailable")

type regionalVideoTask struct {
	ledger ledger.VideoTaskLedger
	record ledger.VideoTaskRecord
}

// A lookup never admits or starts another financial request. Its original SQL
// reservation and immutable price are loaded before sending to the provider.
// Settle before publishing the terminal response so errors retain a retriable
// query and cannot enter the legacy asynchronous billing claim.
func finishRegionalVideoLookup(req *http.Request, result *OpenAIForwardResult, endpoint GrokMediaEndpoint, body []byte) error {
	task, _ := req.Context().Value(regionalVideoTaskKey{}).(*regionalVideoTask)
	if task == nil {
		return nil
	}
	result.RegionalMetered = true
	result.AsyncVideoTerminal = false
	r := task.record.Reservation
	if r.Video == nil || r.VideoOrigin == nil || task.record.Price.Version() != r.PricingVersion || task.record.Price.Model != r.Model {
		return errors.Join(ErrRegionalVideoSettlement, ledger.ErrConflict)
	}
	settle, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), 5*time.Second)
	defer cancel()
	if task.record.State == "completed" {
		if task.record.Event == nil {
			return errors.Join(ErrRegionalVideoSettlement, ledger.ErrConflict)
		}
		if err := task.ledger.Complete(settle, *task.record.Event); err != nil {
			return errors.Join(ErrRegionalVideoSettlement, err)
		}
		result.AsyncVideoTerminal = true
		return nil
	}
	usage, terminal, err := regionalVideoTerminalUsage(task.record, result, endpoint, body)
	if err != nil || !terminal {
		return err
	}
	_, charge, err := task.record.Price.VideoCosts(usage)
	if err != nil || charge > r.Maximum {
		return errors.Join(ledger.ErrConflict, err)
	}
	completed := time.Now().UTC()
	if completed.Before(r.AdmittedAt) {
		completed = r.AdmittedAt
	}
	event := ledger.Event{
		Usage: regional.UsageEvent{SchemaVersion: regional.SchemaVersion, EventID: r.RequestID, LeaseID: r.LeaseID, RegionID: r.RegionID, UserID: r.UserID, APIKeyID: r.APIKeyID, AccountID: r.AccountID, AdmittedAt: r.AdmittedAt, CompletedAt: completed, Charge: charge},
		Model: r.Model, PricingVersion: r.PricingVersion, PayloadSHA256: r.PayloadSHA256,
		OutputTokens: usage.CompletionTokens, Video: &usage, VideoOrigin: r.VideoOrigin,
	}
	if err := task.ledger.Complete(settle, event); err != nil {
		// Another poll may have committed first, or this commit's acknowledgement
		// may have been lost. Replay only the original, fully verified SQL event;
		// never synthesize a second completion time or release an unresolved hold.
		recovered, loadErr := task.ledger.LoadVideoTask(settle, r.VideoOrigin.Owner, task.record.TaskID)
		if loadErr != nil || recovered.State != "completed" || recovered.Event == nil ||
			recovered.Event.Video == nil || *recovered.Event.Video != usage ||
			recovered.Reservation.RequestID != r.RequestID || recovered.Reservation.PricingVersion != r.PricingVersion ||
			recovered.Event.Usage.Charge != charge {
			return errors.Join(ErrRegionalVideoSettlement, err, loadErr)
		}
		if replayErr := task.ledger.Complete(settle, *recovered.Event); replayErr != nil {
			return errors.Join(ErrRegionalVideoSettlement, replayErr)
		}
	}
	result.AsyncVideoTerminal = true
	return nil
}

func regionalVideoTerminalUsage(record ledger.VideoTaskRecord, result *OpenAIForwardResult, endpoint GrokMediaEndpoint, body []byte) (ledger.VideoUsage, bool, error) {
	usage := ledger.VideoUsage{VideoRequest: *record.Reservation.Video, TaskID: record.TaskID}
	if endpoint == SeedanceEndpointDelete && len(strings.TrimSpace(string(body))) == 0 {
		return usage, false, nil
	}
	// This provider's content endpoint returns a validated output URL in a 302
	// instead of a JSON status. The caller validates that URL before this point.
	if endpoint == MiniMaxH3EndpointContent && len(body) == 0 && result.VideoCount == 1 {
		usage.State = "completed"
		return usage, true, nil
	}
	expectedID := record.TaskID
	if usage.Provider != "grok" {
		expectedID = strings.TrimPrefix(expectedID, usage.Provider+":")
	}
	_, state, err := asyncVideoResponseEvidence(body, usage.Provider, expectedID)
	if err != nil {
		return usage, false, err
	}
	switch state {
	case "completed", "done", "succeeded":
		if usage.Provider == "grok" && state != "done" || usage.Provider == "seedance" && state != "succeeded" || usage.Provider == "minimax-h3" && state != "completed" {
			return usage, false, ledger.ErrConflict
		}
		usage.State = "completed"
		if usage.Provider == "grok" && !IsGrokVideoStatusBillable(body) || usage.Provider == "minimax-h3" && !validMiniMaxH3OutputURL(gjson.GetBytes(body, "metadata.url").String()) {
			return usage, false, errors.New("video completion output missing; hold retained")
		}
	case "failed", "cancelled", "expired":
		usage.State = state
	case "canceled":
		usage.State = "cancelled"
	default:
		return usage, false, nil
	}
	seenModel, seenUsage, seenTokens, seenOutput := false, false, false, false
	valid := true
	gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool {
		switch strings.ToLower(key.String()) {
		case "model":
			valid = !seenModel && key.String() == "model" && value.Type == gjson.String && value.String() == usage.UpstreamModel
			seenModel = true
		case "usage":
			if usage.Provider != "seedance" {
				break
			}
			valid = !seenUsage && key.String() == "usage" && value.IsObject()
			seenUsage = true
			if valid {
				value.ForEach(func(name, tokens gjson.Result) bool {
					if !strings.EqualFold(name.String(), "completion_tokens") {
						return true
					}
					var parseErr error
					usage.CompletionTokens, parseErr = strconv.ParseInt(tokens.Raw, 10, 64)
					valid = !seenTokens && name.String() == "completion_tokens" && tokens.Type == gjson.Number && parseErr == nil && usage.CompletionTokens >= 0 && usage.CompletionTokens <= 2147483647
					seenTokens = true
					return valid
				})
			}
		case "video", "metadata":
			outputField := "video"
			if usage.Provider == "minimax-h3" {
				outputField = "metadata"
			}
			if usage.State != "completed" || usage.Provider == "seedance" || !strings.EqualFold(key.String(), outputField) {
				break
			}
			valid = !seenOutput && key.String() == outputField && value.IsObject()
			seenOutput = true
			seenURL := false
			if valid {
				value.ForEach(func(name, url gjson.Result) bool {
					if strings.EqualFold(name.String(), "url") {
						valid = !seenURL && name.String() == "url" && url.Type == gjson.String && strings.TrimSpace(url.String()) != ""
						seenURL = true
					}
					return valid
				})
			}
		}
		return valid
	})
	if !valid || usage.Provider == "seedance" && usage.State == "completed" && !seenTokens {
		return usage, false, errors.New("video terminal billing fields ambiguous; hold retained")
	}
	return usage, true, nil
}
