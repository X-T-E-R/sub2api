CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_logs_gateway_request_id
    ON usage_logs (gateway_request_id)
    WHERE gateway_request_id IS NOT NULL;

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_logs_client_request_id
    ON usage_logs (client_request_id)
    WHERE client_request_id IS NOT NULL;
