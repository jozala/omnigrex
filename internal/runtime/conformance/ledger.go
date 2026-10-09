//go:build integration

package conformance

import (
	"context"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"sync"
)

type Ledger struct {
	mutex   sync.Mutex
	records map[execution.Key]execution.Reservation
}

func NewLedger() *Ledger { return &Ledger{records: map[execution.Key]execution.Reservation{}} }
func (ledger *Ledger) ReserveExecution(_ context.Context, key execution.Key, digest string) (execution.Reservation, bool, error) {
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	if record, ok := ledger.records[key]; ok {
		if record.Revoked || record.Digest != digest {
			return record, false, execution.ErrConflict
		}
		return record, false, nil
	}
	record := execution.Reservation{Key: key, Digest: digest, Phase: execution.Reserved}
	ledger.records[key] = record
	return record, true, nil
}
func (ledger *Ledger) BeginExecutionPhase(_ context.Context, key execution.Key, phase execution.Phase) error {
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	record := ledger.records[key]
	if record.Revoked {
		return execution.ErrConflict
	}
	record.Phase = phase
	ledger.records[key] = record
	return nil
}
func (ledger *Ledger) BindExecution(_ context.Context, key execution.Key, ref execution.Ref) error {
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	record := ledger.records[key]
	if record.Resource != "" && record.Resource != ref {
		return execution.ErrConflict
	}
	record.Resource = ref
	record.CreationSettled = true
	ledger.records[key] = record
	return nil
}
func (ledger *Ledger) RevokeExecution(_ context.Context, key execution.Key) (execution.Reservation, bool, error) {
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	record, found := ledger.records[key]
	record.Key = key
	record.Revoked = true
	ledger.records[key] = record
	return record, found, nil
}
func (ledger *Ledger) RecordNoExecution(_ context.Context, key execution.Key) error {
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	record := ledger.records[key]
	record.CreationSettled = true
	ledger.records[key] = record
	return nil
}
func (*Ledger) BindSetupResource(context.Context, execution.Key, execution.Ref) error {
	return execution.ErrInvalidSpec
}
func (*Ledger) RecordSetupStopped(context.Context, execution.Key, execution.Ref) error {
	return execution.ErrInvalidSpec
}
