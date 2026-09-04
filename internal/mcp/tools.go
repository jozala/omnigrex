package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"

	"github.com/jozala/omnigrex/internal/workflow"
)

var errInvalidArguments = errors.New("invalid tool arguments")

type ToolClass string

const (
	ReadTool     ToolClass = "READ"
	MutationTool ToolClass = "MUTATION"
)

const (
	ToolGetIssue               = "get_issue"
	ToolListIssueComments      = "list_issue_comments"
	ToolGetPullRequest         = "get_pull_request"
	ToolListPullRequestReviews = "list_pull_request_reviews"
	ToolListReviewThreads      = "list_review_threads"
	ToolGetCheckRuns           = "get_check_runs"
	ToolPublishChanges         = "publish_changes"
	ToolOpenPR                 = "open_pr"
	ToolRequestReview          = "request_review"
	ToolSubmitReview           = "submit_review"
	ToolCommentOnIssue         = "comment_on_issue"
	ToolCommentOnPullRequest   = "comment_on_pull_request"
	ToolReportBlocked          = "report_blocked"
)

type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Class       ToolClass      `json:"-"`
}

var toolCatalog = []ToolDefinition{
	{Name: ToolGetIssue, Description: "Get the scoped Issue and its current metadata.", InputSchema: emptyObjectSchema(), Class: ReadTool},
	{Name: ToolListIssueComments, Description: "List comments on the scoped Issue.", InputSchema: emptyObjectSchema(), Class: ReadTool},
	{Name: ToolGetPullRequest, Description: "Get the scoped Pull Request and current head.", InputSchema: emptyObjectSchema(), Class: ReadTool},
	{Name: ToolListPullRequestReviews, Description: "List native reviews on the scoped Pull Request.", InputSchema: emptyObjectSchema(), Class: ReadTool},
	{Name: ToolListReviewThreads, Description: "List review threads on the scoped Pull Request.", InputSchema: emptyObjectSchema(), Class: ReadTool},
	{Name: ToolGetCheckRuns, Description: "Get check runs for the scoped head commit.", InputSchema: emptyObjectSchema(), Class: ReadTool},
	{Name: ToolPublishChanges, Description: "Publish workspace changes to the scoped branch.", InputSchema: objectSchema(map[string]any{
		"operation_id": operationIDSchema(), "message": stringSchema(1, 4096),
	}, "operation_id", "message"), Class: MutationTool},
	{Name: ToolOpenPR, Description: "Open the scoped branch as a Pull Request linked to the Issue.", InputSchema: objectSchema(map[string]any{
		"operation_id": operationIDSchema(), "title": stringSchema(1, 256), "body": stringSchema(1, 65536),
	}, "operation_id", "title", "body"), Class: MutationTool},
	{Name: ToolRequestReview, Description: "Record a durable handoff requesting Reviewer work.", InputSchema: objectSchema(map[string]any{
		"operation_id": operationIDSchema(), "summary": stringSchema(1, 65536),
	}, "operation_id", "summary"), Class: MutationTool},
	{Name: ToolSubmitReview, Description: "Submit a native review for the scoped Pull Request head.", InputSchema: objectSchema(map[string]any{
		"operation_id": operationIDSchema(),
		"event":        map[string]any{"type": "string", "enum": []string{"APPROVE", "REQUEST_CHANGES"}},
		"body":         stringSchema(1, 65536),
		"comments": map[string]any{
			"type": "array", "maxItems": 100,
			"items": objectSchema(map[string]any{
				"path": stringSchema(1, 4096), "line": integerSchema(1, 1_000_000),
				"side":       map[string]any{"type": "string", "enum": []string{"LEFT", "RIGHT"}},
				"start_line": integerSchema(1, 1_000_000),
				"start_side": map[string]any{"type": "string", "enum": []string{"LEFT", "RIGHT"}},
				"body":       stringSchema(1, 65536),
			}, "path", "line", "side", "body"),
		},
	}, "operation_id", "event"), Class: MutationTool},
	{Name: ToolCommentOnIssue, Description: "Add an idempotent comment to the scoped Issue.", InputSchema: objectSchema(map[string]any{
		"operation_id": operationIDSchema(), "body": stringSchema(1, 65536),
	}, "operation_id", "body"), Class: MutationTool},
	{Name: ToolCommentOnPullRequest, Description: "Add an idempotent comment to the scoped Pull Request.", InputSchema: objectSchema(map[string]any{
		"operation_id": operationIDSchema(), "body": stringSchema(1, 65536),
	}, "operation_id", "body"), Class: MutationTool},
	{Name: ToolReportBlocked, Description: "Create a Human Handoff for a blocker.", InputSchema: objectSchema(map[string]any{
		"operation_id": operationIDSchema(), "reason": stringSchema(1, 4096), "details": stringSchema(1, 65536),
	}, "operation_id", "reason"), Class: MutationTool},
}

var roleTools = map[workflow.Role][]string{
	workflow.RoleDeveloper: {
		ToolGetIssue, ToolListIssueComments, ToolGetPullRequest, ToolListPullRequestReviews, ToolListReviewThreads, ToolGetCheckRuns,
		ToolPublishChanges, ToolOpenPR, ToolRequestReview, ToolCommentOnIssue, ToolCommentOnPullRequest, ToolReportBlocked,
	},
	workflow.RoleReviewer: {
		ToolGetIssue, ToolListIssueComments, ToolGetPullRequest, ToolListPullRequestReviews, ToolListReviewThreads, ToolGetCheckRuns,
		ToolSubmitReview, ToolCommentOnIssue, ToolCommentOnPullRequest, ToolReportBlocked,
	},
}

func emptyObjectSchema() map[string]any {
	return objectSchema(map[string]any{})
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) != 0 {
		schema["required"] = required
	}
	return schema
}

func stringSchema(minimum, maximum int) map[string]any {
	return map[string]any{"type": "string", "minLength": minimum, "maxLength": maximum}
}

func operationIDSchema() map[string]any {
	return map[string]any{
		"type": "string", "minLength": 1, "maxLength": 128,
		"pattern": `^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`,
	}
}

func integerSchema(minimum, maximum int) map[string]any {
	return map[string]any{"type": "integer", "minimum": minimum, "maximum": maximum}
}

func definition(name string) (ToolDefinition, bool) {
	for _, candidate := range toolCatalog {
		if candidate.Name == name {
			return candidate, true
		}
	}
	return ToolDefinition{}, false
}

func toolsForScope(scope TokenScope) ([]ToolDefinition, error) {
	allowedForRole := roleTools[scope.Role]
	requested := scope.AllowedTools
	if len(requested) == 0 {
		requested = allowedForRole
	}
	requestedSet := make(map[string]struct{}, len(requested))
	for _, name := range requested {
		if !slices.Contains(allowedForRole, name) {
			return nil, fmt.Errorf("%w: tool authorization", ErrInvalidConfiguration)
		}
		if _, duplicate := requestedSet[name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate allowed tool", ErrInvalidConfiguration)
		}
		requestedSet[name] = struct{}{}
	}
	result := make([]ToolDefinition, 0, len(requestedSet))
	for _, tool := range toolCatalog {
		if _, ok := requestedSet[tool.Name]; ok {
			result = append(result, tool)
		}
	}
	return result, nil
}

func validateArguments(raw json.RawMessage, schema map[string]any) error {
	if len(raw) == 0 {
		return errInvalidArguments
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return errInvalidArguments
	}
	return validateSchemaValue(value, schema)
}

func validateSchemaValue(value any, schema map[string]any) error {
	switch schema["type"] {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return errInvalidArguments
		}
		properties, _ := schema["properties"].(map[string]any)
		for _, required := range stringValues(schema["required"]) {
			if _, present := object[required]; !present {
				return errInvalidArguments
			}
		}
		for name, propertyValue := range object {
			propertySchema, present := properties[name]
			if !present {
				return errInvalidArguments
			}
			typed, ok := propertySchema.(map[string]any)
			if !ok || validateSchemaValue(propertyValue, typed) != nil {
				return errInvalidArguments
			}
		}
	case "string":
		text, ok := value.(string)
		if !ok || len(text) < intValue(schema["minLength"]) || (intValue(schema["maxLength"]) > 0 && len(text) > intValue(schema["maxLength"])) {
			return errInvalidArguments
		}
		if values := stringValues(schema["enum"]); len(values) != 0 && !slices.Contains(values, text) {
			return errInvalidArguments
		}
	case "integer":
		number, ok := value.(json.Number)
		integer, err := number.Int64()
		if !ok || err != nil || integer < int64(intValue(schema["minimum"])) || integer > int64(intValue(schema["maximum"])) {
			return errInvalidArguments
		}
	case "array":
		items, ok := value.([]any)
		if !ok || (intValue(schema["maxItems"]) > 0 && len(items) > intValue(schema["maxItems"])) {
			return errInvalidArguments
		}
		itemSchema, ok := schema["items"].(map[string]any)
		if !ok {
			return errInvalidArguments
		}
		for _, item := range items {
			if validateSchemaValue(item, itemSchema) != nil {
				return errInvalidArguments
			}
		}
	default:
		return errInvalidArguments
	}
	return nil
}

func stringValues(value any) []string {
	switch values := value.(type) {
	case []string:
		return values
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				return nil
			}
			result = append(result, text)
		}
		return result
	default:
		return nil
	}
}

func intValue(value any) int {
	switch number := value.(type) {
	case int:
		return number
	case float64:
		if number >= 0 && number <= math.MaxInt && math.Trunc(number) == number {
			return int(number)
		}
	case json.Number:
		parsed, err := number.Int64()
		if err == nil && parsed >= 0 && parsed <= math.MaxInt {
			return int(parsed)
		}
	}
	return 0
}
