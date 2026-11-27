package retention

import (
	"context"
	"time"
)

type StorageCollectionGuard interface {
	WithParticipantStorageCollection(context.Context, string, func(context.Context) error) error
}
type storageCleaner struct {
	guard   StorageCollectionGuard
	cleaner Cleaner
}

func NewStorageCleaner(guard StorageCollectionGuard, cleaner Cleaner) Cleaner {
	return storageCleaner{guard, cleaner}
}
func (cleaner storageCleaner) CollectParticipant(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return cleaner.guard.WithParticipantStorageCollection(ctx, id, func(ctx context.Context) error { return cleaner.cleaner.CollectParticipant(ctx, id) })
}
