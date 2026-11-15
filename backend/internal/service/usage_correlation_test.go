package service

import (
	"context"
	"strings"
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

func TestUsageCorrelation64BytesFitsBillingIdentity(t *testing.T) {
	id := strings.Repeat("a", 64)
	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "same-client-across-retries")
	ctx = context.WithValue(ctx, ctxkey.RequestID, id)
	billingID := resolveUsageBillingRequestID(ctx, "upstream")
	require.LessOrEqual(t, len(billingID), 64)
	require.Equal(t, billingID, resolveUsageBillingRequestID(ctx, "another-upstream"))
	require.NotEqual(t, billingID, correlatedUsageBillingRequestID("local", strings.Repeat("b", 64)))
	usage := &UsageLog{RequestID: billingID}
	applyUsageCorrelationContext(ctx, usage)
	require.Equal(t, "same-client-across-retries", *usage.ClientRequestID)
	require.Equal(t, id, *usage.GatewayRequestID)
	require.Equal(t, "local:short-id", correlatedUsageBillingRequestID("local", "short-id"))
}
