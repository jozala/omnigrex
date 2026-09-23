package profile

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrProtectedBindingUnavailable means protected state refers to an unconfigured immutable profile.
	ErrProtectedBindingUnavailable = errors.New("protected Runtime Profile binding unavailable")
	// ErrProtectedImageUnavailable means an exact protected image cannot be inspected for its platform.
	ErrProtectedImageUnavailable = errors.New("protected Runtime Profile image unavailable")
)

// ProtectedBindingSource lists distinct immutable bindings whose opaque state has not been collected.
type ProtectedBindingSource interface {
	ListProtectedRuntimeBindings(context.Context) ([]Binding, error)
}

// ImageAvailability verifies one exact registry digest for one platform.
type ImageAvailability interface {
	Available(context.Context, string, Platform) error
}

// AvailabilityChecker is a readiness-compatible check for every protected Runtime Profile image.
type AvailabilityChecker struct {
	source  ProtectedBindingSource
	catalog Catalog
	images  ImageAvailability
}

func NewAvailabilityChecker(source ProtectedBindingSource, catalog Catalog, images ImageAvailability) (*AvailabilityChecker, error) {
	if source == nil || images == nil {
		return nil, errors.New("Runtime Profile availability dependency is nil")
	}
	return &AvailabilityChecker{source: source, catalog: catalog, images: images}, nil
}

// CheckBindings fails when protected opaque state cannot be resolved by the configured Catalog.
func (checker *AvailabilityChecker) CheckBindings(ctx context.Context) error {
	_, err := checker.protectedImages(ctx)
	return err
}

// Check resolves every persisted binding exactly before inspecting each distinct digest and platform.
func (checker *AvailabilityChecker) Check(ctx context.Context) error {
	protected, err := checker.protectedImages(ctx)
	if err != nil {
		return err
	}
	for _, candidate := range protected {
		if err := checker.images.Available(ctx, candidate.image, candidate.platform); err != nil {
			return fmt.Errorf("%w: %s on %s/%s", ErrProtectedImageUnavailable,
				shortDigest(candidate.image), candidate.platform.OS, candidate.platform.Arch)
		}
	}
	return nil
}

type protectedImage struct {
	image    string
	platform Platform
}

func (checker *AvailabilityChecker) protectedImages(ctx context.Context) ([]protectedImage, error) {
	if checker == nil || checker.source == nil || checker.images == nil {
		return nil, errors.New("Runtime Profile availability checker is not configured")
	}
	bindings, err := checker.source.ListProtectedRuntimeBindings(ctx)
	if err != nil {
		return nil, fmt.Errorf("list protected Runtime Profiles: %w", err)
	}
	protected := make([]protectedImage, 0, len(bindings))
	seen := make(map[protectedImage]struct{}, len(bindings))
	for _, binding := range bindings {
		resolved, err := checker.catalog.ResolveBinding(binding)
		if err != nil {
			return nil, fmt.Errorf("%w: %s/%s %s", ErrProtectedBindingUnavailable,
				binding.Name, binding.Version, shortDigest(binding.Image))
		}
		contract := resolved.Contract()
		key := protectedImage{image: contract.Image, platform: contract.Platform}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		protected = append(protected, key)
	}
	return protected, nil
}

func shortDigest(image string) string {
	_, digest, ok := strings.Cut(image, "@sha256:")
	if !ok || len(digest) < 12 {
		return "invalid-digest"
	}
	return "sha256:" + digest[:12]
}
