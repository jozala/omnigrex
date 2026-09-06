package docker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	mobyclient "github.com/moby/moby/client"
)

const (
	assignmentRuntimeStateMount = "/volumes/runtime-state"

	assignmentRuntimeStateCleanerMemoryBytes = 128 << 20
	assignmentRuntimeStateCleanerPIDsLimit   = 32
)

var ErrInvalidAssignmentRuntimeStateCleanup = errors.New("invalid Assignment runtime-state cleanup")

// AssignmentRuntimeStateCleanerOptions pins the helper image and named runtime-state volume.
type AssignmentRuntimeStateCleanerOptions struct {
	Image              string
	Platform           Platform
	User               string
	RuntimeStateVolume string
}

type assignmentRuntimeStateCleanupAPI interface {
	oneShotAPI
	VolumeInspect(context.Context, string, mobyclient.VolumeInspectOptions) (mobyclient.VolumeInspectResult, error)
}

// AssignmentRuntimeStateCleaner removes one canonical Assignment runtime-state subpath.
type AssignmentRuntimeStateCleaner struct {
	api     assignmentRuntimeStateCleanupAPI
	options AssignmentRuntimeStateCleanerOptions
	close   func() error
}

// NewAssignmentRuntimeStateCleaner creates a cleanup client from the Docker environment.
func NewAssignmentRuntimeStateCleaner(options AssignmentRuntimeStateCleanerOptions) (*AssignmentRuntimeStateCleaner, error) {
	if err := validateAssignmentRuntimeStateCleanerOptions(options); err != nil {
		return nil, err
	}
	client, err := mobyclient.New(mobyclient.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("create Assignment runtime-state cleanup Docker client: %w", err)
	}
	return &AssignmentRuntimeStateCleaner{api: client, options: options, close: client.Close}, nil
}

func newAssignmentRuntimeStateCleaner(api assignmentRuntimeStateCleanupAPI, close func() error, options AssignmentRuntimeStateCleanerOptions) (*AssignmentRuntimeStateCleaner, error) {
	if err := validateAssignmentRuntimeStateCleanerOptions(options); err != nil {
		return nil, err
	}
	return &AssignmentRuntimeStateCleaner{api: api, options: options, close: close}, nil
}

// Close releases the Docker client resources owned by the cleaner.
func (cleaner *AssignmentRuntimeStateCleaner) Close() error {
	return cleaner.close()
}

// EnsureAbsent idempotently deletes exactly assignment-<UUID>/runtime-state.
func (cleaner *AssignmentRuntimeStateCleaner) EnsureAbsent(ctx context.Context, assignmentID, runtimeStateSubpath string) error {
	if !validCanonicalRuntimeUUID(assignmentID) || runtimeStateSubpath != "assignment-"+assignmentID+"/runtime-state" {
		return fmt.Errorf("%w: path is not canonical for the Assignment", ErrInvalidAssignmentRuntimeStateCleanup)
	}
	if _, err := cleaner.api.VolumeInspect(ctx, cleaner.options.RuntimeStateVolume, mobyclient.VolumeInspectOptions{}); err != nil {
		return fmt.Errorf("inspect Assignment runtime-state volume: %w", err)
	}
	if err := runOneShotContainer(ctx, cleaner.api, cleaner.container(assignmentID), "Assignment runtime-state cleanup"); err != nil {
		return err
	}
	return nil
}

func (cleaner *AssignmentRuntimeStateCleaner) container(assignmentID string) mobyclient.ContainerCreateOptions {
	pids := int64(assignmentRuntimeStateCleanerPIDsLimit)
	script := `set -eu
assignment_root="/volumes/runtime-state/assignment-$1"
target="$assignment_root/runtime-state"
if [ -L "$assignment_root" ]; then
  exit 64
fi
rm -rf -- "$target"
test ! -e "$target"
test ! -L "$target"`
	platform := cleaner.options.Platform
	return mobyclient.ContainerCreateOptions{
		Platform: &platform,
		Config: &container.Config{
			Image:      cleaner.options.Image,
			User:       cleaner.options.User,
			Entrypoint: []string{"sh", "-c"},
			Cmd:        []string{script, "clean-assignment-runtime-state", assignmentID},
			Labels:     map[string]string{"io.omnigrex.assignment-runtime-state-cleaner": "true"},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    "none",
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			Mounts: []mount.Mount{{
				Type: mount.TypeVolume, Source: cleaner.options.RuntimeStateVolume, Target: assignmentRuntimeStateMount,
			}},
			Resources: container.Resources{
				Memory:    assignmentRuntimeStateCleanerMemoryBytes,
				PidsLimit: &pids,
			},
		},
	}
}

func validateAssignmentRuntimeStateCleanerOptions(options AssignmentRuntimeStateCleanerOptions) error {
	if !validImageDigest(options.Image) {
		return fmt.Errorf("%w: helper image must be digest-qualified", ErrInvalidAssignmentRuntimeStateCleanup)
	}
	if !validPlatform(options.Platform) {
		return fmt.Errorf("%w: helper platform must be exactly linux/amd64 or linux/arm64", ErrInvalidAssignmentRuntimeStateCleanup)
	}
	if !validNonRootNumericUser(options.User) {
		return fmt.Errorf("%w: helper user must contain a canonical non-root UID and GID", ErrInvalidAssignmentRuntimeStateCleanup)
	}
	if !validNamedVolume(options.RuntimeStateVolume) {
		return fmt.Errorf("%w: runtime-state volume must be a Docker volume name", ErrInvalidAssignmentRuntimeStateCleanup)
	}
	return nil
}

func validNonRootNumericUser(user string) bool {
	uid, gid, found := strings.Cut(user, ":")
	if !found {
		return false
	}
	uidNumber, uidErr := strconv.ParseUint(uid, 10, 32)
	gidNumber, gidErr := strconv.ParseUint(gid, 10, 32)
	return uidErr == nil && gidErr == nil && uidNumber != 0 && gidNumber != 0 &&
		strconv.FormatUint(uidNumber, 10) == uid && strconv.FormatUint(gidNumber, 10) == gid
}

func validNamedVolume(value string) bool {
	if len(value) < 2 || strings.TrimSpace(value) != value {
		return false
	}
	for index := range len(value) {
		character := value[index]
		alphaNumeric := (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9')
		if !alphaNumeric && (index == 0 || (character != '_' && character != '.' && character != '-')) {
			return false
		}
	}
	return true
}
