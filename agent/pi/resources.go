// Package piresources identifies the owned resources required by Pi environments.
package piresources

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"strings"
)

//go:embed runner.mjs mcp-extension.mjs failure-extension.mjs session-state.mjs session-validation.mjs history-rpc.mjs package.json package-lock.json package-integrity.mjs qualify-packages.mjs
var resources embed.FS

func QualificationScript() string {
	var script strings.Builder
	script.WriteString(`
for directory in /opt /opt/omnigrex /opt/omnigrex/pi; do
  test -d "$directory" && test ! -L "$directory"
done
test "$(/usr/local/bin/node --version)" = v24.14.1
`)
	files, _ := resources.ReadDir(".")
	for _, file := range files {
		data, _ := resources.ReadFile(file.Name())
		fmt.Fprintf(&script, "test -f '/opt/omnigrex/pi/%s' && test ! -L '/opt/omnigrex/pi/%s'\n", file.Name(), file.Name())
		fmt.Fprintf(&script, "printf '%%s  %%s\\n' '%x' '/opt/omnigrex/pi/%s' | sha256sum -c -\n", sha256.Sum256(data), file.Name())
	}
	script.WriteString("/usr/bin/env -i PATH=/usr/local/bin:/usr/bin:/bin /usr/local/bin/node /opt/omnigrex/pi/qualify-packages.mjs\n")
	return script.String()
}
