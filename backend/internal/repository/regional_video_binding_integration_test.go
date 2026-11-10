//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/regional"
	"github.com/Wei-Shaw/sub2api/internal/regional/ledger"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type regionalVideoCreationLedger struct {
	ledger.VideoTaskLedger
	bindAckLost  bool
	startAckLost bool
	beforeStart  func(context.Context) error
}

func (l *regionalVideoCreationLedger) Start(ctx context.Context, id string) error {
	if l.beforeStart != nil {
		if err := l.beforeStart(ctx); err != nil {
			return err
		}
	}
	if err := l.VideoTaskLedger.Start(ctx, id); err != nil {
		return err
	}
	if l.startAckLost {
		l.startAckLost = false
		return io.ErrUnexpectedEOF
	}
	return nil
}

func (l *regionalVideoCreationLedger) BindVideoTask(ctx context.Context, id, taskID string) error {
	if err := l.VideoTaskLedger.BindVideoTask(ctx, id, taskID); err != nil {
		return err
	}
	if l.bindAckLost {
		l.bindAckLost = false
		return io.ErrUnexpectedEOF
	}
	return nil
}

func TestRegionalVideoHTTPCreateUsesOriginalFundsAndSharedMember(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	t.Setenv("SUB2API_REGIONAL_TEST_DSN", integrationPostgresDSN)
	gin.SetMode(gin.TestMode)
	for _, providerName := range []string{"minimax-h3", "grok", "seedance"} {
		for _, mode := range []string{"primary", "regional"} {
			t.Run(providerName+"/"+mode, func(t *testing.T) {
				ctx := t.Context()
				user, group, key, account := regionalVideoFinancialSubject(t, providerName)
				var sends atomic.Int32
				var responseStatus atomic.Int32
				var ambiguous atomic.Bool
				var taskNumber atomic.Int32
				var activeRequest atomic.Value
				requestTable := "regional_primary_requests"
				if mode == "regional" {
					requestTable = "regional_local_requests"
				}
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					sends.Add(1)
					w.Header().Set("Content-Type", "application/json")
					if r.Header.Get("Authorization") != "Bearer supplier-fixture" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					if r.Method == http.MethodGet {
						switch providerName {
						case "grok":
							_, _ = io.WriteString(w, `{"status":"done","video":{"url":"https://vidgen.x.ai/video.mp4"}}`)
						case "seedance":
							_, _ = io.WriteString(w, `{"status":"succeeded","usage":{"completion_tokens":12345}}`)
						default:
							_, _ = io.WriteString(w, `{"status":"completed","metadata":{"url":"https://cdn.example/video.mp4"}}`)
						}
						return
					}
					if r.Method != http.MethodPost {
						w.WriteHeader(http.StatusMethodNotAllowed)
						return
					}
					body, err := io.ReadAll(r.Body)
					if err != nil {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					digest := sha256.Sum256(body)
					var funded bool
					if err := integrationDB.QueryRowContext(r.Context(), "SELECT state='started' AND request->>'payload_sha256'=$2 FROM "+requestTable+" WHERE request_id=$1", activeRequest.Load(), hex.EncodeToString(digest[:])).Scan(&funded); err != nil || !funded {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					if status := responseStatus.Load(); status != 0 {
						w.WriteHeader(int(status))
						_, _ = io.WriteString(w, `{"error":"fixture rejection"}`)
						return
					}
					if ambiguous.Load() {
						_, _ = io.WriteString(w, `{"id":"task","id":"other"}`)
						return
					}
					_, _ = fmt.Fprintf(w, `{"id":"task-%d","request_id":"task-%d","status":"queued"}`, taskNumber.Load(), taskNumber.Load())
				}))
				t.Cleanup(provider.Close)
				price := ledger.Price{Model: "create-video-" + uuid.NewString(), Video: &ledger.VideoPrice{VideoRequest: ledger.VideoRequest{Provider: providerName, UpstreamModel: "MiniMax-H3", Resolution: "768p", DurationSeconds: 5}, Basis: "request", UnitNano: 20_000_000, SaleNumerator: 1, SaleDenominator: 1}}
				endpoint, taskID := service.MiniMaxH3EndpointCreate, "minimax-h3:task"
				bodyOptions := `,"prompt":"waves","seconds":"5"}`
				switch providerName {
				case "grok":
					price.Video.VideoRequest = ledger.VideoRequest{Provider: providerName, UpstreamModel: "grok-imagine-video", Resolution: "720p", DurationSeconds: 6}
					price.Video.Basis, endpoint, taskID = "second", service.GrokMediaEndpointVideosGenerations, "task"
					bodyOptions = `,"prompt":"waves","resolution":"720p","duration":6}`
				case "seedance":
					price.Video.VideoRequest = ledger.VideoRequest{Provider: providerName, UpstreamModel: "ep-seedance"}
					price.Video.Basis, price.Video.UnitNano, price.Video.MaxCompletionTokens = "completion_tokens", 28_000_000_000, 50_000
					endpoint, taskID = service.SeedanceEndpointCreate, "seedance:task"
					bodyOptions = `,"content":[{"type":"text","text":"waves"}]}`
				}
				account.Credentials = map[string]any{"api_key": "supplier-fixture", "base_url": provider.URL, "model_mapping": map[string]any{price.Model: price.Video.UpstreamModel}, "openai_capabilities": []string{"seedance"}}
				owner := video.Owner{UserID: user, APIKeyID: key.ID, GroupID: group.ID, AccountID: account.ID, Provider: providerName, RouteFingerprint: service.AccountRouteFingerprint(account)}
				require.NoError(t, ledger.InstallPrice(ctx, integrationDB, price))
				maximum, err := price.VideoMaximum(price.Video.VideoRequest)
				require.NoError(t, err)
				primary, holds, verify := NewRegionalFundsAuthorities(integrationDB, func(context.Context, *sql.Tx) error { return nil })
				grant := regionalLedgerLease(t, "create-funds-"+uuid.NewString(), user, 8_000_000_000)
				grant.APIKeyID, grant.AccountID = key.ID, account.ID
				grant.VideoPriceVersions = []string{price.Version()}
				grant.GroupID = group.ID
				var requests ledger.VideoTaskLedger = holds
				table := "regional_primary_requests"
				if mode == "regional" {
					require.NoError(t, primary.Issue(ctx, grant))
					local := &ledger.Local{DB: integrationDB, Region: grant.RegionID, ValidateAdmission: VerifyRegionalSalePolicy}
					require.NoError(t, local.Install(ctx, grant))
					requests, table = local, "regional_local_requests"
				}
				wrapped := &regionalVideoCreationLedger{VideoTaskLedger: requests}
				cfg := upstreamKeyTestConfig(1, 1)
				cfg.Security.URLAllowlist.AllowInsecureHTTP = true
				rdb := regionalKeyRedis(t)
				capacity := regionalAsyncVideoAuthority(t, cfg, rdb, mode)
				upstream := liveKeyTestUpstream(t, cfg, rdb, capacity)
				for index, scenario := range []string{"accepted", "bind_ack_lost", "policy_changed_before_start", "start_ack_lost", "rejected", "unknown_server", "ambiguous_task"} {
					t.Run(scenario, func(t *testing.T) {
						taskNumber.Store(int32(index))
						currentTaskID := fmt.Sprintf("%s-%d", taskID, index)
						requestID := "created-request-" + uuid.NewString()
						activeRequest.Store(requestID)
						admissions := 0
						meter := &service.RegionalMetering{Ledger: wrapped, Readiness: func(ctx context.Context) error { return ctx.Err() }, Admit: func(ctx context.Context, _ *service.APIKey, _ *service.Account, _ []byte) (ledger.Reservation, ledger.Price, error) {
							admissions++
							var now time.Time
							if err := integrationDB.QueryRowContext(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
								return ledger.Reservation{}, price, err
							}
							r := ledger.Reservation{RequestID: requestID, UserID: user, APIKeyID: key.ID, AccountID: account.ID, AdmittedAt: now.UTC().Truncate(time.Microsecond)}
							if mode == "regional" {
								r.RegionID, r.LeaseID = grant.RegionID, grant.LeaseID
							}
							return r, price, nil
						}}
						gateway := service.ProvideOpenAIGatewayService(service.OpenAIGatewayDependencies{Config: cfg, Upstream: upstream, RegionalMetering: meter})
						t.Cleanup(gateway.StopAsyncVideoKeyReleaseRecovery)
						t.Cleanup(gateway.StopOpenAICodexTicketHarvester)
						responseStatus.Store(0)
						ambiguous.Store(scenario == "ambiguous_task")
						wrapped.bindAckLost = scenario == "bind_ack_lost"
						wrapped.startAckLost = scenario == "start_ack_lost"
						wrapped.beforeStart = nil
						if scenario == "policy_changed_before_start" {
							wrapped.beforeStart = func(ctx context.Context) error {
								_, err := integrationDB.ExecContext(ctx, "UPDATE groups SET rate_multiplier=0.8 WHERE id=$1", group.ID)
								return err
							}
							t.Cleanup(func() {
								_, err := integrationDB.ExecContext(ctx, "UPDATE groups SET rate_multiplier=1 WHERE id=$1", group.ID)
								require.NoError(t, err)
							})
						}
						switch scenario {
						case "rejected":
							responseStatus.Store(http.StatusTooManyRequests)
						case "unknown_server":
							responseStatus.Store(http.StatusInternalServerError)
						}
						body := []byte(`{"model":"` + price.Model + `"` + bodyOptions)
						forwardCtx := service.WithAsyncVideoKeyOwner(ctx, owner)
						c, _ := gin.CreateTestContext(httptest.NewRecorder())
						c.Request = httptest.NewRequestWithContext(forwardCtx, http.MethodPost, "/fixture", strings.NewReader(string(body)))
						c.Set("api_key", key)
						var balanceBefore string
						require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT balance::text FROM users WHERE id=$1", user).Scan(&balanceBefore))
						before := sends.Load()
						var result *service.OpenAIForwardResult
						var forwardErr error
						switch providerName {
						case "grok":
							result, forwardErr = gateway.ForwardGrokMedia(forwardCtx, c, account, endpoint, "", body, "application/json")
						case "seedance":
							result, forwardErr = gateway.ForwardSeedance(forwardCtx, c, account, endpoint, "", body)
						default:
							result, forwardErr = gateway.ForwardMiniMaxH3(forwardCtx, c, account, endpoint, "", body)
						}
						if scenario == "policy_changed_before_start" || scenario == "start_ack_lost" {
							require.Equal(t, before, sends.Load(), "a failed Start must never reach the provider")
						} else {
							require.Equal(t, before+1, sends.Load(), "one creation per financial identity, including lost ACK")
						}
						var state string
						var raw []byte
						require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT state,request FROM "+table+" WHERE request_id=$1", requestID).Scan(&state, &raw))
						var reservation ledger.Reservation
						require.NoError(t, json.Unmarshal(raw, &reservation))
						require.Equal(t, maximum, reservation.Maximum)
						require.Equal(t, owner, reservation.VideoOrigin.Owner)
						require.NotEmpty(t, reservation.VideoOrigin.Member)
						switch scenario {
						case "policy_changed_before_start":
							require.ErrorIs(t, forwardErr, ledger.ErrStartRejected)
							require.ErrorIs(t, forwardErr, service.ErrRegionalVideoSettlement)
							require.False(t, service.IsAsyncVideoSubmissionUnconfirmed(forwardErr))
							require.Equal(t, "canceled", state)
							var balanceAfter string
							require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT balance::text FROM users WHERE id=$1", user).Scan(&balanceAfter))
							require.Equal(t, balanceBefore, balanceAfter, "proven unsent admission must refund the original reservation")
						case "start_ack_lost":
							require.ErrorIs(t, forwardErr, service.ErrRegionalVideoSettlement)
							require.NotErrorIs(t, forwardErr, ledger.ErrStartRejected)
							require.Equal(t, "started", state, "a committed Start with a lost acknowledgement must keep the hold")
							require.ErrorIs(t, requests.CancelBeforeStart(ctx, requestID), ledger.ErrAlreadyDispatched)
						case "accepted", "bind_ack_lost":
							require.NoError(t, forwardErr)
							require.True(t, result.RegionalMetered)
							require.Equal(t, "started", state)
							record, err := requests.LoadVideoTask(ctx, owner, currentTaskID)
							require.NoError(t, err)
							require.Equal(t, reservation, record.Reservation)
							lease, err := upstream.LoadAsyncVideoKeyLeaseByTask(ctx, owner, currentTaskID)
							require.NoError(t, err)
							require.NotNil(t, lease)
							require.Equal(t, reservation.VideoOrigin.Member, lease.Member)
							// New-admission failure must not obstruct the original hold's
							// actual terminal query, settlement, or concurrency release.
							meter.Readiness = func(context.Context) error { return errors.New("new admission paused") }
							query, _ := gin.CreateTestContext(httptest.NewRecorder())
							query.Request = httptest.NewRequestWithContext(forwardCtx, http.MethodGet, "/fixture", nil)
							query.Set("api_key", key)
							switch providerName {
							case "grok":
								result, err = gateway.ForwardGrokMedia(forwardCtx, query, account, service.GrokMediaEndpointVideoStatus, currentTaskID, nil, "")
							case "seedance":
								result, err = gateway.ForwardSeedance(forwardCtx, query, account, service.SeedanceEndpointStatus, currentTaskID, nil)
							default:
								result, err = gateway.ForwardMiniMaxH3(forwardCtx, query, account, service.MiniMaxH3EndpointStatus, currentTaskID, nil)
							}
							require.NoError(t, err)
							require.True(t, result.RegionalMetered)
							require.True(t, result.AsyncVideoTerminal)
							require.Equal(t, 1, admissions, "query must not create another financial identity")
							record, err = requests.LoadVideoTask(ctx, owner, currentTaskID)
							require.NoError(t, err)
							require.Equal(t, "completed", record.State)
							require.NotNil(t, record.Event)
							if local, ok := requests.(*ledger.Local); ok {
								applied, err := primary.Settle(ctx, grant.RegionID, *record.Event, verify)
								require.NoError(t, err)
								require.True(t, applied)
								require.NoError(t, local.Acknowledge(ctx, *record.Event))
							}
							var oneUsage bool
							require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT count(*)=1 FROM usage_logs WHERE request_id=$1", requestID).Scan(&oneUsage))
							require.True(t, oneUsage)
							require.NoError(t, upstream.ReleaseAsyncVideoKeyLeaseByTask(ctx, owner, currentTaskID))
						case "rejected":
							require.Error(t, forwardErr)
							require.Equal(t, "canceled", state)
						default:
							require.True(t, service.IsAsyncVideoSubmissionUnconfirmed(forwardErr), "%v", forwardErr)
							require.Equal(t, "started", state)
							var storedMember string
							require.NoError(t, capacity.database().QueryRowContext(ctx, "SELECT member FROM regional_async_video_key_leases WHERE member=$1", reservation.VideoOrigin.Member).Scan(&storedMember))
							require.Equal(t, reservation.VideoOrigin.Member, storedMember)
							// Only this fixture's explicit no-task confirmation releases
							// unknown concurrency; production still requires reconciliation.
							require.NoError(t, upstream.ReleaseAsyncVideoKeyLeaseByMember(ctx, storedMember))
						}
					})
				}
			})
		}
	}
}

func TestRegionalVideoExplicitRejectionRefundsOnlyUnboundTask(t *testing.T) {
	for _, mode := range []string{"primary", "regional"} {
		for _, scenario := range []string{"unbound", "bound", "racing_bind"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				ctx := t.Context()
				user, group, key, account := regionalVideoFinancialSubject(t, "minimax-h3")
				price := ledger.Price{Model: "rejected-video-" + uuid.NewString(), Video: &ledger.VideoPrice{VideoRequest: ledger.VideoRequest{Provider: "minimax-h3", UpstreamModel: "MiniMax-H3", Resolution: "768p", DurationSeconds: 5}, Basis: "request", UnitNano: 20_000_000, SaleNumerator: 1, SaleDenominator: 1}}
				require.NoError(t, ledger.InstallPrice(ctx, integrationDB, price))
				maximum, err := price.VideoMaximum(price.Video.VideoRequest)
				require.NoError(t, err)
				primary, holds, _ := NewRegionalFundsAuthorities(integrationDB, func(context.Context, *sql.Tx) error { return nil })
				grant := regionalLedgerLease(t, "rejection-"+uuid.NewString(), user, 1_000_000_000)
				grant.APIKeyID, grant.AccountID = key.ID, account.ID
				grant.VideoPriceVersions = []string{price.Version()}
				grant.GroupID = group.ID
				r := regionalLedgerReservation(t, grant, "rejection-request-"+uuid.NewString(), maximum)
				r.APIKeyID, r.AccountID, r.Model, r.PricingVersion = key.ID, account.ID, price.Model, price.Version()
				r.PayloadSHA256, r.Video = strings.Repeat("a", 64), &price.Video.VideoRequest
				r.VideoOrigin = &ledger.VideoOrigin{Member: "video:" + uuid.NewString(), Owner: video.Owner{UserID: user, APIKeyID: key.ID, GroupID: group.ID, AccountID: account.ID, Provider: "minimax-h3", RouteFingerprint: service.AccountRouteFingerprint(account)}}
				var requests ledger.VideoTaskLedger = holds
				table := "regional_primary_requests"
				if mode == "regional" {
					require.NoError(t, primary.Issue(ctx, grant))
					local := &ledger.Local{DB: integrationDB, Region: grant.RegionID}
					require.NoError(t, local.Install(ctx, grant))
					requests, table = local, "regional_local_requests"
				} else {
					r.LeaseID, r.RegionID = "", ""
				}
				fresh, err := requests.Reserve(ctx, r)
				require.NoError(t, err)
				require.True(t, fresh)
				require.NoError(t, requests.Start(ctx, r.RequestID))
				require.ErrorIs(t, requests.CancelBeforeStart(ctx, r.RequestID), ledger.ErrAlreadyDispatched)
				bound := false
				switch scenario {
				case "bound":
					require.NoError(t, requests.BindVideoTask(ctx, r.RequestID, "minimax-h3:task"))
					bound = true
					require.ErrorIs(t, requests.RejectVideoTask(ctx, r.RequestID), ledger.ErrConflict)
				case "racing_bind":
					start := make(chan struct{})
					var bindErr, rejectErr error
					var workers sync.WaitGroup
					workers.Add(2)
					go func() {
						defer workers.Done()
						<-start
						bindErr = requests.BindVideoTask(ctx, r.RequestID, "minimax-h3:task")
					}()
					go func() { defer workers.Done(); <-start; rejectErr = requests.RejectVideoTask(ctx, r.RequestID) }()
					close(start)
					workers.Wait()
					bound = bindErr == nil
					if bound {
						require.ErrorIs(t, rejectErr, ledger.ErrConflict)
					} else {
						require.ErrorIs(t, bindErr, ledger.ErrAlreadyDispatched)
						require.NoError(t, rejectErr)
					}
				default:
					require.NoError(t, requests.RejectVideoTask(ctx, r.RequestID))
					require.NoError(t, requests.RejectVideoTask(ctx, r.RequestID), "refund retries must be idempotent")
					require.ErrorIs(t, requests.BindVideoTask(ctx, r.RequestID, "minimax-h3:task"), ledger.ErrAlreadyDispatched)
				}
				var state string
				require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT state FROM "+table+" WHERE request_id=$1", r.RequestID).Scan(&state))
				want := "canceled"
				if bound {
					want = "started"
				}
				require.Equal(t, want, state)
				var correct bool
				amount, err := ledger.USD(maximum)
				require.NoError(t, err)
				if mode == "regional" {
					wantReserved := regional.Amount(0)
					if bound {
						wantReserved = maximum
					}
					require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT reserved=$2 AND spent=0 FROM regional_local_leases WHERE lease_id=$1", grant.LeaseID, wantReserved).Scan(&correct))
				} else {
					if !bound {
						amount = "0"
					}
					require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT balance=10-$2::numeric FROM users WHERE id=$1", user, amount).Scan(&correct))
				}
				require.True(t, correct, "only an unbound rejected task may return its exact original hold")
				var identities, usage int
				require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT count(*) FROM regional_video_financial_bindings WHERE request_id=$1", r.RequestID).Scan(&identities))
				require.Equal(t, 1, identities, "refund must retain the permanent member identity")
				require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT count(*) FROM usage_logs WHERE request_id=$1", r.RequestID).Scan(&usage))
				require.Zero(t, usage, "a rejected creation is not fabricated terminal usage")
			})
		}
	}
}

type regionalVideoTerminalLedger struct {
	ledger.VideoTaskLedger
	fail atomic.Bool
	ack  atomic.Bool
}

func (l *regionalVideoTerminalLedger) Complete(ctx context.Context, event ledger.Event) error {
	if l.fail.Load() {
		return errors.New("fixture original funds temporarily unavailable")
	}
	if err := l.VideoTaskLedger.Complete(ctx, event); err != nil {
		return err
	}
	if l.ack.CompareAndSwap(true, false) {
		return errors.New("fixture committed completion acknowledgement lost")
	}
	return nil
}

func TestRegionalVideoHTTPCompletionUsesOriginalLedger(t *testing.T) {
	for _, providerName := range []string{"minimax-h3", "grok", "seedance"} {
		for _, mode := range []string{"primary", "regional"} {
			t.Run(providerName+"/"+mode, func(t *testing.T) {
				ctx := t.Context()
				var sends atomic.Int32
				var terminal atomic.Bool
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					sends.Add(1)
					if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer original-video-key" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if !terminal.Load() {
						_, _ = io.WriteString(w, `{"status":"pending"}`)
						return
					}
					switch providerName {
					case "grok":
						_, _ = io.WriteString(w, `{"request_id":"task","status":"done","model":"grok-imagine-video","video":{"url":"https://cdn.example/video.mp4","duration":99}}`)
					case "seedance":
						_, _ = io.WriteString(w, `{"id":"task","status":"succeeded","model":"ep-seedance","usage":{"completion_tokens":12345}}`)
					default:
						_, _ = io.WriteString(w, `{"id":"task","status":"completed","metadata":{"url":"https://cdn.example/video.mp4"}}`)
					}
				}))
				t.Cleanup(provider.Close)
				user, group, key, account := regionalVideoFinancialSubject(t, providerName)
				account.Credentials = map[string]any{"api_key": "original-video-key", "base_url": provider.URL, "openai_capabilities": []string{"seedance"}}
				price := ledger.Price{Model: "http-video-" + uuid.NewString(), Video: &ledger.VideoPrice{
					VideoRequest: ledger.VideoRequest{Provider: providerName, UpstreamModel: "MiniMax-H3", Resolution: "768p", DurationSeconds: 5},
					Basis:        "request", UnitNano: 20_000_000, SaleNumerator: 1, SaleDenominator: 10,
				}}
				wantCharge := regional.Amount(2_000_000)
				endpoint, taskID := service.MiniMaxH3EndpointStatus, "minimax-h3:task"
				switch providerName {
				case "grok":
					price.Video.VideoRequest = ledger.VideoRequest{Provider: providerName, UpstreamModel: "grok-imagine-video", Resolution: "720p", DurationSeconds: 6}
					price.Video.Basis, wantCharge = "second", 12_000_000
					endpoint, taskID = service.GrokMediaEndpointVideoStatus, "task"
				case "seedance":
					price.Video.VideoRequest = ledger.VideoRequest{Provider: providerName, UpstreamModel: "ep-seedance"}
					price.Video.Basis, price.Video.UnitNano, price.Video.MaxCompletionTokens = "completion_tokens", 28_000_000_000, 1_000_000
					wantCharge, endpoint, taskID = 34_566_000, service.SeedanceEndpointStatus, "seedance:task"
				}
				require.NoError(t, ledger.InstallPrice(ctx, integrationDB, price))
				maximum, err := price.VideoMaximum(price.Video.VideoRequest)
				require.NoError(t, err)
				primary, holds, verify := NewRegionalFundsAuthorities(integrationDB, func(context.Context, *sql.Tx) error { return nil })
				grant := regionalLedgerLease(t, "http-video-grant-"+uuid.NewString(), user, 8_000_000_000)
				grant.APIKeyID, grant.AccountID = key.ID, account.ID
				grant.VideoPriceVersions = []string{price.Version()}
				grant.GroupID = group.ID
				grant.AccountGranted = 40_000_000_000
				r := regionalLedgerReservation(t, grant, "http-video-request-"+uuid.NewString(), maximum)
				r.APIKeyID, r.AccountID, r.Model, r.PricingVersion = key.ID, account.ID, price.Model, price.Version()
				r.Video = &price.Video.VideoRequest
				r.VideoOrigin = &ledger.VideoOrigin{Member: "video:" + uuid.NewString(), Owner: video.Owner{UserID: user, APIKeyID: key.ID, GroupID: group.ID, AccountID: account.ID, Provider: providerName, RouteFingerprint: service.AccountRouteFingerprint(account)}}
				var requests ledger.VideoTaskLedger = holds
				var local *ledger.Local
				if mode == "regional" {
					require.NoError(t, primary.Issue(ctx, grant))
					local = &ledger.Local{DB: integrationDB, Region: grant.RegionID}
					require.NoError(t, local.Install(ctx, grant))
					requests = local
				} else {
					r.LeaseID, r.RegionID = "", ""
				}
				fresh, err := requests.Reserve(ctx, r)
				require.NoError(t, err)
				require.True(t, fresh)
				require.NoError(t, requests.Start(ctx, r.RequestID))
				require.NoError(t, requests.BindVideoTask(ctx, r.RequestID, taskID))
				_, err = integrationDB.ExecContext(ctx, "UPDATE groups SET rate_multiplier=0.8 WHERE id=$1", group.ID)
				require.NoError(t, err)
				wrapped := &regionalVideoTerminalLedger{VideoTaskLedger: requests}
				cfg := &config.Config{}
				cfg.Security.URLAllowlist.AllowInsecureHTTP = true
				newGateway := func() *service.OpenAIGatewayService {
					gateway := service.ProvideOpenAIGatewayService(service.OpenAIGatewayDependencies{Config: cfg, RegionalMetering: &service.RegionalMetering{Ledger: wrapped}, Upstream: newHTTPUpstream(cfg, nil, nil, nil)})
					t.Cleanup(gateway.StopAsyncVideoKeyReleaseRecovery)
					t.Cleanup(gateway.StopOpenAICodexTicketHarvester)
					return gateway
				}
				gateway := newGateway()
				query := func(gateway *service.OpenAIGatewayService) (*service.OpenAIForwardResult, bool, error) {
					queryCtx := service.WithAsyncVideoKeyOwner(ctx, r.VideoOrigin.Owner)
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequestWithContext(queryCtx, http.MethodGet, "/video/task", nil)
					var result *service.OpenAIForwardResult
					var queryErr error
					switch providerName {
					case "grok":
						result, queryErr = gateway.ForwardGrokMedia(queryCtx, c, account, endpoint, taskID, nil, "")
					case "seedance":
						result, queryErr = gateway.ForwardSeedance(queryCtx, c, account, endpoint, taskID, nil)
					default:
						result, queryErr = gateway.ForwardMiniMaxH3(queryCtx, c, account, endpoint, taskID, nil)
					}
					return result, c.Writer.Written(), queryErr
				}
				result, _, err := query(gateway)
				require.NoError(t, err)
				require.True(t, result.RegionalMetered)
				require.False(t, result.AsyncVideoTerminal)
				terminal.Store(true)
				wrapped.fail.Store(true)
				result, written, err := query(gateway)
				require.Error(t, err)
				require.Nil(t, result)
				require.False(t, written)
				record, err := requests.LoadVideoTask(ctx, r.VideoOrigin.Owner, taskID)
				require.NoError(t, err)
				require.Equal(t, price, record.Price)
				require.Equal(t, "started", record.State)
				wrapped.fail.Store(false)
				wrapped.ack.Store(true)
				var wg sync.WaitGroup
				errorsCh := make(chan error, 4)
				for range 4 {
					wg.Go(func() {
						result, written, queryErr := query(gateway)
						if queryErr == nil && (result == nil || !result.RegionalMetered || !result.AsyncVideoTerminal || !written) {
							queryErr = errors.New("terminal financial query did not produce a metered response")
						}
						errorsCh <- queryErr
					})
				}
				wg.Wait()
				close(errorsCh)
				for queryErr := range errorsCh {
					require.NoError(t, queryErr)
				}
				record, err = requests.LoadVideoTask(ctx, r.VideoOrigin.Owner, taskID)
				require.NoError(t, err)
				require.Equal(t, "completed", record.State)
				require.Equal(t, wantCharge, record.Event.Usage.Charge)
				require.Equal(t, r.VideoOrigin, record.Event.VideoOrigin)
				if local != nil {
					applied, err := primary.Settle(ctx, grant.RegionID, *record.Event, verify)
					require.NoError(t, err)
					require.True(t, applied)
					require.NoError(t, local.Acknowledge(ctx, *record.Event))
					wrapped.VideoTaskLedger = &ledger.Local{DB: integrationDB, Region: grant.RegionID}
				} else {
					_, restarted, _ := NewRegionalFundsAuthorities(integrationDB, func(context.Context, *sql.Tx) error { return nil })
					wrapped.VideoTaskLedger = restarted
				}
				gateway = newGateway()
				result, _, err = query(gateway)
				require.NoError(t, err)
				require.True(t, result.RegionalMetered)
				var count int
				var exactCharge bool
				wantUSD, err := ledger.USD(wantCharge)
				require.NoError(t, err)
				require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT count(*),COALESCE(sum(actual_cost),0)=$2::numeric FROM usage_logs WHERE request_id=$1", r.RequestID, wantUSD).Scan(&count, &exactCharge))
				require.Equal(t, 1, count)
				require.True(t, exactCharge, "canonical usage must match the original exact video charge")
				require.EqualValues(t, 7, sends.Load(), "lookups must not resubmit generation, including financial retries")
			})
		}
	}
}

func TestRegionalVideoPermanentRouteRejectsChangedAccountBeforeHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"primary", "regional"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			var sends atomic.Int32
			var redirectStatus atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				sends.Add(1)
				if request.Header.Get("Authorization") != "Bearer original-video-key" || request.URL.Path != "/v1/videos/original-task" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if status := redirectStatus.Load(); status != 0 {
					w.Header().Set("Location", "/redirected-task")
					w.WriteHeader(int(status))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"original-task","status":"pending"}`)
			}))
			t.Cleanup(provider.Close)
			user, group, key, account := regionalVideoFinancialSubject(t, "minimax-h3")
			account.Credentials = map[string]any{"api_key": "original-video-key", "base_url": provider.URL}
			price := ledger.Price{Model: "route-" + uuid.NewString(), Video: &ledger.VideoPrice{
				VideoRequest: ledger.VideoRequest{Provider: "minimax-h3", UpstreamModel: "MiniMax-H3", Resolution: "768p", DurationSeconds: 5},
				Basis:        "request", UnitNano: 20_000_000, SaleNumerator: 1, SaleDenominator: 10,
			}}
			require.NoError(t, ledger.InstallPrice(ctx, integrationDB, price))
			primary, holds, _ := NewRegionalFundsAuthorities(integrationDB, func(context.Context, *sql.Tx) error { return nil })
			grant := regionalLedgerLease(t, "route-grant-"+uuid.NewString(), user, 8_000_000_000)
			grant.APIKeyID, grant.AccountID = key.ID, account.ID
			grant.VideoPriceVersions = []string{price.Version()}
			grant.GroupID = group.ID
			r := regionalLedgerReservation(t, grant, "route-request-"+uuid.NewString(), 2_000_000)
			r.APIKeyID, r.AccountID, r.Model, r.PricingVersion = key.ID, account.ID, price.Model, price.Version()
			r.Video = &price.Video.VideoRequest
			r.VideoOrigin = &ledger.VideoOrigin{Member: "video:" + uuid.NewString(), Owner: video.Owner{
				UserID: user, APIKeyID: key.ID, GroupID: group.ID, AccountID: account.ID, Provider: "minimax-h3", RouteFingerprint: service.AccountRouteFingerprint(account),
			}}
			var requests ledger.VideoTaskLedger = holds
			if mode == "regional" {
				require.NoError(t, primary.Issue(ctx, grant))
				local := &ledger.Local{DB: integrationDB, Region: grant.RegionID}
				require.NoError(t, local.Install(ctx, grant))
				requests = local
			} else {
				r.LeaseID, r.RegionID = "", ""
			}
			fresh, err := requests.Reserve(ctx, r)
			require.NoError(t, err)
			require.True(t, fresh)
			require.NoError(t, requests.Start(ctx, r.RequestID))
			const task = "minimax-h3:original-task"
			require.NoError(t, requests.BindVideoTask(ctx, r.RequestID, task))
			cfg := &config.Config{}
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			meter := &service.RegionalMetering{Ledger: requests, Readiness: func(context.Context) error { return errors.New("new admissions paused") }}
			gateway := service.ProvideOpenAIGatewayService(service.OpenAIGatewayDependencies{Config: cfg, RegionalMetering: meter, Upstream: newHTTPUpstream(cfg, nil, nil, nil)})
			t.Cleanup(gateway.StopOpenAICodexTicketHarvester)
			t.Cleanup(gateway.StopAsyncVideoKeyReleaseRecovery)
			query := func(wantSuccess bool) {
				t.Helper()
				resolved, err := gateway.ResolveMediaVideoRequestAccount(ctx, &group.ID, task, user, key.ID, service.MiniMaxH3EndpointStatus)
				require.NoError(t, err)
				require.Equal(t, account.ID, resolved)
				current := r.VideoOrigin.Owner
				current.RouteFingerprint = service.AccountRouteFingerprint(account)
				queryCtx := service.WithAsyncVideoKeyOwner(ctx, current)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/original-task", nil).WithContext(queryCtx)
				before := sends.Load()
				result, err := gateway.ForwardMiniMaxH3(queryCtx, c, account, service.MiniMaxH3EndpointStatus, task, nil)
				if wantSuccess {
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, before+1, sends.Load())
				} else {
					require.ErrorIs(t, err, service.ErrUpstreamKeyPolicy)
					require.Nil(t, result)
					require.Equal(t, before, sends.Load(), "changed route must be rejected before reaching the HTTP provider")
				}
			}
			query(true)
			account.Credentials["api_key"] = "replacement-video-key"
			query(false)
			account.Credentials["api_key"] = "original-video-key"
			account.Credentials["base_url"] = provider.URL + "/replacement"
			query(false)
			account.Credentials["base_url"] = provider.URL
			query(true)
			record, err := requests.LoadVideoTask(ctx, r.VideoOrigin.Owner, task)
			require.NoError(t, err)
			require.Equal(t, r, record.Reservation, "queries must not allocate, reprice or replace the original financial hold")
			require.Equal(t, "started", record.State)
			require.EqualValues(t, 2, sends.Load())
			for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
				redirectStatus.Store(int32(status))
				queryCtx := service.WithAsyncVideoKeyOwner(ctx, r.VideoOrigin.Owner)
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/original-task", nil).WithContext(queryCtx)
				before := sends.Load()
				result, err := gateway.ForwardMiniMaxH3(queryCtx, c, account, service.MiniMaxH3EndpointStatus, task, nil)
				require.Error(t, err, "status %d must not change the physical task target", status)
				require.Nil(t, result)
				require.False(t, c.Writer.Written(), "unexpected redirect must return to the existing error handler without forwarding a response")
				require.Equal(t, before+1, sends.Load(), "status %d must not send a second request to the redirect target", status)
			}
			redirectStatus.Store(0)
			query(true)
		})
	}
}

func TestRegionalVideoFinancialBindingCompetitionAndRecovery(t *testing.T) {
	for _, mode := range []string{"primary", "regional"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			user, group, key, account := regionalVideoFinancialSubject(t, "minimax-h3")
			price := ledger.Price{Model: "binding-" + uuid.NewString(), Video: &ledger.VideoPrice{
				VideoRequest: ledger.VideoRequest{Provider: "minimax-h3", UpstreamModel: "MiniMax-H3", Resolution: "768p", DurationSeconds: 5},
				Basis:        "request", UnitNano: 20_000_000, SaleNumerator: 1, SaleDenominator: 10,
			}}
			require.NoError(t, ledger.InstallPrice(ctx, integrationDB, price))
			primary, holds, verify := NewRegionalFundsAuthorities(integrationDB, func(context.Context, *sql.Tx) error { return nil })
			lease := regionalLedgerLease(t, "binding-grant-"+uuid.NewString(), user, 8_000_000_000)
			lease.APIKeyID, lease.AccountID = key.ID, account.ID
			lease.VideoPriceVersions = []string{price.Version()}
			lease.GroupID = group.ID
			r := regionalLedgerReservation(t, lease, "binding-request-"+uuid.NewString(), 2_000_000)
			r.APIKeyID, r.AccountID, r.Model, r.PricingVersion = key.ID, account.ID, price.Model, price.Version()
			r.Video = &price.Video.VideoRequest
			r.VideoOrigin = &ledger.VideoOrigin{Member: "video:" + uuid.NewString(), Owner: video.Owner{UserID: user, APIKeyID: key.ID, GroupID: group.ID, AccountID: account.ID, Provider: "minimax-h3", RouteFingerprint: service.AccountRouteFingerprint(account)}}
			var requests ledger.VideoTaskLedger = holds
			if mode == "regional" {
				require.NoError(t, primary.Issue(ctx, lease))
				local := &ledger.Local{DB: integrationDB, Region: lease.RegionID}
				require.NoError(t, local.Install(ctx, lease))
				requests = local
			} else {
				r.LeaseID, r.RegionID = "", ""
				bad := r
				badOrigin := *r.VideoOrigin
				badOrigin.Owner.GroupID++
				bad.VideoOrigin = &badOrigin
				_, err := requests.Reserve(ctx, bad)
				require.ErrorIs(t, err, ledger.ErrConflict, "origin group must match the allocated financial scope")
			}
			other := r
			other.RequestID = "binding-other-" + uuid.NewString()
			otherOrigin := *r.VideoOrigin
			otherOrigin.Member = "video:" + uuid.NewString()
			other.VideoOrigin = &otherOrigin
			for _, hold := range []ledger.Reservation{r, other} {
				fresh, err := requests.Reserve(ctx, hold)
				require.NoError(t, err)
				require.True(t, fresh)
				require.ErrorIs(t, requests.BindVideoTask(ctx, hold.RequestID, "unsent"), ledger.ErrAlreadyDispatched)
				require.NoError(t, requests.Start(ctx, hold.RequestID))
			}
			reusedMember := r
			reusedMember.RequestID = "binding-reused-" + uuid.NewString()
			_, err := requests.Reserve(ctx, reusedMember)
			require.ErrorIs(t, err, ledger.ErrConflict)
			type bindingResult struct {
				hold ledger.Reservation
				err  error
			}
			results := make(chan bindingResult, 2)
			var workers sync.WaitGroup
			// The same raw ID also occurs under the next mode's different user:
			// tenant separation must not turn unrelated tasks into one charge.
			const taskID = "same-provider-task"
			for _, hold := range []ledger.Reservation{r, other} {
				workers.Go(func() { results <- bindingResult{hold, requests.BindVideoTask(ctx, hold.RequestID, taskID)} })
			}
			workers.Wait()
			close(results)
			var winner, loser ledger.Reservation
			won := 0
			for result := range results {
				if result.err == nil {
					winner = result.hold
					won++
				} else {
					require.ErrorIs(t, result.err, ledger.ErrConflict)
					loser = result.hold
				}
			}
			require.Equal(t, 1, won)
			makeEvent := func(hold ledger.Reservation, id, state string) ledger.Event {
				charge := hold.Maximum
				if state != "completed" {
					charge = 0
				}
				e := regionalLedgerEvent(hold, charge)
				e.InputTokens, e.OutputTokens = 0, 0
				e.Video = &ledger.VideoUsage{VideoRequest: *hold.Video, TaskID: id, State: state}
				e.VideoOrigin = hold.VideoOrigin
				return e
			}
			require.ErrorIs(t, requests.Complete(ctx, makeEvent(loser, taskID, "completed")), ledger.ErrConflict)
			require.ErrorIs(t, requests.CancelBeforeStart(ctx, loser.RequestID), ledger.ErrAlreadyDispatched, "a conflicting task response must not refund possibly dispatched work")
			if mode == "regional" {
				requests = &ledger.Local{DB: integrationDB, Region: lease.RegionID}
			} else {
				_, restarted, _ := NewRegionalFundsAuthorities(integrationDB, func(context.Context, *sql.Tx) error { return nil })
				requests = restarted
			}
			record, err := requests.LoadVideoTask(ctx, winner.VideoOrigin.Owner, taskID)
			require.NoError(t, err)
			require.Equal(t, winner, record.Reservation)
			require.Equal(t, "started", record.State)
			gateway := &service.OpenAIGatewayService{}
			service.WithRegionalMetering(&service.RegionalMetering{Ledger: requests, Readiness: func(context.Context) error { return errors.New("new admission is unavailable") }})(gateway)
			resolvedAccount, err := gateway.ResolveMediaVideoRequestAccount(ctx, &group.ID, taskID, user, key.ID, service.MiniMaxH3EndpointStatus)
			require.NoError(t, err, "the real service must use original SQL ownership without sticky/slot records or new admission")
			require.Equal(t, account.ID, resolvedAccount)
			for _, statement := range []string{
				`UPDATE regional_video_financial_bindings SET member='video:00000000-0000-0000-0000-000000000000' WHERE request_id=$1`,
				`UPDATE regional_video_financial_bindings SET task_id='replacement' WHERE request_id=$1`,
				`DELETE FROM regional_video_financial_bindings WHERE request_id=$1`,
			} {
				_, err := integrationDB.ExecContext(ctx, statement, winner.RequestID)
				require.Error(t, err, "financial task identities must remain immutable after slot release or application rollback")
			}
			// Recovery can bind a different, verified task to the still-unknown
			// loser; an explicit failed terminal event then releases only its hold.
			require.NoError(t, requests.BindVideoTask(ctx, loser.RequestID, "verified-other-task"))
			events := []ledger.Event{makeEvent(winner, taskID, "completed"), makeEvent(loser, "verified-other-task", "failed")}
			for _, e := range events {
				require.NoError(t, requests.Complete(ctx, e))
				if mode == "regional" {
					_, err := primary.Settle(ctx, lease.RegionID, e, verify)
					require.NoError(t, err)
					localLedger, ok := requests.(*ledger.Local)
					require.True(t, ok)
					require.NoError(t, localLedger.Acknowledge(ctx, e))
				}
			}
			record, err = requests.LoadVideoTask(ctx, winner.VideoOrigin.Owner, taskID)
			require.NoError(t, err)
			require.Equal(t, &events[0], record.Event)
			require.NoError(t, requests.Complete(ctx, *record.Event))
			resolvedAccount, err = gateway.ResolveMediaVideoRequestAccount(ctx, &group.ID, taskID, user, key.ID, service.MiniMaxH3EndpointContent)
			require.NoError(t, err)
			require.Equal(t, account.ID, resolvedAccount)
			if mode == "regional" {
				principal := winner.VideoOrigin.Owner
				principal.AccountID, principal.RouteFingerprint = 0, [32]byte{}
				_, err := holds.ResolveVideoTaskOwner(ctx, principal, taskID)
				require.ErrorIs(t, err, sql.ErrNoRows, "primary reconciliation must not transfer execution ownership from the original region")
			}
			// Primary requests and every regional grant share one permanent
			// primary namespace, even when event/member/lease/region all change.
			cross := regionalLedgerLease(t, "binding-cross-"+uuid.NewString(), user, 10_000_000)
			cross.RegionID, cross.APIKeyID, cross.AccountID = "eu", key.ID, account.ID
			cross.VideoPriceVersions = []string{price.Version()}
			cross.GroupID = group.ID
			cross.AccountGranted = 20_000_000
			require.NoError(t, primary.Issue(ctx, cross))
			duplicate := events[0]
			duplicate.Usage.EventID = "binding-replay-" + uuid.NewString()
			duplicate.Usage.LeaseID, duplicate.Usage.RegionID = cross.LeaseID, cross.RegionID
			duplicate.Usage.AdmittedAt = regionalLedgerClock(t)
			duplicate.Usage.CompletedAt = duplicate.Usage.AdmittedAt
			duplicateOrigin := *duplicate.VideoOrigin
			duplicateOrigin.Member = "video:" + uuid.NewString()
			duplicate.VideoOrigin = &duplicateOrigin
			applied, err := primary.Settle(ctx, cross.RegionID, duplicate, verify)
			require.ErrorIs(t, err, ledger.ErrConflict)
			require.False(t, applied)
			var orphaned, usage int
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM regional_video_financial_bindings WHERE request_id=$1`, duplicate.Usage.EventID).Scan(&orphaned))
			require.Zero(t, orphaned, "rejected primary claims roll back with money and canonical usage")
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs WHERE user_id=$1`, user).Scan(&usage))
			require.Equal(t, 2, usage, "one completed and one explicitly failed task, with no duplicate financial event")
			// A failed independent scope verification must also roll back the new
			// binding, so a corrected event can be retried after recovery.
			uniqueTask := *duplicate.Video
			uniqueTask.TaskID = "independent-scope-task"
			duplicate.Video = &uniqueTask
			duplicateOrigin.Owner.GroupID++
			_, err = primary.Settle(ctx, cross.RegionID, duplicate, verify)
			require.ErrorIs(t, err, ledger.ErrConflict, "primary independently checks original grant group attribution")
			duplicateOrigin.Owner.GroupID--
			_, err = primary.Settle(ctx, cross.RegionID, duplicate, func(context.Context, *sql.Tx, ledger.Event) error { return errors.New("fixture authority unavailable") })
			require.ErrorContains(t, err, "authority unavailable")
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM regional_video_financial_bindings WHERE request_id=$1`, duplicate.Usage.EventID).Scan(&orphaned))
			require.Zero(t, orphaned)
			_, err = primary.Settle(ctx, cross.RegionID, duplicate, verify)
			require.NoError(t, err)
		})
	}
}
