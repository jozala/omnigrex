package github

import (
	"errors"
	"fmt"
	"strings"
)

const MarkerVersion = "v1"

var ErrInvalidMarkerToken = errors.New("invalid Omnigrex marker token")

type Marker struct {
	WorkflowID        string
	AgentAssignmentID string
	OperationID       string
}

func RenderMarker(marker Marker) (string, error) {
	if err := validateMarker(marker, true); err != nil {
		return "", err
	}
	var rendered strings.Builder
	rendered.WriteString("<!-- omnigrex:")
	rendered.WriteString(MarkerVersion)
	rendered.WriteString(" workflow=")
	rendered.WriteString(marker.WorkflowID)
	if marker.AgentAssignmentID != "" {
		rendered.WriteString(" assignment=")
		rendered.WriteString(marker.AgentAssignmentID)
	}
	if marker.OperationID != "" {
		rendered.WriteString(" operation=")
		rendered.WriteString(marker.OperationID)
	}
	rendered.WriteString(" -->")
	return rendered.String(), nil
}

func ParseMarkers(text string) []Marker {
	const prefix = "<!-- omnigrex:"
	markers := make([]Marker, 0)
	for offset := 0; offset < len(text); {
		start := strings.Index(text[offset:], prefix)
		if start < 0 {
			break
		}
		start += offset
		end := strings.Index(text[start+4:], "-->")
		next := strings.Index(text[start+len(prefix):], prefix)
		if next >= 0 {
			next += start + len(prefix)
		}
		if next >= 0 && (end < 0 || next < start+4+end) {
			offset = next
			continue
		}
		if end < 0 {
			break
		}
		end += start + 4
		if marker, ok := parseMarkerComment(text[start+4 : end]); ok {
			markers = append(markers, marker)
		}
		offset = end + 3
	}
	return markers
}

func EnsureMarker(text string, marker Marker) (string, error) {
	rendered, err := RenderMarker(marker)
	if err != nil {
		return "", err
	}
	for _, existing := range ParseMarkers(text) {
		if existing == marker {
			return text, nil
		}
	}
	if text == "" {
		return rendered, nil
	}
	separator := "\n\n"
	if strings.HasSuffix(text, "\n\n") {
		separator = ""
	} else if strings.HasSuffix(text, "\n") {
		separator = "\n"
	}
	return text + separator + rendered, nil
}

func FindMarker(text string, query Marker) (Marker, bool, error) {
	if err := validateMarker(query, false); err != nil {
		return Marker{}, false, err
	}
	for _, marker := range ParseMarkers(text) {
		if query.WorkflowID != "" && marker.WorkflowID != query.WorkflowID {
			continue
		}
		if query.AgentAssignmentID != "" && marker.AgentAssignmentID != query.AgentAssignmentID {
			continue
		}
		if query.OperationID != "" && marker.OperationID != query.OperationID {
			continue
		}
		return marker, true, nil
	}
	return Marker{}, false, nil
}

func parseMarkerComment(comment string) (Marker, bool) {
	if len(comment) < 2 || comment[0] != ' ' || comment[len(comment)-1] != ' ' {
		return Marker{}, false
	}
	fields := strings.Split(comment[1:len(comment)-1], " ")
	if len(fields) < 2 || len(fields) > 4 || fields[0] != "omnigrex:"+MarkerVersion {
		return Marker{}, false
	}
	workflowID, ok := markerField(fields[1], "workflow")
	if !ok {
		return Marker{}, false
	}
	marker := Marker{WorkflowID: workflowID}
	index := 2
	if index < len(fields) {
		if assignmentID, assignment := markerField(fields[index], "assignment"); assignment {
			marker.AgentAssignmentID = assignmentID
			index++
		}
	}
	if index < len(fields) {
		if operationID, operation := markerField(fields[index], "operation"); operation {
			marker.OperationID = operationID
			index++
		}
	}
	if index != len(fields) || validateMarker(marker, true) != nil {
		return Marker{}, false
	}
	return marker, true
}

func markerField(field, name string) (string, bool) {
	prefix := name + "="
	if !strings.HasPrefix(field, prefix) {
		return "", false
	}
	return strings.TrimPrefix(field, prefix), true
}

func validateMarker(marker Marker, requireWorkflow bool) error {
	if requireWorkflow && !validMarkerToken(marker.WorkflowID) {
		return fmt.Errorf("%w: workflow id", ErrInvalidMarkerToken)
	}
	if !requireWorkflow && marker.WorkflowID == "" && marker.AgentAssignmentID == "" && marker.OperationID == "" {
		return fmt.Errorf("%w: empty lookup", ErrInvalidMarkerToken)
	}
	for name, value := range map[string]string{
		"workflow id":         marker.WorkflowID,
		"agent assignment id": marker.AgentAssignmentID,
		"operation id":        marker.OperationID,
	} {
		if value != "" && !validMarkerToken(value) {
			return fmt.Errorf("%w: %s", ErrInvalidMarkerToken, name)
		}
	}
	return nil
}

func validMarkerToken(value string) bool {
	if len(value) == 0 || len(value) > 128 || !asciiAlphaNumeric(value[0]) {
		return false
	}
	for index := 1; index < len(value); index++ {
		if !asciiAlphaNumeric(value[index]) && !strings.ContainsRune("-_.:", rune(value[index])) {
			return false
		}
	}
	return true
}

func asciiAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}
