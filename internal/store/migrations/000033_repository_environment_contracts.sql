CREATE TABLE runtime_profile_contracts (
    content_sha256 text PRIMARY KEY CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
    contract jsonb NOT NULL CHECK (jsonb_typeof(contract) = 'object')
);

CREATE FUNCTION reject_runtime_profile_contract_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Runtime Profile contracts are immutable';
END;
$$;

CREATE TRIGGER runtime_profile_contracts_immutable
BEFORE UPDATE OR DELETE ON runtime_profile_contracts
FOR EACH ROW EXECUTE FUNCTION reject_runtime_profile_contract_mutation();
