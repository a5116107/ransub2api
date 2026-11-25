package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBalanceForecastRetainedNotification(t *testing.T) {
	svc := NewNotificationEmailService(newNotificationEmailMemorySettingRepo(), nil)
	var forecast, low bool
	for _, event := range svc.ListEventInfos() {
		forecast = forecast || event.Event == NotificationEmailEventBalanceForecast
		low = low || event.Event == NotificationEmailEventBalanceLow
	}
	require.True(t, forecast)
	require.True(t, low)
	for _, event := range []string{NotificationEmailEventBalanceForecast, NotificationEmailEventBalanceLow} {
		_, err := svc.GetTemplate(context.Background(), event, "zh")
		require.NoError(t, err)
	}
}
