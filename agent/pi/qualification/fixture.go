//go:build integration

// Package qualification supplies fixture code to isolated integration processes.
package qualification

import _ "embed"

//go:embed upgrade.mjs
var UpgradeScript string
