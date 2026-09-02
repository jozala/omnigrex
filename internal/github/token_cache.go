package github

import (
	"context"
	"errors"
	"sync"
	"time"
)

const InstallationTokenRefreshWindow = time.Minute
const InstallationTokenRefreshTimeout = 30 * time.Second

var (
	ErrInvalidInstallationID    = errors.New("GitHub App installation ID must be positive")
	ErrInvalidInstallationToken = errors.New("invalid GitHub App installation token")
)

type InstallationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type InstallationTokenRequester interface {
	CreateInstallationToken(context.Context, string, int64) (InstallationToken, error)
}

type InstallationTokenCache struct {
	provider  AppJWTProvider
	requester InstallationTokenRequester
	clock     Clock

	mutex   sync.Mutex
	tokens  map[int64]InstallationToken
	flights map[int64]*installationTokenFlight
}

type installationTokenFlight struct {
	done  chan struct{}
	token string
	err   error
}

func NewInstallationTokenCache(provider AppJWTProvider, requester InstallationTokenRequester, clock Clock) *InstallationTokenCache {
	if clock == nil {
		clock = systemClock{}
	}
	return &InstallationTokenCache{
		provider:  provider,
		requester: requester,
		clock:     clock,
		tokens:    make(map[int64]InstallationToken),
		flights:   make(map[int64]*installationTokenFlight),
	}
}

func (cache *InstallationTokenCache) Token(ctx context.Context, installationID int64) (string, error) {
	if installationID <= 0 {
		return "", &ConfigurationError{Cause: ErrInvalidInstallationID}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	cache.mutex.Lock()
	if token, ok := cache.tokens[installationID]; ok && cache.clock.Now().Add(InstallationTokenRefreshWindow).Before(token.ExpiresAt) {
		cache.mutex.Unlock()
		return token.Token, nil
	}
	flight := cache.flights[installationID]
	if flight == nil {
		flight = &installationTokenFlight{done: make(chan struct{})}
		cache.flights[installationID] = flight
		go cache.refresh(ctx, installationID, flight)
	}
	cache.mutex.Unlock()

	select {
	case <-flight.done:
		return flight.token, flight.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (cache *InstallationTokenCache) refresh(ctx context.Context, installationID int64, flight *installationTokenFlight) {
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), InstallationTokenRefreshTimeout)
	defer cancel()
	jwt, err := cache.provider.AppJWT(refreshCtx)
	var token InstallationToken
	if err == nil {
		token, err = cache.requester.CreateInstallationToken(refreshCtx, jwt, installationID)
	}
	if err == nil && (token.Token == "" || !cache.clock.Now().Add(InstallationTokenRefreshWindow).Before(token.ExpiresAt)) {
		err = ErrInvalidInstallationToken
	}

	cache.mutex.Lock()
	if err == nil {
		cache.tokens[installationID] = token
		flight.token = token.Token
	}
	flight.err = err
	delete(cache.flights, installationID)
	close(flight.done)
	cache.mutex.Unlock()
}
