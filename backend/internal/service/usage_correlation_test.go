package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func TestUsageCorrelationContextPersistsWithoutObservationSnapshot(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "edge-axon:retained-42")
	ctx = context.WithValue(ctx, ctxkey.RequestID, "gateway-request-42")
	usage := &UsageLog{RequestID: "web_search:durable-money-event"}
	applyUsageCorrelationContext(ctx, usage)
	require.Equal(t, "edge-axon:retained-42", *usage.ClientRequestID)
	require.Equal(t, "gateway-request-42", *usage.GatewayRequestID)
	require.Equal(t, "web_search:durable-money-event", usage.RequestID)
	snapshotID := "snapshot-retained"
	usage.ClientRequestID = &snapshotID
	applyUsageCorrelationContext(ctx, usage)
	require.Equal(t, snapshotID, *usage.ClientRequestID)
}
