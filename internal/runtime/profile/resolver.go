package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// ContractSource retrieves immutable canonical definitions from the operational ledger.
type ContractSource interface {
	RuntimeProfileContract(context.Context, Binding) (json.RawMessage, error)
}

type Resolver struct {
	Catalog
	Source ContractSource
}

func (resolver Resolver) ResolveBinding(binding Binding) (Profile, error) {
	if resolver.Source != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		content, err := resolver.Source.RuntimeProfileContract(ctx, binding)
		if err == nil {
			return DecodeContract(content, binding)
		}
		if !errors.Is(err, ErrNotFound) {
			return Profile{}, err
		}
	}
	return resolver.Catalog.ResolveBinding(binding)
}

func DecodeContract(content json.RawMessage, binding Binding) (Profile, error) {
	if len(content) == 0 || len(content) > 64<<10 {
		return Profile{}, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var contract Contract
	if decoder.Decode(&contract) != nil || decoder.Decode(new(any)) != io.EOF {
		return Profile{}, ErrInvalid
	}
	profile, err := New(contract)
	if err != nil {
		return Profile{}, err
	}
	if profile.Binding() != binding {
		return Profile{}, ErrConflict
	}
	return profile, nil
}
