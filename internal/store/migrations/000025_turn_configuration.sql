ALTER TABLE agent_turns
    ADD COLUMN turn_configuration JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD CONSTRAINT agent_turns_turn_configuration_object CHECK (jsonb_typeof(turn_configuration) = 'object');
