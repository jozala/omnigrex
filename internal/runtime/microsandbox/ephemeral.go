package microsandbox

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"sync"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

// Ephemeral input fingerprints live only in process memory. The journal stores
// an independent random ownership nonce, never secret bytes or their hash.
// After restart, cleanup can recover resources, but credentials/registrations
// cannot be adopted or replaced by guessing the original input contents.
type inputBinding struct {
	owner       string
	fingerprint [sha256.Size]byte
}
type inputBindings struct {
	mutex  sync.Mutex
	values map[execution.Key]inputBinding
}

func newInputBindings() *inputBindings {
	return &inputBindings{values: make(map[execution.Key]inputBinding)}
}

func inputFingerprint(request execution.Request) [sha256.Size]byte {
	content, _ := json.Marshal(request.Environment)
	return sha256.Sum256(content)
}

func (bindings *inputBindings) bind(request execution.Request) (inputBinding, error) {
	bindings.mutex.Lock()
	defer bindings.mutex.Unlock()
	fingerprint := inputFingerprint(request)
	if bound, ok := bindings.values[request.Key]; ok {
		if bound.fingerprint != fingerprint {
			return inputBinding{}, execution.ErrConflict
		}
		return bound, nil
	}
	bound := inputBinding{owner: rand.Text(), fingerprint: fingerprint}
	bindings.values[request.Key] = bound
	return bound, nil
}

func (bindings *inputBindings) forget(key execution.Key) {
	bindings.mutex.Lock()
	defer bindings.mutex.Unlock()
	delete(bindings.values, key)
}

func (bindings *inputBindings) clear() {
	bindings.mutex.Lock()
	defer bindings.mutex.Unlock()
	clear(bindings.values)
}
