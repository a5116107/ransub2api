//go:build unit && integration

package regionalvideo_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/regional"
	"github.com/Wei-Shaw/sub2api/internal/regional/ledger"
	"github.com/Wei-Shaw/sub2api/internal/regional/video"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/server/routes"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

type regionalVideoRouterUpstream struct {
	service.HTTPUpstream
	service.AsyncVideoKeyLeaseController
	service.AsyncVideoRegionalForwarder
	dispatch video.Dispatch
}

func (u *regionalVideoRouterUpstream) SetAsyncVideoForwardHandler(dispatch video.Dispatch) {
	u.dispatch = dispatch
	u.AsyncVideoRegionalForwarder.SetAsyncVideoForwardHandler(dispatch)
}

type originalVideoFundsLedger struct {
	ledger.VideoTaskLedger
	fail atomic.Bool
	ack  atomic.Bool
}

func (l *originalVideoFundsLedger) Complete(ctx context.Context, event ledger.Event) error {
	if l.fail.Load() {
		return errors.New("fixture funds temporarily unavailable")
	}
	if err := l.VideoTaskLedger.Complete(ctx, event); err != nil {
		return err
	}
	if l.ack.CompareAndSwap(true, false) {
		return errors.New("fixture committed acknowledgement lost")
	}
	return nil
}

func TestRegionalVideoOriginalFundsRouterLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	require.NoError(t, timezone.Init("UTC"))
	ctx := t.Context()
	pg, err := tcpostgres.Run(ctx, "postgres:18.1-alpine3.23", tcpostgres.WithDatabase("video_funds_test"), tcpostgres.WithUsername("postgres"), tcpostgres.WithPassword("fixture"), tcpostgres.BasicWaitStrategies())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Terminate(context.Background())) })
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable", "TimeZone=UTC")
	require.NoError(t, err)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, repository.ApplyMigrations(ctx, db))
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	rc, err := tcredis.Run(ctx, "redis:7-alpine")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rc.Terminate(context.Background())) })
	address, err := rc.Endpoint(ctx, "")
	require.NoError(t, err)
	rdb := redis.NewClient(&redis.Options{Addr: address})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	require.NoError(t, rdb.Ping(ctx).Err())
	for _, providerName := range []string{"grok", "minimax-h3", "seedance"} {
		for _, mode := range []string{"primary", "regional"} {
			t.Run(providerName+"/"+mode, func(t *testing.T) {
				ctx := t.Context()
				requestID := "router-video-request-" + uuid.NewString()
				var sends atomic.Int32
				var terminal atomic.Bool
				var paused atomic.Bool
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					sends.Add(1)
					if r.Header.Get("Authorization") != "Bearer original-video-key" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					body := `{"status":"pending"}`
					if r.Method == http.MethodPost {
						table := "regional_primary_requests"
						if mode == "regional" {
							table = "regional_local_requests"
						}
						var started bool
						if queryErr := db.QueryRowContext(r.Context(), "SELECT state='started' FROM "+table+" WHERE request_id=$1", requestID).Scan(&started); queryErr != nil || !started {
							t.Errorf("creation reached provider before durable Start: started=%v, error=%v", started, queryErr)
							w.WriteHeader(http.StatusInternalServerError)
							return
						}
						body = `{"id":"task","status":"pending"}`
						if providerName == "grok" {
							body = `{"request_id":"task"}`
						}
					} else if terminal.Load() {
						switch providerName {
						case "grok":
							body = `{"status":"done","video":{"url":"https://vidgen.x.ai/video.mp4"}}`
						case "minimax-h3":
							body = `{"status":"completed","metadata":{"url":"https://cdn.example/video.mp4"}}`
						case "seedance":
							body = `{"status":"succeeded","usage":{"completion_tokens":12345}}`
						}
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, body)
				}))
				t.Cleanup(provider.Close)
				var user, groupID, keyID, accountID int64
				platform := service.PlatformOpenAI
				switch providerName {
				case "grok":
					platform = service.PlatformGrok
				case "minimax-h3":
					platform = service.PlatformMiniMax
				}
				suffix := uuid.NewString()
				require.NoError(t, db.QueryRowContext(ctx, "INSERT INTO users(email,password_hash,status,balance) VALUES($1,'fixture','active',10) RETURNING id", "video-"+suffix+"@test.invalid").Scan(&user))
				require.NoError(t, db.QueryRowContext(ctx, "INSERT INTO groups(name,platform,status,rate_multiplier) VALUES($1,$2,'active',1) RETURNING id", "video-"+suffix, platform).Scan(&groupID))
				require.NoError(t, db.QueryRowContext(ctx, "INSERT INTO api_keys(user_id,key,name,group_id,status) VALUES($1,$2,'fixture',$3,'active') RETURNING id", user, "sk-fixture-"+suffix, groupID).Scan(&keyID))
				require.NoError(t, db.QueryRowContext(ctx, "INSERT INTO accounts(name,platform,type,status,schedulable,concurrency) VALUES($1,$2,'apikey','active',true,5) RETURNING id", "video-"+suffix, platform).Scan(&accountID))
				_, err := db.ExecContext(ctx, "INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)", accountID, groupID)
				require.NoError(t, err)
				group := &service.Group{ID: groupID, Platform: platform, RateMultiplier: 1, AllowImageGeneration: true}
				key := &service.APIKey{ID: keyID, UserID: user, GroupID: &groupID, Key: "sk-fixture-" + suffix}
				accountRepo := repository.NewAccountRepository(client, db, nil)
				credentials := map[string]any{"api_key": "original-video-key", "base_url": provider.URL, "openai_capabilities": []string{"seedance"}}
				account, err := accountRepo.GetByID(ctx, accountID)
				require.NoError(t, err)
				account.Credentials = credentials
				require.NoError(t, accountRepo.Update(ctx, account))
				account, err = accountRepo.GetByID(ctx, accountID)
				require.NoError(t, err)
				price := ledger.Price{Model: "router-video-" + uuid.NewString(), Video: &ledger.VideoPrice{VideoRequest: ledger.VideoRequest{Provider: providerName, UpstreamModel: "MiniMax-H3", Resolution: "768p", DurationSeconds: 5}, Basis: "request", UnitNano: 20_000_000, SaleNumerator: 1, SaleDenominator: 10}}
				taskID, path, operation := "minimax-h3:task", "/v1/videos/task", service.MiniMaxH3EndpointStatus
				switch providerName {
				case "grok":
					price.Video.VideoRequest = ledger.VideoRequest{Provider: providerName, UpstreamModel: "grok-imagine-video", Resolution: "720p", DurationSeconds: 6}
					price.Video.Basis = "second"
					taskID, operation = "task", service.GrokMediaEndpointVideoStatus
				case "minimax-h3":
					price.Model = "MiniMax-H3"
				case "seedance":
					price.Video.VideoRequest = ledger.VideoRequest{Provider: providerName, UpstreamModel: "ep-seedance"}
					price.Video.Basis, price.Video.UnitNano, price.Video.MaxCompletionTokens = "completion_tokens", 28_000_000_000, 1_000_000
					taskID, path, operation = "seedance:task", "/v1/contents/generations/tasks/task", service.SeedanceEndpointStatus
				}
				require.NoError(t, ledger.InstallPrice(ctx, db, price))
				maximum, err := price.VideoMaximum(price.Video.VideoRequest)
				require.NoError(t, err)
				primary, holds, verify := repository.NewRegionalFundsAuthorities(db, func(context.Context, *sql.Tx) error { return nil })
				var now time.Time
				require.NoError(t, db.QueryRowContext(ctx, "SELECT clock_timestamp()").Scan(&now))
				now = now.UTC().Truncate(time.Microsecond)
				grant := regional.Lease{SchemaVersion: regional.SchemaVersion, LeaseID: "router-video-grant-" + uuid.NewString(), RegionID: "us", UserID: user, Granted: 8_000_000_000, IssuedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour)}
				grant.APIKeyID, grant.AccountID = key.ID, account.ID
				r := ledger.Reservation{RequestID: requestID, LeaseID: grant.LeaseID, RegionID: grant.RegionID, UserID: user, Maximum: maximum, PayloadSHA256: strings.Repeat("a", 64), AdmittedAt: now}
				r.APIKeyID, r.AccountID, r.Model, r.PricingVersion = key.ID, account.ID, price.Model, price.Version()
				r.Video = &price.Video.VideoRequest
				r.VideoOrigin = &ledger.VideoOrigin{Member: "video:" + uuid.NewString(), Owner: video.Owner{UserID: user, APIKeyID: key.ID, GroupID: group.ID, AccountID: account.ID, Provider: providerName, RouteFingerprint: service.AccountRouteFingerprint(account)}}
				var requests ledger.VideoTaskLedger = holds
				var local *ledger.Local
				if mode == "regional" {
					require.NoError(t, primary.Issue(ctx, grant))
					local = &ledger.Local{DB: db, Region: grant.RegionID}
					require.NoError(t, local.Install(ctx, grant))
					requests = local
				} else {
					r.LeaseID, r.RegionID = "", ""
				}
				wrapped := &originalVideoFundsLedger{VideoTaskLedger: requests}
				cfg := &config.Config{RunMode: config.RunModeStandard, Runtime: config.RuntimeConfig{RegionalFundsFile: "fixture"}, Gateway: config.GatewayConfig{MaxBodySize: 1 << 20, TextMaxBodySize: 1 << 20}, Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowPrivateHosts: true, AllowInsecureHTTP: true}}}
				cache := repository.NewGatewayCache(rdb)
				concurrency := service.NewConcurrencyService(repository.NewConcurrencyCache(rdb, 15, 30))
				key.User, key.Group = &service.User{ID: user}, group
				// Exercise the real model mapping before the immutable contract check.
				credentials["model_mapping"] = map[string]any{price.Model: price.Video.UpstreamModel}
				account.Credentials = credentials
				require.NoError(t, accountRepo.Update(ctx, account))
				account, err = accountRepo.GetByID(ctx, accountID)
				require.NoError(t, err)
				r.VideoOrigin.Owner.RouteFingerprint = service.AccountRouteFingerprint(account)
				newRouter := func() (*gin.Engine, *regionalVideoRouterUpstream) {
					transport := repository.ProvideHTTPUpstream(cfg, nil, nil, nil)
					controller, ok := transport.(service.AsyncVideoKeyLeaseController)
					require.True(t, ok)
					forwarder, ok := transport.(service.AsyncVideoRegionalForwarder)
					require.True(t, ok)
					upstream := &regionalVideoRouterUpstream{HTTPUpstream: transport, AsyncVideoKeyLeaseController: controller, AsyncVideoRegionalForwarder: forwarder}
					meter := &service.RegionalMetering{Ledger: wrapped, Readiness: func(context.Context) error {
						if paused.Load() {
							return errors.New("new admission paused")
						}
						return nil
					}, Admit: func(_ context.Context, admittedKey *service.APIKey, admittedAccount *service.Account, _ []byte) (ledger.Reservation, ledger.Price, error) {
						require.False(t, paused.Load(), "queries must not admit another financial request")
						require.Equal(t, key.ID, admittedKey.ID)
						require.Equal(t, account.ID, admittedAccount.ID)
						return r, price, nil
					}}
					gateway := service.ProvideOpenAIGatewayService(service.OpenAIGatewayDependencies{Config: cfg, Accounts: accountRepo, Cache: cache, Concurrency: concurrency, RegionalMetering: meter, Upstream: upstream, GrokTokens: service.NewGrokTokenProvider(accountRepo, nil)})
					t.Cleanup(gateway.StopAsyncVideoKeyReleaseRecovery)
					t.Cleanup(gateway.StopOpenAICodexTicketHarvester)
					billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
					t.Cleanup(billing.Stop)
					h := &handler.Handlers{Gateway: &handler.GatewayHandler{}, OpenAIGateway: handler.NewOpenAIGatewayHandler(gateway, concurrency, billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg), AsyncImage: handler.NewAsyncImageHandler(nil, nil)}
					router := gin.New()
					// Authentication is injected; the full production route, funds gate,
					// account repository, transport and financial ledger execute below.
					routes.RegisterGatewayRoutes(router, h, func(c *gin.Context) {
						if c.GetHeader("Authorization") != "Bearer "+key.Key {
							c.AbortWithStatus(http.StatusUnauthorized)
							return
						}
						c.Set(string(middleware.ContextKeyAPIKey), key)
						c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: user, Concurrency: 5})
						c.Next()
					}, nil, nil, nil, nil, nil, cfg)
					gateway.InstallMediaVideoForwarding(router)
					return router, upstream
				}
				router, upstream := newRouter()
				createPath := "/v1/videos"
				createBody := `{"model":"` + price.Model + `","prompt":"fixture","seconds":"5"}`
				switch providerName {
				case "grok":
					createBody = `{"model":"` + price.Model + `","prompt":"fixture","duration":6,"resolution":"720p"}`
				case "seedance":
					createPath = "/v3/contents/generations/tasks"
					createBody = `{"model":"` + price.Model + `","content":[{"type":"text","text":"fixture"}]}`
				}
				create := func(body, credential, subagent string) *httptest.ResponseRecorder {
					request := httptest.NewRequestWithContext(ctx, http.MethodPost, createPath, strings.NewReader(body))
					request.Header.Set("Authorization", "Bearer "+credential)
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("X-OpenAI-Subagent", subagent)
					response := httptest.NewRecorder()
					router.ServeHTTP(response, request)
					return response
				}
				for _, scenario := range []struct {
					name, field string
					status      int
				}{
					{"wrong_key", "", http.StatusUnauthorized},
					{"paused", "", http.StatusServiceUnavailable},
					{"model_policy", "", http.StatusNotFound},
					{"subagent_policy", "", http.StatusForbidden},
					{"unsupported_multiplier", "", http.StatusServiceUnavailable},
					{"unsupported_video_multiplier", "", http.StatusServiceUnavailable},
					{"priority", `,"service_tier":"priority"`, http.StatusServiceUnavailable},
					{"fast", `,"fast":true`, http.StatusServiceUnavailable},
					{"batch", `,"n":2`, http.StatusServiceUnavailable},
					{"ambiguous_model", `,"model":"` + price.Model + `"`, http.StatusServiceUnavailable},
				} {
					t.Run(scenario.name, func(t *testing.T) {
						sends.Store(0)
						group.RateMultiplier = 1
						group.VideoRateIndependent = false
						group.VideoRateMultiplier = 1
						if scenario.name == "unsupported_multiplier" {
							group.RateMultiplier = 0.8
						}
						if scenario.name == "unsupported_video_multiplier" {
							group.VideoRateIndependent = true
							group.VideoRateMultiplier = 0.8
						}
						_, updateErr := db.ExecContext(ctx, "UPDATE groups SET rate_multiplier=$2, video_rate_independent=$3, video_rate_multiplier=$4 WHERE id=$1", group.ID, group.RateMultiplier, group.VideoRateIndependent, group.VideoRateMultiplier)
						require.NoError(t, updateErr)
						paused.Store(scenario.name == "paused")
						group.BlockSubagents = scenario.name == "subagent_policy"
						group.ModelAllowlist = service.GroupModelAllowlist{Enabled: scenario.name == "model_policy", Models: []string{"other"}}
						credential, subagent := key.Key, ""
						if scenario.name == "wrong_key" {
							credential = "wrong-key"
						}
						if scenario.name == "subagent_policy" {
							subagent = "worker"
						}
						body := strings.TrimSuffix(createBody, "}") + scenario.field + "}"
						response := create(body, credential, subagent)
						wantStatus := scenario.status
						if providerName == "minimax-h3" && (scenario.name == "batch" || scenario.name == "ambiguous_model") {
							wantStatus = http.StatusBadRequest // H3 rejects these malformed fields before financial admission.
						}
						require.Equal(t, wantStatus, response.Code, response.Body.String())
						require.Zero(t, sends.Load(), "rejected creation must not contact the provider")
						var reserved int
						require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM regional_video_financial_bindings WHERE request_id=$1", requestID).Scan(&reserved))
						require.Zero(t, reserved, "rejection before Reserve must leave no orphaned financial identity")
					})
				}
				paused.Store(false)
				group.BlockSubagents = false
				group.ModelAllowlist = service.GroupModelAllowlist{}
				group.RateMultiplier = 1
				group.VideoRateIndependent = false
				group.VideoRateMultiplier = 1
				_, err = db.ExecContext(ctx, "UPDATE groups SET rate_multiplier=1, video_rate_independent=false, video_rate_multiplier=1 WHERE id=$1", group.ID)
				require.NoError(t, err)
				created := create(createBody, key.Key, "")
				require.Equal(t, http.StatusOK, created.Code, created.Body.String())
				createdRecord, err := requests.LoadVideoTask(ctx, r.VideoOrigin.Owner, taskID)
				require.NoError(t, err)
				require.Equal(t, requestID, createdRecord.Reservation.RequestID)
				require.Equal(t, price, createdRecord.Price)
				r = createdRecord.Reservation
				pending, err := cache.GetGrokVideoPendingBilling(ctx, fmt.Sprintf("%d:%d:%s", user, key.ID, taskID))
				require.NoError(t, err)
				require.Empty(t, pending, "SQL-metered creation must not write a second legacy billing snapshot")
				// Current group prices never replace the tariff of an accepted task.
				_, err = db.ExecContext(ctx, "UPDATE groups SET rate_multiplier=0.8 WHERE id=$1", group.ID)
				require.NoError(t, err)
				group.RateMultiplier = 0.8
				paused.Store(true)
				query := func(method, url, credential string) *httptest.ResponseRecorder {
					req := httptest.NewRequestWithContext(ctx, method, url, nil)
					req.Header.Set("Authorization", "Bearer "+credential)
					w := httptest.NewRecorder()
					router.ServeHTTP(w, req)
					return w
				}
				w := query(http.MethodGet, path, key.Key)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				record, err := requests.LoadVideoTask(ctx, r.VideoOrigin.Owner, taskID)
				require.NoError(t, err)
				require.Equal(t, "started", record.State)
				terminal.Store(true)
				wrapped.fail.Store(true)
				w = query(http.MethodGet, path, key.Key)
				require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), "regional_video_settlement_unavailable")
				record, err = requests.LoadVideoTask(ctx, r.VideoOrigin.Owner, taskID)
				require.NoError(t, err)
				require.Equal(t, "started", record.State)
				wrapped.fail.Store(false)
				wrapped.ack.Store(true)
				w = httptest.NewRecorder()
				upstream.dispatch(w, httptest.NewRequestWithContext(ctx, http.MethodPost, "/private", nil), video.Query{PolicyVersion: video.ForwardPolicyVersion, Owner: r.VideoOrigin.Owner, TaskID: taskID, Operation: string(operation), Credential: key.Key, ClientIP: "192.0.2.5"})
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				record, err = requests.LoadVideoTask(ctx, r.VideoOrigin.Owner, taskID)
				require.NoError(t, err)
				require.Equal(t, "completed", record.State)
				usage := ledger.VideoUsage{VideoRequest: price.Video.VideoRequest, TaskID: taskID, State: "completed"}
				if providerName == "seedance" {
					usage.CompletionTokens = 12345
				}
				_, wantCharge, err := price.VideoCosts(usage)
				require.NoError(t, err)
				require.Equal(t, wantCharge, record.Event.Usage.Charge)
				if local != nil {
					_, err := primary.Settle(ctx, grant.RegionID, *record.Event, verify)
					require.NoError(t, err)
					require.NoError(t, local.Acknowledge(ctx, *record.Event))
					wrapped.VideoTaskLedger = &ledger.Local{DB: db, Region: grant.RegionID}
				} else {
					_, restarted, _ := repository.NewRegionalFundsAuthorities(db, func(context.Context, *sql.Tx) error { return nil })
					wrapped.VideoTaskLedger = restarted
				}
				router, _ = newRouter()
				w = query(http.MethodGet, path, key.Key)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				before := sends.Load()
				w = query(http.MethodGet, path, "wrong-key")
				require.Equal(t, http.StatusUnauthorized, w.Code)
				credentials["api_key"] = "replacement-video-key"
				account.Credentials = credentials
				require.NoError(t, accountRepo.Update(ctx, account))
				w = query(http.MethodGet, path, key.Key)
				require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
				require.Equal(t, before, sends.Load(), "changed original route must be rejected before HTTP")
				credentials["api_key"] = "original-video-key"
				account.Credentials = credentials
				require.NoError(t, accountRepo.Update(ctx, account))
				w = query(http.MethodGet, path, key.Key)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				w = query(http.MethodPost, "/v1/videos", key.Key)
				require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
				require.EqualValues(t, 6, sends.Load())
				var count int
				var exact bool
				amount, err := ledger.USD(wantCharge)
				require.NoError(t, err)
				require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*),COALESCE(sum(actual_cost),0)=$2::numeric FROM usage_logs WHERE request_id=$1", r.RequestID, amount).Scan(&count, &exact))
				require.Equal(t, 1, count)
				require.True(t, exact)
				var duplicate int
				require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM usage_logs WHERE request_id=$1", service.StableGrokVideoBillingRequestID(taskID)).Scan(&duplicate))
				require.Zero(t, duplicate)
			})
		}
	}
}
