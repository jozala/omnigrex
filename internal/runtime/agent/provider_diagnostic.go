package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// ProviderDiagnostic is a closed, credential-free observation, not an SDK error.
// Nil numeric/correlation fields mean unavailable. Retryability is observed,
// never inferred from the broad subscription_temporary fallback.
type ProviderDiagnostic struct {
	Attempt       string `json:"attempt,omitempty"`
	Stage         string `json:"stage"`
	Cause         string `json:"cause"`
	AuthMode      string `json:"auth_mode"`
	HTTPStatus    *int   `json:"http_status"`
	Code          string `json:"code"`
	Retryability  string `json:"retryability"`
	RequestIDHash string `json:"request_id_hash,omitempty"`
	ElapsedMS     *int   `json:"elapsed_ms"`
}

var diagnosticUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var diagnosticHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseProviderDiagnostic revalidates the Pi boundary. Malformed/oversized
// metadata never changes settlement and is never returned verbatim.
func ParseProviderDiagnostic(raw json.RawMessage) *ProviderDiagnostic {
	d := &ProviderDiagnostic{Stage: "unknown", Cause: "unknown", AuthMode: "unknown", Code: "unknown", Retryability: "unknown"}
	var input ProviderDiagnostic
	if len(raw) > 2048 || json.Unmarshal(raw, &input) != nil {
		return d
	}
	switch input.Stage {
	case "credential", "payload", "provider_http", "provider_stream", "transport", "local":
		d.Stage = input.Stage
	}
	switch input.Cause {
	case "network", "timeout", "cancelled", "protocol", "validation", "http", "stream_error":
		d.Cause = input.Cause
	}
	if input.AuthMode == "subscription" {
		d.AuthMode = input.AuthMode
	}
	switch input.Code {
	case "subscription_sharing_usage_limit_exceeded", "subscription_sharing_invalid_user", "subscription_sharing_user_not_eligible", "subscription_sharing_unsupported_capability", "subscription_sharing_route_not_supported", "chatpass_v2_scope_not_authorized", "chatpass_v2_invalid_authorization_context", "subscription_sharing_usage_unavailable", "subscription_sharing_user_unavailable", "model_not_found":
		d.Code = input.Code
	}
	switch input.Retryability {
	case "yes", "no":
		d.Retryability = input.Retryability
	}
	if input.HTTPStatus != nil && *input.HTTPStatus >= 100 && *input.HTTPStatus <= 599 {
		d.HTTPStatus = input.HTTPStatus
	}
	if input.ElapsedMS != nil && *input.ElapsedMS >= 0 && *input.ElapsedMS <= 86400000 {
		d.ElapsedMS = input.ElapsedMS
	}
	if diagnosticUUID.MatchString(input.Attempt) {
		d.Attempt = input.Attempt
	}
	if diagnosticHash.MatchString(input.RequestIDHash) {
		d.RequestIDHash = input.RequestIDHash
	}
	return d
}

// Summary uses only the validated observation; no provider text or IDs.
func (d *ProviderDiagnostic) Summary() string {
	if d == nil {
		return ""
	}
	status := "unavailable"
	if d.HTTPStatus != nil {
		status = fmt.Sprint(*d.HTTPStatus)
	}
	return fmt.Sprintf("stage=%s cause=%s http_status=%s code=%s retryability=%s", d.Stage, d.Cause, status, d.Code, d.Retryability)
}
