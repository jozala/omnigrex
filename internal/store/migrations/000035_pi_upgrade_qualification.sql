-- Pi evidence has backend-specific provenance; preserve legacy immutable rows.
CREATE TABLE pi_runtime_profile_compatibility_results (
    qualification_key TEXT PRIMARY KEY CHECK (qualification_key ~ '^[0-9a-f]{64}$'),
    result JSONB NOT NULL CHECK (jsonb_typeof(result) = 'object'),
    result_sha256 BYTEA NOT NULL CHECK (octet_length(result_sha256) = 32),
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE TRIGGER pi_runtime_profile_compatibility_results_immutable
BEFORE UPDATE OR DELETE ON pi_runtime_profile_compatibility_results
FOR EACH ROW EXECUTE FUNCTION reject_runtime_profile_compatibility_result_mutation();
