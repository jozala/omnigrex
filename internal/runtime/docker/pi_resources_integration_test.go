//go:build integration

package docker

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/environment"
)

func TestPiRepositoryEnvironmentRejectsUnqualifiedResolution(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "image", "inspect", "omnigrex/pi:1.4.0", "--format", "{{.Id}} {{.Architecture}}").Output()
	if err != nil {
		t.Fatal("build the Pi environment fixture before resource qualification")
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 {
		t.Fatal("incomplete Pi fixture identity")
	}
	script, err := environment.QualificationScript(fields[1], "pi-rpc")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, mutation string
		reject         bool
	}{
		{"unchanged", "", false},
		{"SDK export redirection", `const p=base+'/package.json',m=JSON.parse(f.readFileSync(p));m.exports={'.':'./unqualified.mjs'};f.writeFileSync(p,JSON.stringify(m));f.writeFileSync(base+'/unqualified.mjs','export const unqualified = true;');`, true},
		{"added SDK module", `f.writeFileSync(base+'/unqualified.mjs','export const unqualified = true;');`, true},
		{"dependency export redirection", `const d='/opt/omnigrex/pi/node_modules/@earendil-works/pi-ai',p=d+'/package.json',m=JSON.parse(f.readFileSync(p));m.exports={'.':'./unqualified.mjs'};f.writeFileSync(p,JSON.stringify(m));f.writeFileSync(d+'/unqualified.mjs','export const unqualified = true;');`, true},
		{"required dependency missing", `f.rmSync('/opt/omnigrex/pi/node_modules/@earendil-works/pi-ai',{recursive:true});`, true},
		{"dependency shadow", `const d=base+'/node_modules/@earendil-works/pi-ai';f.mkdirSync(d,{recursive:true});f.writeFileSync(d+'/package.json','{"name":"@earendil-works/pi-ai","main":"index.js"}');f.writeFileSync(d+'/index.js','module.exports = {};');`, true},
		{"byte-identical runner alias", `const p='/opt/omnigrex/pi/runner.mjs',other='/opt/runner-copy.mjs';f.copyFileSync(p,other);f.unlinkSync(p);f.symlinkSync(other,p);`, true},
		{"resource directory alias", `f.renameSync('/opt/omnigrex/pi','/opt/pi-copy');f.symlinkSync('/opt/pi-copy','/opt/omnigrex/pi');`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := script
			if tc.mutation != "" {
				program := `const f=require('node:fs'),base='/opt/omnigrex/pi/node_modules/@earendil-works/pi-coding-agent';` + tc.mutation
				command = fmt.Sprintf("/usr/local/bin/node -e %s\n%s", shellQuoteForTest(program), script)
			}
			output, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--user", "0:0", "--entrypoint", "/bin/sh", fields[0], "-ec", command).CombinedOutput()
			if (err != nil) != tc.reject {
				t.Fatalf("qualification rejection=%t, want %t: %v\n%s", err != nil, tc.reject, err, output)
			}
		})
	}
}

func shellQuoteForTest(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
