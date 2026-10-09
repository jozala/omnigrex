// Package piresources identifies the owned resources required by Pi environments.
package piresources

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"strings"
)

//go:embed runner.mjs mcp-extension.mjs failure-extension.mjs session-state.mjs session-validation.mjs history-rpc.mjs
var resources embed.FS

func QualificationScript() string {
	var script strings.Builder
	script.WriteString(`
test "$(/usr/local/bin/node --version)" = v24.14.1
/usr/local/bin/node -e 'const f=require("fs"),c=require("crypto");if(c.createHash("sha512").update(f.readFileSync("/opt/omnigrex/pi-runtime.tgz")).digest("base64")!=="+956nfMFHr5lDUVY/2Q4k+YzojzBuCaBXFgj0eSlXVGr7QVliVddKdc1Pz6yVg1dOlJQmb67doOVrlMsIcIdaw==")process.exit(1)'
/usr/local/bin/node -e 'const f=require("fs"),c=require("crypto"),tar=require("/usr/local/lib/node_modules/npm/node_modules/tar");tar.t({file:"/opt/omnigrex/pi-runtime.tgz",sync:true,onReadEntry(e){if(e.type!=="File"||!e.path.endsWith(".js")){e.resume();return;}const h=c.createHash("sha256");e.on("data",b=>h.update(b));e.on("end",()=>{const p="/opt/omnigrex/pi/node_modules/@earendil-works/pi-coding-agent/"+e.path.slice("package/".length);if(h.digest("hex")!==c.createHash("sha256").update(f.readFileSync(p)).digest("hex"))process.exit(1);});}})'
`)
	files, _ := resources.ReadDir(".")
	for _, file := range files {
		data, _ := resources.ReadFile(file.Name())
		fmt.Fprintf(&script, "printf '%%s  %%s\\n' '%x' '/opt/omnigrex/pi/%s' | sha256sum -c -\n", sha256.Sum256(data), file.Name())
	}
	return script.String()
}
