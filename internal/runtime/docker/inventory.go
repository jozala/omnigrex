package docker

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	mobyclient "github.com/moby/moby/client"
)

const (
	// RuntimeProcessMarkerLabel distinguishes managed Runtime Processes from other containers.
	RuntimeProcessMarkerLabel = "io.omnigrex.runtime-process"
	RuntimeProcessMarkerValue = "true"

	RuntimeProcessAssignmentLabel = "io.omnigrex.assignment"
	RuntimeProcessSessionLabel    = "io.omnigrex.agent-session"
	RuntimeProcessTurnLabel       = "io.omnigrex.agent-turn"
	RuntimeProcessEpochLabel      = "io.omnigrex.execution-epoch"
	RuntimeProfileIdentityLabel   = "io.omnigrex.runtime-profile"
)

// RuntimeProfileIdentity is the immutable name and version of a Runtime Profile.
type RuntimeProfileIdentity struct {
	Name    string
	Version string
}

// RuntimeProcessIdentity is the non-secret immutable identity of one Runtime Process.
type RuntimeProcessIdentity struct {
	AssignmentID   string
	AgentSessionID string
	AgentTurnID    string
	ExecutionEpoch uint64
	RuntimeProfile RuntimeProfileIdentity
}

// ManagedRuntimeProcess associates one unambiguous identity with Docker evidence.
type ManagedRuntimeProcess struct {
	Identity    RuntimeProcessIdentity
	ContainerID string
}

// DuplicateManagedRuntimeProcess reports every container claiming one identity.
type DuplicateManagedRuntimeProcess struct {
	Identity     RuntimeProcessIdentity
	ContainerIDs []string
}

// MalformedRuntimeProcessReason identifies why a marked container was quarantined.
type MalformedRuntimeProcessReason string

const (
	MalformedMissingContainerID MalformedRuntimeProcessReason = "missing_container_id"
	MalformedRuntimeMarker      MalformedRuntimeProcessReason = "runtime_marker_mismatch"
	MalformedIncompleteIdentity MalformedRuntimeProcessReason = "incomplete_identity"
	MalformedAssignmentID       MalformedRuntimeProcessReason = "invalid_assignment_id"
	MalformedSessionID          MalformedRuntimeProcessReason = "invalid_agent_session_id"
	MalformedTurnID             MalformedRuntimeProcessReason = "invalid_agent_turn_id"
	MalformedExecutionEpoch     MalformedRuntimeProcessReason = "invalid_execution_epoch"
	MalformedRuntimeProfile     MalformedRuntimeProcessReason = "invalid_runtime_profile"
)

// MalformedManagedRuntimeProcess reports Docker evidence without copying untrusted labels.
type MalformedManagedRuntimeProcess struct {
	ContainerID string
	Reason      MalformedRuntimeProcessReason
}

// RuntimeProcessInventorySnapshot separates unique, duplicate, and malformed containers.
// Duplicate and malformed containers never appear in Processes.
type RuntimeProcessInventorySnapshot struct {
	Processes  []ManagedRuntimeProcess
	Duplicates []DuplicateManagedRuntimeProcess
	Malformed  []MalformedManagedRuntimeProcess
}

type runtimeProcessInventoryAPI interface {
	ContainerList(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error)
}

// RuntimeProcessInventory lists current marked Runtime Processes and legacy processes carrying established identity labels.
type RuntimeProcessInventory struct {
	api   runtimeProcessInventoryAPI
	close func() error
}

// NewRuntimeProcessInventory creates an inventory client from the Docker environment.
func NewRuntimeProcessInventory() (*RuntimeProcessInventory, error) {
	client, err := mobyclient.New(mobyclient.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("create Runtime Process inventory Docker client: %w", err)
	}
	return newRuntimeProcessInventory(client, client.Close), nil
}

func newRuntimeProcessInventory(api runtimeProcessInventoryAPI, close func() error) *RuntimeProcessInventory {
	return &RuntimeProcessInventory{api: api, close: close}
}

// Close releases the Docker client resources owned by the inventory.
func (inventory *RuntimeProcessInventory) Close() error {
	return inventory.close()
}

// List returns a deterministic snapshot of Runtime Process candidates without classifying unrelated containers.
func (inventory *RuntimeProcessInventory) List(ctx context.Context) (RuntimeProcessInventorySnapshot, error) {
	identityLabels := []string{
		RuntimeProcessAssignmentLabel,
		RuntimeProcessSessionLabel,
		RuntimeProcessTurnLabel,
		RuntimeProcessEpochLabel,
		RuntimeProfileIdentityLabel,
	}
	filters := []mobyclient.Filters{
		make(mobyclient.Filters).Add("label", RuntimeProcessMarkerLabel+"="+RuntimeProcessMarkerValue),
		make(mobyclient.Filters).Add("label", identityLabels...),
	}
	listedCandidates := make([]mobyclient.ContainerListResult, 0, len(filters))
	for _, filter := range filters {
		listed, err := inventory.api.ContainerList(ctx, mobyclient.ContainerListOptions{All: true, Filters: filter})
		if err != nil {
			return RuntimeProcessInventorySnapshot{}, fmt.Errorf("list managed Runtime Processes: %w", err)
		}
		listedCandidates = append(listedCandidates, listed)
	}

	byIdentity := make(map[RuntimeProcessIdentity][]string)
	snapshot := RuntimeProcessInventorySnapshot{}
	seen := make(map[string]struct{})
	for _, listed := range listedCandidates {
		for _, candidate := range listed.Items {
			if _, duplicate := seen[candidate.ID]; duplicate {
				continue
			}
			seen[candidate.ID] = struct{}{}
			if !runtimeProcessCandidateLabels(candidate.Labels) {
				continue
			}
			identity, reason := parseRuntimeProcessIdentity(candidate.ID, candidate.Labels)
			if reason != "" {
				snapshot.Malformed = append(snapshot.Malformed, MalformedManagedRuntimeProcess{
					ContainerID: candidate.ID,
					Reason:      reason,
				})
				continue
			}
			byIdentity[identity] = append(byIdentity[identity], candidate.ID)
		}
	}
	for identity, containerIDs := range byIdentity {
		slices.Sort(containerIDs)
		if len(containerIDs) > 1 {
			snapshot.Duplicates = append(snapshot.Duplicates, DuplicateManagedRuntimeProcess{
				Identity: identity, ContainerIDs: slices.Clone(containerIDs),
			})
			continue
		}
		snapshot.Processes = append(snapshot.Processes, ManagedRuntimeProcess{
			Identity: identity, ContainerID: containerIDs[0],
		})
	}

	slices.SortFunc(snapshot.Processes, func(left, right ManagedRuntimeProcess) int {
		return strings.Compare(left.ContainerID, right.ContainerID)
	})
	slices.SortFunc(snapshot.Duplicates, func(left, right DuplicateManagedRuntimeProcess) int {
		return compareRuntimeProcessIdentity(left.Identity, right.Identity)
	})
	slices.SortFunc(snapshot.Malformed, func(left, right MalformedManagedRuntimeProcess) int {
		if compared := strings.Compare(left.ContainerID, right.ContainerID); compared != 0 {
			return compared
		}
		return strings.Compare(string(left.Reason), string(right.Reason))
	})
	return snapshot, nil
}

func parseRuntimeProcessIdentity(containerID string, labels map[string]string) (RuntimeProcessIdentity, MalformedRuntimeProcessReason) {
	if containerID == "" {
		return RuntimeProcessIdentity{}, MalformedMissingContainerID
	}
	if marker, present := labels[RuntimeProcessMarkerLabel]; present && marker != RuntimeProcessMarkerValue {
		return RuntimeProcessIdentity{}, MalformedRuntimeMarker
	}
	required := []string{
		RuntimeProcessAssignmentLabel,
		RuntimeProcessSessionLabel,
		RuntimeProcessTurnLabel,
		RuntimeProcessEpochLabel,
		RuntimeProfileIdentityLabel,
	}
	for _, name := range required {
		if labels[name] == "" {
			return RuntimeProcessIdentity{}, MalformedIncompleteIdentity
		}
	}
	if !validCanonicalRuntimeUUID(labels[RuntimeProcessAssignmentLabel]) {
		return RuntimeProcessIdentity{}, MalformedAssignmentID
	}
	if !validCanonicalRuntimeUUID(labels[RuntimeProcessSessionLabel]) {
		return RuntimeProcessIdentity{}, MalformedSessionID
	}
	if !validCanonicalRuntimeUUID(labels[RuntimeProcessTurnLabel]) {
		return RuntimeProcessIdentity{}, MalformedTurnID
	}
	epochText := labels[RuntimeProcessEpochLabel]
	epoch, err := strconv.ParseUint(epochText, 10, 64)
	if err != nil || epoch == 0 || strconv.FormatUint(epoch, 10) != epochText {
		return RuntimeProcessIdentity{}, MalformedExecutionEpoch
	}
	profile, ok := parseRuntimeProfileIdentity(labels[RuntimeProfileIdentityLabel])
	if !ok {
		return RuntimeProcessIdentity{}, MalformedRuntimeProfile
	}
	return RuntimeProcessIdentity{
		AssignmentID:   labels[RuntimeProcessAssignmentLabel],
		AgentSessionID: labels[RuntimeProcessSessionLabel],
		AgentTurnID:    labels[RuntimeProcessTurnLabel],
		ExecutionEpoch: epoch,
		RuntimeProfile: profile,
	}, ""
}

func runtimeProcessCandidateLabels(labels map[string]string) bool {
	marker, markerPresent := labels[RuntimeProcessMarkerLabel]
	if markerPresent {
		return marker == RuntimeProcessMarkerValue
	}
	_, reason := parseRuntimeProcessIdentity("legacy-runtime-process", labels)
	return reason == ""
}

func validCanonicalRuntimeUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return value != "00000000-0000-0000-0000-000000000000"
}

func parseRuntimeProfileIdentity(value string) (RuntimeProfileIdentity, bool) {
	name, version, found := strings.Cut(value, "/")
	if !found || strings.Contains(version, "/") || !validRuntimeProfileIdentityPart(name) || !validRuntimeProfileIdentityPart(version) {
		return RuntimeProfileIdentity{}, false
	}
	return RuntimeProfileIdentity{Name: name, Version: version}, true
}

func validRuntimeProfileIdentityPart(value string) bool {
	if value == "" || len(value) > 128 || !isLowerAlphaNumeric(value[0]) || !isLowerAlphaNumeric(value[len(value)-1]) {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if !isLowerAlphaNumeric(character) && character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func isLowerAlphaNumeric(character byte) bool {
	return (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9')
}

func compareRuntimeProcessIdentity(left, right RuntimeProcessIdentity) int {
	leftKey := left.AssignmentID + "\x00" + left.AgentSessionID + "\x00" + left.AgentTurnID + "\x00" +
		strconv.FormatUint(left.ExecutionEpoch, 10) + "\x00" + left.RuntimeProfile.Name + "\x00" + left.RuntimeProfile.Version
	rightKey := right.AssignmentID + "\x00" + right.AgentSessionID + "\x00" + right.AgentTurnID + "\x00" +
		strconv.FormatUint(right.ExecutionEpoch, 10) + "\x00" + right.RuntimeProfile.Name + "\x00" + right.RuntimeProfile.Version
	return strings.Compare(leftKey, rightKey)
}
