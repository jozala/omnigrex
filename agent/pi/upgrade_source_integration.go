//go:build integration

package piresources

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
)

//go:embed upgrade-source/package.json upgrade-source/package-lock.json
var upgradeSource embed.FS

// UpgradeSourceContract captures the previous pinned package bundle while
// retaining the same owned runner contract in this preparatory fixture.
func UpgradeSourceContract(architecture string) json.RawMessage {
	var contract ResourceContract
	_ = json.Unmarshal(CurrentContract(architecture), &contract)
	for i, file := range contract.Files {
		if file.Name == "package.json" || file.Name == "package-lock.json" {
			data, _ := upgradeSource.ReadFile("upgrade-source/" + file.Name)
			digest := sha256.Sum256(data)
			contract.Files[i].SHA256 = hex.EncodeToString(digest[:])
		}
	}
	content, _ := json.Marshal(contract)
	return content
}
