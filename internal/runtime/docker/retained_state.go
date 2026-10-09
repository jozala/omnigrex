package docker

import (
	"github.com/jozala/omnigrex/internal/runtime/dirstate"
	"github.com/jozala/omnigrex/internal/runtime/execution"
)

type StateSource = dirstate.StateSource

type RetainedState struct{ *dirstate.RetainedState }

func NewRetainedState(root, volume string, source execution.StateMetadataSource) (*RetainedState, error) {
	if !validNamedVolume(volume) {
		return nil, execution.ErrBindingMismatch
	}
	state, err := dirstate.New(root, execution.BackendBinding{Family: "docker", Version: 1, Namespace: volume}, source)
	if err != nil {
		return nil, err
	}
	return &RetainedState{state}, nil
}

func (state *RetainedState) WithCollector(options AssignmentRuntimeStateCleanerOptions) (*RetainedState, error) {
	if options.RuntimeStateVolume != state.Binding().Namespace {
		return nil, execution.ErrBindingMismatch
	}
	cleaner, err := NewAssignmentRuntimeStateCleaner(options)
	if err != nil {
		return nil, err
	}
	state.SetCollector(cleaner)
	return state, nil
}
