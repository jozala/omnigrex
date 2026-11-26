ALTER TABLE workflows ADD COLUMN human_input_waiting boolean NOT NULL DEFAULT false;
ALTER TABLE workflows ADD COLUMN human_input_presentation_revision bigint NOT NULL DEFAULT 0;

-- Label-only upgrade work. Never enqueue preparation or mutate collaboration
-- history; the ordinary serialized worker evaluates current state on execution.
INSERT INTO jobs(id,queue,kind,payload,max_attempts,idempotency_key,workflow_id)
SELECT gen_random_uuid(),'workflow','RECONCILE_GITHUB_LABELS',
       jsonb_build_object('revision',w.state_revision,'human_input_only',true),3,
       'human-input-upgrade:' || w.id::text,w.id
FROM workflows w JOIN work_item_collaboration c ON c.workflow_id=w.id
ON CONFLICT DO NOTHING;
