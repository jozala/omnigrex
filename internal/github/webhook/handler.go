package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jozala/omnigrex/internal/store"
)

const maxPayloadBytes = 1 << 20

// DeliveryInbox is the durable insert boundary used by Handler.
type DeliveryInbox interface {
	InsertWebhookDelivery(context.Context, store.WebhookDelivery) (bool, error)
}

// Handler authenticates GitHub webhooks and writes them to the durable inbox.
type Handler struct {
	secret  []byte
	inbox   DeliveryInbox
	onError func(error)
}

// NewHandler creates a GitHub webhook HTTP handler.
func NewHandler(secret []byte, inbox DeliveryInbox, onError func(error)) (*Handler, error) {
	if len(secret) == 0 {
		return nil, errors.New("webhook secret is empty")
	}
	if inbox == nil {
		return nil, errors.New("webhook inbox is nil")
	}
	return &Handler{secret: append([]byte(nil), secret...), inbox: inbox, onError: onError}, nil
}

// ServeHTTP handles one GitHub webhook request.
func (handler *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		response.Header().Set("Allow", http.MethodPost)
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxPayloadBytes+1))
	if err != nil {
		http.Error(response, "read webhook body", http.StatusBadRequest)
		return
	}
	if len(body) > maxPayloadBytes {
		http.Error(response, "webhook body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !validSignature(handler.secret, body, request.Header.Get("X-Hub-Signature-256")) {
		http.Error(response, "invalid webhook signature", http.StatusUnauthorized)
		return
	}

	deliveryID, ok := canonicalUUID(request.Header.Get("X-GitHub-Delivery"))
	eventName := strings.TrimSpace(request.Header.Get("X-GitHub-Event"))
	if !ok || eventName == "" {
		http.Error(response, "invalid webhook headers", http.StatusBadRequest)
		return
	}
	var envelope struct {
		Action     string `json:"action"`
		Repository struct {
			ID    int64  `json:"id"`
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
		Issue *struct {
			ID     int64 `json:"id"`
			Number int64 `json:"number"`
		} `json:"issue"`
	}
	if workflowEvent(eventName) {
		if err := json.Unmarshal(body, &envelope); err != nil || envelope.Repository.ID <= 0 || strings.TrimSpace(envelope.Repository.Name) == "" || strings.TrimSpace(envelope.Repository.Owner.Login) == "" {
			http.Error(response, "invalid webhook payload", http.StatusBadRequest)
			return
		}
		if (envelope.Issue != nil && (envelope.Issue.ID <= 0 || envelope.Issue.Number <= 0)) || (eventName == "issues" && envelope.Issue == nil) {
			http.Error(response, "invalid webhook Issue identity", http.StatusBadRequest)
			return
		}
	}

	headers := make(map[string]string)
	for _, name := range []string{
		"Content-Type",
		"User-Agent",
		"X-GitHub-Delivery",
		"X-GitHub-Event",
		"X-GitHub-Hook-ID",
		"X-GitHub-Hook-Installation-Target-ID",
		"X-GitHub-Hook-Installation-Target-Type",
	} {
		if value := request.Header.Get(name); value != "" {
			headers[name] = value
		}
	}
	delivery := store.WebhookDelivery{
		DeliveryID:      deliveryID,
		EventName:       eventName,
		Action:          envelope.Action,
		RepositoryID:    envelope.Repository.ID,
		RepositoryOwner: envelope.Repository.Owner.Login,
		RepositoryName:  envelope.Repository.Name,
		Headers:         headers,
		Payload:         append([]byte(nil), body...),
	}
	if envelope.Issue != nil {
		delivery.IssueID = envelope.Issue.ID
		delivery.IssueNumber = envelope.Issue.Number
	}
	_, err = handler.inbox.InsertWebhookDelivery(request.Context(), delivery)
	if err != nil {
		if handler.onError != nil {
			handler.onError(fmt.Errorf("store GitHub webhook delivery %s (%s.%s): %w", delivery.DeliveryID, delivery.EventName, delivery.Action, err))
		}
		http.Error(response, "store webhook delivery", http.StatusInternalServerError)
		return
	}
	response.WriteHeader(http.StatusAccepted)
}

func workflowEvent(eventName string) bool {
	return eventName == "issues" || eventName == "pull_request" || eventName == "pull_request_review"
}

func validSignature(secret, body []byte, signature string) bool {
	algorithm, encoded, ok := strings.Cut(signature, "=")
	if !ok || algorithm != "sha256" {
		return false
	}
	want, err := hex.DecodeString(encoded)
	if err != nil || len(want) != sha256.Size {
		return false
	}
	digest := hmac.New(sha256.New, secret)
	_, _ = digest.Write(body)
	return hmac.Equal(digest.Sum(nil), want)
}

func canonicalUUID(value string) (string, bool) {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return "", false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return "", false
		}
	}
	return strings.ToLower(value), true
}
