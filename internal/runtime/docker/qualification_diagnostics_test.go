package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	piresources "github.com/jozala/omnigrex/agent/pi"
	"github.com/jozala/omnigrex/internal/runtime/environment"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	mobyclient "github.com/moby/moby/client"
)

type qualificationLogsFake struct {
	*preparationFailureAPI
	wire   []byte
	logErr error
	reads  int
}

func (api *qualificationLogsFake) ContainerLogs(_ context.Context, _ string, options mobyclient.ContainerLogsOptions) (mobyclient.ContainerLogsResult, error) {
	api.reads++
	if !options.ShowStderr || options.ShowStdout || options.Follow {
		panic("qualification broadened log capture")
	}
	return io.NopCloser(bytes.NewReader(api.wire)), api.logErr
}
func diagnosticFrame(body string) []byte {
	header := []byte{2, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(header[4:], uint32(len(body)))
	return append(header, body...)
}
func TestQualificationLogsAreNarrowAndCleanupClassificationSurvives(t *testing.T) {
	for _, removeFails := range []bool{false, true} {
		api := &qualificationLogsFake{preparationFailureAPI: &preparationFailureAPI{newRuntimeAPIFake()}, wire: diagnosticFrame("fixture-secret\nPi resource check failed: owned checksum runner.mjs\n")}
		cleanupErr := errors.New("cleanup unavailable")
		if removeFails {
			api.removeErr = cleanupErr
		}
		output := environment.NewQualificationOutput("amd64", "pi-rpc", piresources.CandidateContract("amd64"))
		err := runOneShotContainer(context.Background(), api, mobyclient.ContainerCreateOptions{}, "qualification", output)
		if err == nil || !strings.Contains(err.Error(), "exited with status 1") || strings.Contains(err.Error(), "fixture-secret") {
			t.Fatal(err)
		}
		if !strings.Contains(output.Check(), "owned checksum runner.mjs") || api.reads != 1 {
			t.Fatal(output.Check(), api.reads)
		}
		if errors.Is(err, errNoExecutionEffects) == removeFails || errors.Is(err, cleanupErr) != removeFails {
			t.Fatalf("cleanup evidence changed: %v", err)
		}
		api.reads = 0
		_ = runOneShotContainer(context.Background(), api, mobyclient.ContainerCreateOptions{}, "unrelated setup")
		if api.reads != 0 {
			t.Fatal("generic setup captured output")
		}
	}
}
func TestQualificationLogFramingFailsClosed(t *testing.T) {
	label := "Pi resource check failed: Node version\n"
	for _, wire := range [][]byte{
		[]byte(label),
		diagnosticFrame(strings.TrimSuffix(label, "\n")),
		append(diagnosticFrame(label), 2, 0),
		append(diagnosticFrame(label), 2, 0, 0, 0, 255, 255, 255, 255),
		append(diagnosticFrame(label), diagnosticFrame(strings.Repeat("x", 17000))...),
	} {
		api := &qualificationLogsFake{preparationFailureAPI: &preparationFailureAPI{newRuntimeAPIFake()}, wire: wire}
		output := environment.NewQualificationOutput("amd64", "pi-rpc", piresources.CandidateContract("amd64"))
		_ = runOneShotContainer(context.Background(), api, mobyclient.ContainerCreateOptions{}, "qualification", output)
		if !strings.Contains(output.Check(), "unknown stage") {
			t.Fatal(output.Check())
		}
	}
}
func TestQualificationLogRetrievalFailureDoesNotReplaceObservedExit(t *testing.T) {
	api := &qualificationLogsFake{preparationFailureAPI: &preparationFailureAPI{newRuntimeAPIFake()}, logErr: errors.New("secret source log error")}
	output := environment.NewQualificationOutput("amd64", "pi-rpc", piresources.CandidateContract("amd64"))
	err := runOneShotContainer(context.Background(), api, mobyclient.ContainerCreateOptions{}, "qualification", output)
	if err == nil || !strings.Contains(err.Error(), "exited with status 1") || strings.Contains(err.Error(), "secret") || !strings.Contains(output.Check(), "unknown stage") {
		t.Fatal(err, output.Check())
	}
}

type qualificationWaitFake struct {
	*qualificationLogsFake
	waitErr error
}

func (api *qualificationWaitFake) ContainerWait(context.Context, string, mobyclient.ContainerWaitOptions) mobyclient.ContainerWaitResult {
	failures := make(chan error, 1)
	failures <- api.waitErr
	return mobyclient.ContainerWaitResult{Error: failures}
}
func TestQualificationDoesNotCollectLogsForUnobservedExit(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, execution.ErrUnknownEffect} {
		api := &qualificationWaitFake{qualificationLogsFake: &qualificationLogsFake{preparationFailureAPI: &preparationFailureAPI{newRuntimeAPIFake()}}, waitErr: cause}
		output := environment.NewQualificationOutput("amd64", "pi-rpc", piresources.CandidateContract("amd64"))
		err := runOneShotContainer(context.Background(), api, mobyclient.ContainerCreateOptions{}, "qualification", output)
		if !errors.Is(err, cause) || api.reads != 0 || !strings.Contains(output.Check(), "unknown stage") {
			t.Fatal(err, api.reads, output.Check())
		}
	}
}
