package repository

// Both admin upstream monitoring and the downstream user's experience read the
// same canonical counters. Billing buckets and missing reports are not evidence.
const rawCacheCounterJoins = `		LEFT JOIN LATERAL (
			SELECT
				CASE WHEN jsonb_typeof(cache->'input_tokens')='number'
					AND cache->>'input_tokens' ~ '^[0-9]{1,16}$'
					THEN (cache->>'input_tokens')::bigint END AS raw_input,
				CASE WHEN jsonb_typeof(cache->'read_tokens')='number'
					AND cache->>'read_tokens' ~ '^[0-9]{1,16}$'
					THEN (cache->>'read_tokens')::bigint END AS raw_read,
				CASE WHEN jsonb_typeof(cache->'write_tokens')='number'
					AND cache->>'write_tokens' ~ '^[0-9]{1,16}$'
					THEN (cache->>'write_tokens')::bigint END AS raw_write
		) counters ON true`

const rawCacheValidCounters = `raw_input BETWEEN 1 AND 9007199254740991
	AND raw_read BETWEEN 0 AND raw_input
	AND (cache->'write_tokens'='null'::jsonb OR raw_write BETWEEN 0 AND raw_input-raw_read)`

const rawCacheObservationJoins = rawCacheCounterJoins + `
		LEFT JOIN LATERAL (
			SELECT COALESCE(cache->>'status'='observed'
				AND (cache->>'source',cache->>'scope') IN (
					('canonical_responses_terminal_usage','final_physical_http_responses_terminal_usage'),
					('canonical_responses_terminal_usage','physical_websocket_turn_responses_terminal_usage'),
					('canonical_messages_cumulative_usage','final_physical_http_messages_cumulative_usage'),
					('canonical_gemini_usage_metadata','final_physical_http_gemini_usage_metadata'),
					('canonical_chat_completions_usage','final_physical_http_chat_completions_usage'))
				AND ` + rawCacheValidCounters + `, false) AS raw_valid
		) validity ON true`
