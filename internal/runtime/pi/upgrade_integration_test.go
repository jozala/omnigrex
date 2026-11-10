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
	mode := os.Getenv("OMNIGREX_PI_UPGRADE_MODE")
	if mode != "" && mode != "compatibility" && mode != "bundle" {
		t.Fatal("unknown isolated qualification mode")
	}
	bundleQualification := mode == "bundle"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	sourceImage, targetImage, platform, settings := upgradeImages(t)
	source, err := profile.NewCandidatePiV2(sourceImage, platform)
	if err != nil {
		t.Fatal(err)
	}
	// The old deployment's approved inventory is retained with the source
	// contract. Candidate defaults cannot replace that inventory on reopening.
	sourceContract := source.Contract()
	sourceContract.GoldenResourceContract = piresources.UpgradeSourceContract(platform.Arch)
	if os.Getenv("OMNIGREX_PI_UPGRADE_SELECTED_SOURCE") == "1" {
		bundle, err := piresources.SelectedBundle(platform.OS, platform.Arch)
		if err != nil {
			t.Fatal(err)
		}
		sourceContract.GoldenResourceContract = bundle.Resources
	}

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
	target, err := profile.NewCandidatePiV2(targetImage, platform)
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("OMNIGREX_PI_BUNDLE_CANDIDATE_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		proposed, err := piresources.ParseCandidate(data)
		if err != nil {
			t.Fatal(err)
		}
		if proposed.Image != targetImage || proposed.Platform != platform.OS+"/"+platform.Arch {
			t.Fatal("candidate differs from tested image/platform")
		}
		c := target.Contract()
		c.GoldenResourceContract = proposed.Resources
		target, err = profile.New(c)
		if err != nil {
			t.Fatal(err)
		}
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
	metadata := qualificationStateSource{ref: ref}
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
	retainedSessionID := sessionID
	launch := func(storage string, p profile.Profile, version string, epoch int64, mustExist bool) (agent.Connection, func(), *diagnosticOutput, error) {
		t.Helper()
		key := execution.Key{AssignmentID: participant, AgentSessionID: sessionID, AgentTurnID: uuid.NewString(), ExecutionEpoch: epoch, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2"}
		request, err := (pi.TurnAdapter{}).BuildProcess(p, key, "", 512<<20, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Command = []string{"/usr/local/bin/node", "--input-type=module", "--eval", qualification.UpgradeScript}
		request.Environment = append(request.Environment, "OMNIGREX_PI_EXPECTED_VERSION="+version)
		if bundleQualification && storage == "candidate" {
			// Regression fixture: this R2 accepts independent Sessions but
			// deliberately refuses the original R1 Session, even on reopening.
			request.Environment = append(request.Environment, "OMNIGREX_PI_REJECT_SESSION="+retainedSessionID)
		}
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
			return nil, cleanup, stderr, err
		}
		t.Cleanup(func() { _ = c.Close() })
		return c, func() { _ = c.Close(); cleanup() }, stderr, nil
	}
	start := func(storage string, p profile.Profile, version string, epoch int64, mustExist bool) (agent.Connection, func()) {
		t.Helper()
		c, remove, _, err := launch(storage, p, version, epoch, mustExist)
		if err != nil {
			remove()
			t.Fatal(err)
		}
		return c, remove
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
	restoreNames := []string{"restore"}
	if !bundleQualification {
		restoreNames = append(restoreNames, "candidate")
	}
	for _, name := range restoreNames {
		copied := states(name)
		for range 2 {
			if err := copied.Restore(ctx, ref, bytes.NewReader(archive.Bytes())); err != nil {
				t.Fatal(err)
			}
		}
		for _, mutate := range []func(*execution.StateRef){func(r *execution.StateRef) { r.ParticipantID = uuid.NewString(); r.ID = r.ParticipantID }, func(r *execution.StateRef) { r.Image = foreignUpgradeImage(ref.Image) }, func(r *execution.StateRef) { r.Backend.Family = "other" }, func(r *execution.StateRef) { r.Backend.Namespace += "-foreign" }, func(r *execution.StateRef) { r.Format = "pi-session/v4" }} {
			wrong := ref
			mutate(&wrong)
			if err := copied.Restore(ctx, wrong, bytes.NewReader(archive.Bytes())); !errors.Is(err, execution.ErrBindingMismatch) {
				t.Fatalf("foreign archive accepted: %v", err)
			}
		}
	}
	type continuationCase struct {
		name    string
		p       profile.Profile
		version string
	}
	continuations := []continuationCase{{"restore", source, sourceVersion}}
	if !bundleQualification {
		continuations = append(continuations, continuationCase{"candidate", target, targetVersion})
	}
	for index, tc := range continuations {
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
	if bundleQualification || os.Getenv("OMNIGREX_PI_UPGRADE_SELECTED_SOURCE") == "1" {
		// An orchestrator upgrade reopens the original image/inventory and Session,
		// not the candidate used by the optional copied-state compatibility probe.
		encoded, _ := json.Marshal(source.Contract())
		restored, err := profile.DecodeContract(encoded, source.Binding())
		if err != nil {
			t.Fatal(err)
		}
		c, remove := start("source", restored, sourceVersion, 4, true)
		if ready, err := c.Prepare(ctx); err != nil || ready.Disposition != "continued" || ready.RuntimeSessionID != sessionID {
			t.Fatalf("retained R1 continuation: %+v %v", ready, err)
		}
		if out, err := c.Prompt(ctx, "upgrade-continue", nil); err != nil || out.Status != agent.Completed {
			t.Fatalf("retained R1 Turn: %+v %v", out, err)
		}
		remove()
		if restored.Binding() != source.Binding() {
			t.Fatal("retained binding changed")
		}
		if bundleQualification {
			// Prove that refusal to read R1 state cannot gate fresh R2
			// qualification. No R1 archive is attached to candidate storage.
			// The native backend requires an existing mount directory even for
			// this rejected probe. Keep it empty so rejection reaches the
			// controlled runtime rather than failing backend state validation.
			if err := states("candidate").Ensure(ctx, ref); err != nil {
				t.Fatal(err)
			}
			rejected, remove, diagnostics, err := launch("candidate", target, targetVersion, 5, true)
			if err == nil {
				_, err = rejected.Prepare(ctx)
			}
			remove()
			if err == nil || !strings.Contains(diagnostics.String(), "fixture refuses previous-image Session") {
				t.Fatalf("incompatible R2 probe did not reject the R1 Session: %v: %s", err, diagnostics.String())
			}
		}
		// A newly created Reviewer may use R2 independently of the old Developer.
		// This is isolated candidate qualification, not production admission of R2.
		participant, sessionID = uuid.NewString(), uuid.NewString()
		ref = execution.StateRef{ID: participant, ParticipantID: participant, Backend: ref.Backend, Format: profile.PiStateContractVersion, RuntimeProfileSHA256: target.ContentSHA256(), Image: targetImage}
		metadata = qualificationStateSource{ref: ref}
		if err := states("candidate").Ensure(ctx, ref); err != nil {
			t.Fatal(err)
		}
		fresh, remove := start("candidate", target, targetVersion, 5, false)
		if ready, err := fresh.Prepare(ctx); err != nil || ready.Disposition != "created" || ready.RuntimeSessionID != sessionID {
			t.Fatalf("fresh R2 Session: %+v %v", ready, err)
		}
		if out, err := fresh.Prompt(ctx, "upgrade-source-context", nil); err != nil || out.Status != agent.Completed {
			t.Fatalf("fresh R2 Turn: %+v %v", out, err)
		}
		remove()
		if bundleQualification {
			// Inability to consume the old image's Session does not imply
			// inability to replay and continue R2's own independent Session.
			continued, remove := start("candidate", target, targetVersion, 6, true)
			if ready, err := continued.Prepare(ctx); err != nil || ready.Disposition != "continued" || ready.RuntimeSessionID != sessionID {
				t.Fatalf("independent R2 continuation: %+v %v", ready, err)
			}
			sink := &historySink{}
			if cursor, err := continued.Replay(ctx, agent.HistoryCursor{}, sink); err != nil || cursor.RuntimeSessionID != sessionID || len(sink.events) == 0 {
				t.Fatalf("independent R2 replay: %+v %v", cursor, err)
			}
			if out, err := continued.Prompt(ctx, "upgrade-continue", nil); err != nil || out.Status != agent.Completed {
				t.Fatalf("independent R2 further Turn: %+v %v", out, err)
			}
			remove()
		}
	}
	if bundleQualification {
		// Independent retained/fresh qualification is not Session migration.
		// Never manufacture a source-to-target compatibility result.
		t.Log("PASS retained R1 and independent fresh R2 qualification despite incompatible prior Session; no migration artifact emitted")
		return
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

func foreignUpgradeImage(source string) string {
	image := "registry.example/foreign-upgrade@sha256:" + strings.Repeat("a", 64)
	if image == source {
		return "registry.example/foreign-upgrade@sha256:" + strings.Repeat("b", 64)
	}
	return image
}

func TestSameImagePIDUpgradeArchiveKeepsForeignImageProbeNegative(t *testing.T) {
	for _, image := range []string{
		"registry.example/pi@sha256:" + strings.Repeat("a", 64),
		"registry.example/foreign-upgrade@sha256:" + strings.Repeat("a", 64),
	} {
		current, err := profile.NewCandidatePiV2(image, profile.Platform{OS: "linux", Arch: "amd64"})
		if err != nil {
			t.Fatal(err)
		}
		contract := current.Contract()
		contract.PIDsLimit = 128
		source, err := profile.New(contract)
		if err != nil || source.Binding() == current.Binding() {
			t.Fatalf("same-image source/target budget contracts = %v", err)
		}
		participant := uuid.NewString()
		ref := execution.StateRef{ID: participant, ParticipantID: participant,
			Backend: execution.BackendBinding{Family: "docker", Version: 1, Namespace: "upgrade-isolated"},
			Format:  profile.PiStateContractVersion, RuntimeProfileSHA256: source.ContentSHA256(), Image: image}
		metadata := qualificationStateSource{ref: ref}
		original, err := dirstate.New(t.TempDir(), ref.Backend, metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err := original.Ensure(t.Context(), ref); err != nil {
			t.Fatal(err)
		}
		var archive bytes.Buffer
		if err := original.Export(t.Context(), ref, &archive); err != nil {
			t.Fatal(err)
		}
		copied, err := dirstate.New(t.TempDir(), ref.Backend, metadata)
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := copied.Restore(t.Context(), ref, bytes.NewReader(archive.Bytes())); err != nil {
				t.Fatal(err)
			}
		}
		wrong := ref
		wrong.Image = foreignUpgradeImage(ref.Image)
		if wrong.Image == image || !profile.IsExactRegistryImage(wrong.Image) {
			t.Fatal("foreign-image probe did not change a valid exact image")
		}
		if err := copied.Restore(t.Context(), wrong, bytes.NewReader(archive.Bytes())); !errors.Is(err, execution.ErrBindingMismatch) {
			t.Fatalf("same-image upgrade accepted foreign archive: %v", err)
		}
	}
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

type qualificationStateSource struct{ ref execution.StateRef }

func (s qualificationStateSource) StateMetadata(_ context.Context, ref execution.StateRef) (json.RawMessage, bool, error) {
	if ref != s.ref {
		return nil, false, execution.ErrBindingMismatch
	}
	data, _ := json.Marshal(struct {
		Version int    `json:"version"`
		Subpath string `json:"subpath"`
	}{1, "assignment-" + ref.ID + "/runtime-state"})
	return data, false, nil
}
