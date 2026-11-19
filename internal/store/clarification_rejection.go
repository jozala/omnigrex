package store

import "strings"

// clarificationRejectionSQL mirrors collaboration.IsRequestRejection for JSONB
// ledger selectors. Missing waiting evidence alone must never hide malformed
// terminal evidence. column is repository-owned SQL, never caller input.
func clarificationRejectionSQL(column string) string {
	return strings.ReplaceAll(`COALESCE((
    RESULT = jsonb_build_object('status','rejected','publication','not_applied',
        'reason',RESULT->>'reason','question_index',RESULT->'question_index',
        'next_action',RESULT->>'next_action')
    AND RESULT->>'question_index' ~ '^(0|[1-9]|[1-4][0-9]|50)$'
    AND ((RESULT->>'reason' IN ('question_identity_content_changed','duplicate_question',
        'unresolved_question_dropped','question_limit_exceeded','empty_questions_without_wait')
        AND RESULT->>'next_action' = 'correct_arguments_new_operation')
      OR (RESULT->>'reason' = 'earlier_publication_unsettled'
        AND RESULT->>'next_action' = 'reconcile_prior_operation'))
 ), FALSE)`, "RESULT", column)
}
