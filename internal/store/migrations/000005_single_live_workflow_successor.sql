CREATE UNIQUE INDEX jobs_one_live_workflow_successor_idx
    ON jobs (workflow_id)
    WHERE workflow_id IS NOT NULL
      AND kind = 'PREPARE_AGENT_TURN'
      AND status IN ('AVAILABLE', 'LEASED');
