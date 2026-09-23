ALTER TABLE normalized_events
    VALIDATE CONSTRAINT normalized_events_attempt_limit_check;

ALTER TABLE normalized_events
    VALIDATE CONSTRAINT normalized_events_status_check;

ALTER TABLE normalized_events
    VALIDATE CONSTRAINT normalized_events_outcome_check;
