package config

import (
	"fmt"
	"math"
	"time"
)

// HealthcheckResponseAllowance lets the client receive the readiness handler's
// response after the shared dependency-check budget has expired.
const HealthcheckResponseAllowance = 5 * time.Second

// LoadReadinessTimeout is shared by startup and the standalone healthcheck,
// which must not require deployment credentials merely to probe HTTP health.
func LoadReadinessTimeout(value string) (time.Duration, error) {
	if value == "" {
		return defaultReadinessTimeout, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("OMNIGREX_READINESS_TIMEOUT must be a valid duration")
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("OMNIGREX_READINESS_TIMEOUT must be positive")
	}
	if timeout > time.Duration(math.MaxInt64)-HealthcheckResponseAllowance {
		return 0, fmt.Errorf("OMNIGREX_READINESS_TIMEOUT must leave room for the healthcheck response allowance")
	}
	return timeout, nil
}
