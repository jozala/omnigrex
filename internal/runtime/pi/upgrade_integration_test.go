//go:build integration

package pi_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	piresources "github.com/jozala/omnigrex/agent/pi"
	"github.com/jozala/omnigrex/agent/pi/qualification"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/runtime/conformance"
	"github.com/jozala/omnigrex/internal/runtime/dirstate"
	"github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/pi"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

// Candidate qualification is deliberately off-ledger: none of these factories
// can acquire a production Turn or attach production storage.
func TestPiControlledStateUpgrade(t *testing.T) {
	if os.Getenv("OMNIGREX_PI_UPGRADE_INTEGRATION") != "1" {
		t.Skip("explicit isolated Pi upgrade gate required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	sourceImage, targetImage, platform, settings := upgradeImages(t)
	source, err := profile.NewPiV2(sourceImage, platform)
	if err != nil {
		t.Fatal(err)
	}
	// The old deployment's approved inventory is retained with the source
	// contract. Candidate defaults cannot replace that inventory on reopening.
	sourceContract := source.Contract()
	sourceContract.GoldenResourceContract = piresources.UpgradeSourceContract(platform.Arch)
	if path := os.Getenv("OMNIGREX_PI_UPGRADE_SOURCE_CONTRACT_FILE"); path != "" {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(bytes.NewReader(content))
		decoder.DisallowUnknownFields()
		if len(content) > 64<<10 || decoder.Decode(&sourceContract) != nil || decoder.Decode(new(any)) != io.EOF || sourceContract.Image != sourceImage || sourceContract.Platform != platform || len(sourceContract.GoldenResourceContract) == 0 {
			t.Fatal("source definition does not match the exact qualified source image/platform")
		}
	}
	source, err = profile.New(sourceContract)
	if err != nil {
		t.Fatal(err)
	}
	target, err := profile.NewPiV2(targetImage, platform)
	if err != nil {
		t.Fatal(err)
	}
	participant, sessionID := uuid.NewString(), uuid.NewString()
	ref := execution.StateRef{ID: participant, ParticipantID: participant, Backend: execution.BackendBinding{Family: settings.ExecutionBackend, Version: 1, Namespace: "upgrade-isolated"}, Format: profile.PiStateContractVersion, RuntimeProfileSHA256: source.ContentSHA256(), Image: sourceImage}
	var imported []byte
	if path := os.Getenv("OMNIGREX_PI_UPGRADE_RESTORE_ARCHIVE"); path != "" {
		imported, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		ref, sessionID = upgradeArchiveIdentity(t, imported)
		participant = ref.ParticipantID
		if ref.Validate() != nil || ref.Backend.Family != settings.ExecutionBackend || ref.Backend.Version != 1 || ref.Format != profile.PiStateContractVersion || ref.Image != sourceImage || ref.RuntimeProfileSHA256 != source.ContentSHA256() {
			t.Fatalf("foreign host archive does not match source/runtime/backend contract: archived=%+v expected=%+v backend=%s", ref, source.Binding(), settings.ExecutionBackend)
		}
	}
	metadata := upgradeStateSource{ref: ref}
	root := os.Getenv("OMNIGREX_PI_UPGRADE_ROOT")
	states := func(name string) *dirstate.RetainedState {
		t.Helper()
		s, err := dirstate.New(filepath.Join(root, name), ref.Backend, metadata)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	original := states("source")
	if imported != nil {
		if err := original.Restore(ctx, ref, bytes.NewReader(imported)); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := original.Ensure(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	start := func(storage string, p profile.Profile, version string, epoch int64, mustExist bool) (agent.Connection, func()) {
		t.Helper()
		key := execution.Key{AssignmentID: participant, AgentSessionID: sessionID, AgentTurnID: uuid.NewString(), ExecutionEpoch: epoch, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2"}
		request, err := (pi.TurnAdapter{}).BuildProcess(p, key, "", 512<<20, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Command = []string{"/usr/local/bin/node", "--input-type=module", "--eval", qualification.UpgradeScript}
		request.Environment = append(request.Environment, "OMNIGREX_PI_EXPECTED_VERSION="+version)
		// Full production golden-resource qualification uses the bound inventory.
		// Tool/cache and workspace attachments are isolated copies as well.
		request.GoldenResources = true
		request.Requirements = []execution.Requirement{execution.ProcessIsolation, execution.ToolInstallation}
		for _, requirement := range p.Contract().RepositoryEnvironment.Requirements {
			request.Requirements = append(request.Requirements, execution.Requirement(requirement))
		}
		var factory execution.Factory
		if settings.ExecutionBackend == "docker" {
			request.Image = os.Getenv("OMNIGREX_PI_UPGRADE_SOURCE_LOCAL")
			if p.Binding() == target.Binding() {
				request.Image = os.Getenv("OMNIGREX_PI_UPGRADE_TARGET_LOCAL")
			}
			volume := os.Getenv("OMNIGREX_PI_UPGRADE_" + strings.ToUpper(storage) + "_VOLUME")
			factory = docker.ExecutionFactory{Network: "none", RuntimeStateVolume: volume, WorkspaceVolume: volume, MiseVolume: volume}
		} else {
			factory = upgradeNativeFactory(t, settings, filepath.Join(root, storage), participant)
		}
		engine, err := factory.New(request, conformance.NewLedger())
		if err != nil {
			t.Fatal(err)
		}
		stderr := &diagnosticOutput{}
		process, err := engine.Create(ctx, request, stderr)
		if err != nil {
			_ = engine.Close()
			t.Fatal(err)
		}
		cleaned := false
		cleanup := func() {
			if cleaned {
				return
			}
			cleaned = true
			stop, done := context.WithTimeout(context.Background(), 20*time.Second)
			defer done()
			_ = process.Transport().Close()
			if err := process.Stop(stop, 5*time.Second); err != nil {
				t.Error(err)
				return
			}
			if err := process.Remove(stop); err != nil {
				t.Error(err)
			}
			if err := engine.Close(); err != nil {
				t.Error(err)
			}
			if t.Failed() {
				t.Log(stderr.String())
			}
		}
		t.Cleanup(cleanup)
		if err := process.Start(ctx); err != nil {
			t.Fatal(err)
		}
		c, err := pi.Start(ctx, process.Transport(), pi.Configuration{Selection: agent.ModelSelection{Model: "upgrade/controlled"}, Tools: []string{}, Instructions: "Verify retained upgrade context.", MCP: agent.MCPEndpoint{Name: "omnigrex", URL: "http://127.0.0.1:8845/mcp", Headers: []agent.Header{{Name: "Authorization", Value: "Bearer upgrade-only-mcp"}}}, MCPTools: []string{"get_issue"}, Auth: json.RawMessage(`{"upgrade":{"type":"api_key","key":"upgrade-only-provider"}}`), Session: &agent.SessionIdentity{AgentSessionID: sessionID, RuntimeSessionID: sessionID, MustExist: mustExist}}, pi.Options{RequestTimeout: 20 * time.Second, EventContext: agentevent.Context{AssignmentID: participant, AgentSessionID: sessionID, TurnID: key.AgentTurnID, ExecutionEpoch: uint64(epoch)}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c, func() { _ = c.Close(); cleanup() }
	}
	sourceVersion, targetVersion := os.Getenv("OMNIGREX_PI_UPGRADE_SOURCE_VERSION"), os.Getenv("OMNIGREX_PI_UPGRADE_TARGET_VERSION")
	first, removeFirst := start("source", source, sourceVersion, 1, imported != nil)
	if ready, err := first.Prepare(ctx); err != nil || ready.RuntimeSessionID != sessionID {
		t.Fatalf("source Session: %+v %v", ready, err)
	}
	if imported == nil {
		if outcome, err := first.Prompt(ctx, "upgrade-source-context", nil); err != nil || outcome.Status != agent.Completed {
			t.Fatalf("source prompt: %+v %v", outcome, err)
		}
	}
	sourceHistory := &historySink{}
	if _, err := first.Replay(ctx, agent.HistoryCursor{}, sourceHistory); err != nil {
		t.Fatal(err)
	}
	removeFirst()
	var archive bytes.Buffer
	if err := original.Export(ctx, ref, &archive); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(archive.Bytes(), []byte("upgrade-only-provider")) || bytes.Contains(archive.Bytes(), []byte("upgrade-only-mcp")) {
		t.Fatal("credentials retained in archive")
	}
	if err := os.WriteFile("/tmp/pi-upgrade-source-state.tar", archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"candidate", "restore"} {
		copied := states(name)
		for range 2 {
			if err := copied.Restore(ctx, ref, bytes.NewReader(archive.Bytes())); err != nil {
				t.Fatal(err)
			}
		}
		for _, mutate := range []func(*execution.StateRef){func(r *execution.StateRef) { r.ParticipantID = uuid.NewString(); r.ID = r.ParticipantID }, func(r *execution.StateRef) { r.Image = targetImage }, func(r *execution.StateRef) { r.Backend.Family = "other" }, func(r *execution.StateRef) { r.Backend.Namespace = "other" }, func(r *execution.StateRef) { r.Format = "pi-session/v4" }} {
			wrong := ref
			mutate(&wrong)
			if err := copied.Restore(ctx, wrong, bytes.NewReader(archive.Bytes())); !errors.Is(err, execution.ErrBindingMismatch) {
				t.Fatalf("foreign archive accepted: %v", err)
			}
		}
	}
	for index, tc := range []struct {
		name    string
		p       profile.Profile
		version string
	}{{"candidate", target, targetVersion}, {"restore", source, sourceVersion}} {
		c, remove := start(tc.name, tc.p, tc.version, int64(index+2), true)
		if ready, err := c.Prepare(ctx); err != nil || ready.Disposition != "continued" || ready.RuntimeSessionID != sessionID {
			t.Fatalf("%s continuation: %+v %v", tc.name, ready, err)
		}
		sink := &historySink{}
		cursor, err := c.Replay(ctx, agent.HistoryCursor{}, sink)
		if err != nil || cursor.RuntimeSessionID != sessionID || len(sink.events) < len(sourceHistory.events) {
			t.Fatalf("%s replay: %+v %v", tc.name, cursor, err)
		}
		for i, event := range sink.events[:len(sourceHistory.events)] {
			if event.Kind != sourceHistory.events[i].Kind || event.Metadata.MessageID != sourceHistory.events[i].Metadata.MessageID || !bytes.Equal(event.Metadata.Runtime, sourceHistory.events[i].Metadata.Runtime) || event.AgentSessionID != sessionID || event.AssignmentID != participant {
				t.Fatal("replay content or ownership changed")
			}
		}
		// Checked model selection may append configuration observations when
		// reopening. It must never fabricate a new conversation or model Turn.
		for _, event := range sink.events[len(sourceHistory.events):] {
			if event.Kind != "history_model_change" && event.Kind != "history_thinking_level_change" && event.Kind != "history_usage" {
				t.Fatalf("reopening appended model activity: %s", event.Kind)
			}
		}
		count := len(sink.events)
		if got, err := c.Replay(ctx, cursor, sink); err != nil || got != cursor || len(sink.events) != count {
			t.Fatal("replay cursor repeated history")
		}
		if outcome, err := c.Prompt(ctx, "upgrade-continue", nil); err != nil || outcome.Status != agent.Completed {
			t.Fatalf("%s continued prompt: %+v %v", tc.name, outcome, err)
		}
		remove()
	}
	var unchanged bytes.Buffer
	if err := original.Export(ctx, ref, &unchanged); err != nil || !bytes.Equal(archive.Bytes(), unchanged.Bytes()) {
		t.Fatalf("qualification modified source state: %v", err)
	}
	result := profile.CompatibilityResult{Source: source.Binding(), Target: target.Binding(), Platform: platform, StateContractVersion: profile.PiStateContractVersion, WorkspacePath: "/workspace", QualificationSuite: profile.PiCompatibilityQualificationSuite, QualificationVersion: "1", QualifiedAt: time.Now().UTC().Truncate(time.Second).Format(time.RFC3339), Outcome: "success", BackendFamily: settings.ExecutionBackend, BackendContractVersion: 1}
	file := profile.CompatibilityResultsFile{SchemaVersion: profile.PiCompatibilityResultsSchemaVersion, Results: []profile.CompatibilityResult{result}}
	if err := file.ValidateDeployment(settings.ExecutionBackend, 1, target); err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("OMNIGREX_PI_UPGRADE_ARTIFACT"); path != "" {
		output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(output).Encode(file); err != nil {
			_ = output.Close()
			_ = os.Remove(path)
			t.Fatal(err)
		}
		if err := errors.Join(output.Sync(), output.Close()); err != nil {
			_ = os.Remove(path)
			t.Fatal(err)
		}
	}
	t.Logf("PASS: Pi %s to %s, provider-free exact replay, isolated compatible-host restore and unchanged source archive", sourceVersion, targetVersion)
}

// This fixture-only reader discovers the original identity for a two-host
// drill. The production Restore still validates the entire archive before use.
func upgradeArchiveIdentity(t *testing.T, archive []byte) (execution.StateRef, string) {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(archive))
	var ref execution.StateRef
	sessionID := ""
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == ".omnigrex-state-manifest.json" {
			var manifest struct {
				State execution.StateRef `json:"state"`
			}
			if err := json.NewDecoder(reader).Decode(&manifest); err != nil {
				t.Fatal(err)
			}
			ref = manifest.State
		}
		parts := strings.Split(header.Name, "/")
		if len(parts) == 3 && parts[0] == "sessions" && parts[2] == "session.jsonl" {
			if sessionID != "" {
				t.Fatal("upgrade fixture archive has multiple Sessions")
			}
			sessionID = parts[1]
		}
	}
	if _, err := uuid.Parse(sessionID); err != nil {
		t.Fatal("missing exact Session in host archive")
	}
	return ref, sessionID
}

type upgradeStateSource struct{ ref execution.StateRef }

func (s upgradeStateSource) StateMetadata(_ context.Context, ref execution.StateRef) (json.RawMessage, bool, error) {
	if ref != s.ref {
		return nil, false, execution.ErrBindingMismatch
	}
	data, _ := json.Marshal(struct {
		Version int    `json:"version"`
		Subpath string `json:"subpath"`
	}{1, "assignment-" + ref.ID + "/runtime-state"})
	return data, false, nil
}
