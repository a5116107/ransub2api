package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIAPIKeyHealthBreakerSettingsSaveAndFailure(t *testing.T) {
	repo := &openAIAPIKeyHealthSettingRepo{}
	svc := NewSettingService(repo, nil)
	initial, err := svc.GetOpenAIAPIKeyHealthBreakerSettings(t.Context())
	require.NoError(t, err)
	require.False(t, initial.Enabled)
	want := &OpenAIAPIKeyHealthBreakerSettings{Enabled: true, WindowMinutes: 5, FailureThreshold: 3, CooldownMinutes: 1}
	require.NoError(t, svc.SetOpenAIAPIKeyHealthBreakerSettings(t.Context(), want))
	got, err := svc.GetOpenAIAPIKeyHealthBreakerSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, 1, repo.getCalls, "same node uses the updated cache immediately")
	fresh, err := NewSettingService(repo, nil).GetOpenAIAPIKeyHealthBreakerSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, want, fresh)

	repo.setErr = errors.New("synthetic write failure")
	failed := *want
	failed.Enabled = false
	require.ErrorIs(t, svc.SetOpenAIAPIKeyHealthBreakerSettings(t.Context(), &failed), repo.setErr)
	got, err = svc.GetOpenAIAPIKeyHealthBreakerSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, want, got, "failed write must not publish opt-out")
	got.Enabled = false
	got, err = svc.GetOpenAIAPIKeyHealthBreakerSettings(t.Context())
	require.NoError(t, err)
	require.True(t, got.Enabled, "callers must not mutate the cached value")
}

type blockedOpenAIHealthSettingsRepo struct {
	SettingRepository
	entered chan struct{}
	release chan struct{}
	value   string
}

func (r *blockedOpenAIHealthSettingsRepo) GetValue(context.Context, string) (string, error) {
	value := r.value
	close(r.entered)
	<-r.release
	return value, nil
}

func (r *blockedOpenAIHealthSettingsRepo) Set(_ context.Context, _, value string) error {
	r.value = value
	return nil
}

func TestOpenAIAPIKeyHealthBreakerSettingsRefreshCannotUndoSave(t *testing.T) {
	repo := &blockedOpenAIHealthSettingsRepo{
		entered: make(chan struct{}), release: make(chan struct{}),
		value: `{"enabled":true,"window_minutes":2,"failure_threshold":10,"cooldown_minutes":5}`,
	}
	svc := NewSettingService(repo, nil)
	read := make(chan error, 1)
	go func() { _, err := svc.GetOpenAIAPIKeyHealthBreakerSettings(t.Context()); read <- err }()
	<-repo.entered
	want := &OpenAIAPIKeyHealthBreakerSettings{Enabled: false, WindowMinutes: 5, FailureThreshold: 3, CooldownMinutes: 1}
	write := make(chan error, 1)
	go func() { write <- svc.SetOpenAIAPIKeyHealthBreakerSettings(t.Context(), want) }()
	close(repo.release)
	require.NoError(t, <-read)
	require.NoError(t, <-write)
	got, err := svc.GetOpenAIAPIKeyHealthBreakerSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, want, got)
}
