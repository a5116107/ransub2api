//go:build integration

package service_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/cyberevidence"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type moderationAdmissionSettings struct{ service.SettingRepository }

func (moderationAdmissionSettings) GetValue(context.Context, string) (string, error) {
	return "", service.ErrSettingNotFound
}

func TestContentModerationSharesRealRedisKeyCapacityAndRecovers(t *testing.T) {
	rdb := startKeyAdmissionIntegrationRedis(t)
	for _, engine := range []string{"openai", "typesafe"} {
		for _, astra := range []bool{false, true} {
			name := engine + "-direct"
			if astra {
				name = engine + "-astra"
			}
			t.Run(name, func(t *testing.T) {
				var audits atomic.Int32
				var slow atomic.Bool
				started, finish := make(chan struct{}, 1), make(chan struct{})
				var finishOnce sync.Once
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.URL.Path == "/held" {
						_, _ = io.WriteString(w, "held")
						_ = http.NewResponseController(w).Flush()
						<-req.Context().Done()
						return
					}
					audits.Add(1)
					var body []byte
					if engine == "typesafe" {
						answers := map[string]any{}
						for _, category := range service.ContentModerationCategories("typesafe") {
							answers[category] = map[string]any{"type": "noul", "noul": 0.1}
						}
						body, _ = json.Marshal(map[string]any{"model": "fixture", "answers": answers})
					} else {
						body = []byte(`{"model":"fixture","results":[{"flagged":false,"categories":{},"category_scores":{}}]}`)
					}
					if slow.Load() {
						_, _ = w.Write(body[:1])
						_ = http.NewResponseController(w).Flush()
						started <- struct{}{}
						select {
						case <-finish:
						case <-req.Context().Done():
							return
						}
						body = body[1:]
					}
					_, _ = w.Write(body)
				}))
				t.Cleanup(server.Close)
				t.Cleanup(func() { finishOnce.Do(func() { close(finish) }) })
				cfg := &config.Config{}
				cfg.Gateway.UpstreamKeyConcurrency = config.UpstreamKeyConcurrencyConfig{Revision: 1, Limits: map[string]int{config.UpstreamKeyFingerprint("shared-fixture"): 1}}
				cfg.Gateway.CodexGatewayPin.Enabled = astra
				transport := repository.ProvideHTTPUpstream(cfg, nil, rdb, nil)
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				t.Cleanup(func() { _ = db.Close() })
				mock.ExpectQuery("SELECT value FROM settings").WithArgs(cyberevidence.ConfigKey).WillReturnError(sql.ErrNoRows)
				svc := service.ProvideContentModerationService(moderationAdmissionSettings{}, nil, nil, nil, nil, nil, nil, nil, db, nil, cfg, transport)
				t.Cleanup(svc.CyberEvidence().Close)
				require.NoError(t, mock.ExpectationsWereMet())
				busyReq, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/held", strings.NewReader("{}"))
				require.NoError(t, err)
				busyReq.Header.Set("Authorization", "Bearer shared-fixture")
				busy, err := transport.Do(busyReq, "", 1, 1)
				require.NoError(t, err)
				t.Cleanup(func() { _ = busy.Body.Close() })
				input := service.TestContentModerationAPIKeysInput{Engine: engine, BaseURL: server.URL, APIKeys: []string{"shared-fixture"}, Prompt: "fixture"}
				_, err = svc.TestAPIKeys(t.Context(), input)
				require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
				require.Zero(t, audits.Load(), "a locally full supplier key must not send the audit")
				require.NoError(t, busy.Body.Close())
				result, err := svc.TestAPIKeys(t.Context(), input)
				require.NoError(t, err)
				require.NotNil(t, result.AuditResult)
				require.EqualValues(t, 1, audits.Load())

				slow.Store(true)
				done := make(chan error, 1)
				go func() { _, auditErr := svc.TestAPIKeys(t.Context(), input); done <- auditErr }()
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("slow audit did not start")
				}
				other, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/other", strings.NewReader("{}"))
				require.NoError(t, err)
				other.Header.Set("Authorization", "Bearer shared-fixture")
				response, err := transport.Do(other, "", 2, 1)
				if response != nil {
					_ = response.Body.Close()
				}
				require.ErrorIs(t, err, service.ErrUpstreamKeyCapacity)
				require.EqualValues(t, 2, audits.Load(), "the competing business request must not reach the server")
				finishOnce.Do(func() { close(finish) })
				select {
				case err = <-done:
					require.NoError(t, err)
				case <-time.After(5 * time.Second):
					t.Fatal("slow audit did not finish")
				}
				other, err = http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/other", strings.NewReader("{}"))
				require.NoError(t, err)
				other.Header.Set("Authorization", "Bearer shared-fixture")
				response, err = transport.Do(other, "", 2, 1)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.EqualValues(t, 3, audits.Load())
			})
		}
	}
}
