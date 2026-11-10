package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/regional"
	"github.com/Wei-Shaw/sub2api/internal/regional/ledger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type regionalVideoTaskKey struct{}

var ErrRegionalVideoSettlement = errors.New("original video funds settlement unavailable")

type regionalVideoTask struct {
	ledger ledger.VideoTaskLedger
	record ledger.VideoTaskRecord
}

// Creation uses the existing request ledger and the same member as the shared
// key handoff. Reserve and Start precede the POST; only its acknowledged task
// identity may bind the hold. An uncertain send never admits another attempt.
func (s *OpenAIGatewayService) beginRegionalVideoCreate(req *http.Request, c *gin.Context, account *Account, endpoint GrokMediaEndpoint, billingBody, outboundBody []byte) (*http.Request, func(*OpenAIForwardResult, error) error, error) {
	noop := func(_ *OpenAIForwardResult, err error) error { return err }
	if !endpoint.IsAsyncVideoCreate() {
		return req, noop, nil
	}
	financial := s.regionalMetering != nil || s.cfg != nil && s.cfg.Runtime.RegionalFundsFile != ""
	if !financial {
		return req, noop, nil
	}
	reject := func(err error) (*http.Request, func(*OpenAIForwardResult, error) error, error) {
		return nil, nil, errors.Join(ErrRegionalVideoSettlement, err)
	}
	if c == nil || c.Request == nil {
		return reject(ledger.ErrConflict)
	}
	if err := c.Request.Context().Err(); err != nil {
		return reject(err)
	}
	m := s.regionalMetering
	if err := m.Check(req.Context()); err != nil {
		return reject(err)
	}
	tasks, ok := m.Ledger.(ledger.VideoTaskLedger)
	if !ok {
		return reject(ledger.ErrConflict)
	}
	key := getAPIKeyFromContext(c)
	handoff := AsyncVideoKeyHandoffFromRequest(req)
	owner, valid := AsyncVideoKeyOwnerFromContext(req.Context())
	if key == nil || account == nil || !valid || key.GroupID == nil ||
		owner.UserID != key.UserID || owner.APIKeyID != key.ID || owner.GroupID != *key.GroupID ||
		owner.AccountID != account.ID || owner.Provider != endpoint.AsyncVideoProvider() || owner.RouteFingerprint != AccountRouteFingerprint(account) ||
		handoff == nil || handoff.Owner() != owner {
		return reject(ledger.ErrConflict)
	}
	// Authenticate both spellings before the mapper or a first-key decoder can
	// hide duplicate billing fields. Multipart video needs its own cost proof.
	if err := validateRegionalVideoFields(billingBody); err != nil {
		return reject(err)
	}
	if err := validateRegionalVideoFields(outboundBody); err != nil {
		return reject(err)
	}
	r, price, err := m.Admit(req.Context(), key, account, billingBody)
	if err != nil {
		return reject(err)
	}
	if r.UserID != key.UserID || r.APIKeyID != key.ID || r.AccountID != account.ID || price.Model != gjson.GetBytes(billingBody, "model").String() || price.Video == nil || price.Validate() != nil {
		return reject(ledger.ErrConflict)
	}
	dimensions, err := regionalVideoCreateDimensions(endpoint, outboundBody)
	if err != nil || price.Video.VideoRequest != dimensions {
		return reject(errors.Join(ledger.ErrConflict, err))
	}
	maximum, err := price.VideoMaximum(dimensions)
	if err != nil {
		return reject(err)
	}
	digest := sha256.Sum256(outboundBody)
	r.Model, r.PricingVersion, r.Maximum, r.PayloadSHA256 = price.Model, price.Version(), maximum, hex.EncodeToString(digest[:])
	r.Video, r.VideoOrigin = &dimensions, &ledger.VideoOrigin{Member: handoff.Member(), Owner: owner}
	fresh, err := tasks.Reserve(req.Context(), r)
	if err != nil || !fresh {
		return reject(errors.Join(ledger.ErrAlreadyDispatched, err))
	}
	attempt := &regionalMeterAttempt{ledger: tasks, reservation: r, price: price}
	req = req.WithContext(context.WithValue(req.Context(), regionalMeterKey{}, attempt))
	finish := func(result *OpenAIForwardResult, forwardErr error) error {
		if result != nil {
			result.RegionalMetered = true
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), 5*time.Second)
		defer cancel()
		if !attempt.sent.Load() || errors.Is(forwardErr, ledger.ErrStartRejected) {
			return errors.Join(forwardErr, tasks.CancelBeforeStart(cleanup, r.RequestID))
		}
		if errors.Is(forwardErr, ErrRegionalVideoSettlement) {
			return forwardErr
		}
		if attempt.videoRejected.Load() || IsUpstreamKeyAdmissionFailure(forwardErr) && !IsAsyncVideoSubmissionUnconfirmed(forwardErr) {
			if err := tasks.RejectVideoTask(cleanup, r.RequestID); err != nil {
				return errors.Join(ErrRegionalVideoSettlement, forwardErr, err)
			}
			return forwardErr
		}
		if forwardErr != nil || !attempt.videoBound.Load() {
			return asyncVideoSubmissionError(endpoint, errors.Join(errors.New("video creation outcome unresolved; hold retained"), forwardErr))
		}
		return nil
	}
	return req, finish, nil
}

func validateRegionalVideoFields(body []byte) error {
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return errors.New("video funds require a JSON object")
	}
	seen := make(map[string]bool)
	valid := true
	gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool {
		name := strings.ToLower(key.String())
		switch name {
		case "model", "duration", "seconds", "resolution", "size", "n", "service_tier", "fast", "stream", "metadata", "content":
			valid = key.String() == name && !seen[name]
			seen[name] = true
		}
		if !valid {
			return false
		}
		switch name {
		case "model":
			valid = value.Type == gjson.String && value.String() != "" && strings.TrimSpace(value.String()) == value.String()
		case "n":
			valid = value.Type == gjson.Number && value.Raw == "1"
		case "service_tier":
			valid = value.Type == gjson.String && value.String() == "default"
		case "fast", "stream":
			valid = value.Type == gjson.False
		}
		return valid
	})
	if !valid || !seen["model"] {
		return errors.New("video billing fields ambiguous or outside immutable cost bound")
	}
	return nil
}

func regionalVideoCreateDimensions(endpoint GrokMediaEndpoint, body []byte) (ledger.VideoRequest, error) {
	dimensions := ledger.VideoRequest{Provider: endpoint.AsyncVideoProvider(), UpstreamModel: gjson.GetBytes(body, "model").String()}
	switch dimensions.Provider {
	case "minimax-h3":
		info, err := ParseMiniMaxH3Request(body)
		if err != nil {
			return dimensions, err
		}
		dimensions.Resolution, dimensions.DurationSeconds = info.Resolution, int64(info.DurationSeconds)
	case "grok":
		info := ParseGrokMediaRequest("application/json", body)
		dimensions.Resolution, dimensions.DurationSeconds = info.Resolution, int64(info.DurationSeconds)
		if duration := gjson.GetBytes(body, "duration"); duration.Exists() {
			n, err := strconv.ParseInt(duration.Raw, 10, 32)
			if duration.Type != gjson.Number || err != nil || n != dimensions.DurationSeconds {
				return dimensions, ledger.ErrConflict
			}
		}
		if resolution := gjson.GetBytes(body, "resolution"); resolution.Exists() && (resolution.Type != gjson.String || resolution.String() != dimensions.Resolution) {
			return dimensions, ledger.ErrConflict
		}
		if gjson.GetBytes(body, "size").Exists() || gjson.GetBytes(body, "seconds").Exists() {
			return dimensions, ledger.ErrConflict
		}
	case "seedance":
		if _, err := ParseSeedanceRequest(body); err != nil {
			return dimensions, err
		}
		// Completion-token bounds cover content options. Explicit dimensions
		// must still match the installed contract rather than being discarded.
		if resolution := gjson.GetBytes(body, "resolution"); resolution.Exists() {
			if resolution.Type != gjson.String {
				return dimensions, ledger.ErrConflict
			}
			dimensions.Resolution = resolution.String()
		}
		if duration := gjson.GetBytes(body, "duration"); duration.Exists() {
			var err error
			dimensions.DurationSeconds, err = strconv.ParseInt(duration.Raw, 10, 32)
			if duration.Type != gjson.Number || err != nil {
				return dimensions, ledger.ErrConflict
			}
		}
		if gjson.GetBytes(body, "size").Exists() || gjson.GetBytes(body, "seconds").Exists() {
			return dimensions, ledger.ErrConflict
		}
	default:
		return dimensions, ledger.ErrConflict
	}
	return dimensions, nil
}

func observeRegionalVideoRejection(req *http.Request, response *http.Response) {
	a := regionalMeterAttemptFrom(req.Context())
	if a == nil || a.reservation.Video == nil || response == nil {
		return
	}
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden,
		http.StatusMethodNotAllowed, http.StatusUnprocessableEntity, http.StatusTooManyRequests:
		a.videoRejected.Store(true)
	}
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
