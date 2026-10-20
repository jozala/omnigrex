package agentturn

// SubscriptionAccess is an optional deployment-owned admission and failure
// boundary. It never exposes provider tokens to Workflow policy.
type SubscriptionAccess interface {
	Check() error
	Report(string, string) error
}
