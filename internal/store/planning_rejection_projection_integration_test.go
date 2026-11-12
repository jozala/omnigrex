//go:build integration

package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	pibundle "github.com/jozala/omnigrex/agent/pi"
	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestHistoricalPlanningRejectionBoundedThroughPinnedSession(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 1160, workflow.RolePlanner, "")
	lease, err := database.RefreshAgentTurnLease(ctx, lease, 8*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var state collaboration.State
	if err := state.StartPlanning(collaboration.OriginalRequest{Title: "Local checklist", Body: "Private original request", Source: collaboration.Source{Channel: "fixture", ID: "original"}}); err != nil {
		t.Fatal(err)
	}
	admit := func(id string) {
		t.Helper()
		r := state.Accept(id, collaboration.Command{Kind: "steer", Body: "private-human-text"}, collaboration.Admission{Active: true, Authorized: true, ParticipantID: lease.AgentParticipantID, Actor: collaboration.Actor{ID: "human"}, Source: collaboration.Source{Channel: "fixture", ID: id}, At: time.Now()})
		if r.ResponseID != id {
			t.Fatal(r)
		}
	}
	admit("Aoriginal")
	state.Prepare(lease.AgentParticipantID, "original-turn")
	if err := state.Assess("original", lease.AgentParticipantID, "original-turn", []collaboration.Assessment{{InputID: "Aoriginal", EvidenceIDs: []string{"Aoriginal"}, Disposition: "changed", Explanation: "Accept original"}}); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 900; n++ {
		questions := make([]collaboration.Question, 5)
		resolutions := make([]collaboration.Resolution, 5)
		for q := range questions {
			// Valid 128-byte IDs deliberately include JSON escaping. The outer Session
			// representation, not just the inner result, is the compatibility boundary.
			id := fmt.Sprintf("q%04d-%d-", n, q) + strings.Repeat("\"\\", 60)
			questions[q] = collaboration.Question{ID: id, Body: "Decision?"}
			resolutions[q] = collaboration.Resolution{QuestionID: id, ResponseIDs: []string{"Aoriginal"}, Explanation: "private-interpretation"}
		}
		round, err := state.Request(fmt.Sprintf("round-%d", n), lease.AgentParticipantID, "original-turn", "", questions)
		if err != nil {
			t.Fatal(err)
		}
		if err = state.Publish(round.ID, collaboration.Source{Channel: "fixture", ID: round.ID}); err != nil {
			t.Fatal(err)
		}
		if err = state.Resolve(round.ID, "original-turn", resolutions); err != nil {
			t.Fatal(err)
		}
	}
	admit("Acorrection")
	state.Prepare(lease.AgentParticipantID, lease.ID)
	if err := state.Assess("correct", lease.AgentParticipantID, lease.ID, []collaboration.Assessment{{InputID: "Acorrection", EvidenceIDs: []string{"Aoriginal", "Acorrection"}, Corrects: []string{"Aoriginal"}, Disposition: "changed", Explanation: "Explicit correction"}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(state)
	if _, err := pool.Exec(ctx, `INSERT INTO work_item_collaboration(workflow_id,state) VALUES($1,$2)`, fixture.workflowID, raw); err != nil {
		t.Fatal(err)
	}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: &replayGatewayGitHub{}, Credentials: replayGatewayCredentials{}, Publisher: &replayGatewayPublisher{}, Workflow: &replayGatewayWorkflow{}, Clarifications: database})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := database.GetAgentTurnExecutionContext(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	scope := replayGatewayScope(lease, execution, strings.Repeat("a", 40))
	scope.Role = workflow.RolePlanner
	gateway, registration := openReplayGateway(t, database, backend, scope)
	defer func() {
		if err := gateway.CloseAndDrain(ctx, registration); err != nil {
			t.Error(err)
		}
	}()
	args := map[string]any{"operation_id": "large-history", "content": collaboration.PlanningContent{Title: "Local checklist", Summary: "No retained data.", UnresolvedDecisions: []string{}, Status: "proposed", Changes: "Settled scope", RequirementsBrief: "Local checklist with no retained data.", TechnicalPlan: "Local CLI. Test reset.", InvestigatedCommit: strings.Repeat("a", 40), Evidence: []string{"README.md"}}}
	response := callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishPlanCheckpoint, args, false)
	replay := callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishPlanCheckpoint, args, false)
	text := rejectionToolText(t, response)
	replayText := rejectionToolText(t, replay)
	if !equalRejectionJSON(t, text, replayText) {
		t.Fatal("exact replay changed")
	}
	var projection struct {
		Publication string                        `json:"publication"`
		RejectionID string                        `json:"rejection_id"`
		Count       int                           `json:"decision_count"`
		Decisions   []collaboration.StaleDecision `json:"decisions"`
		Retrieval   struct {
			Collection  string `json:"collection"`
			RejectionID string `json:"rejection_id"`
			Index       int    `json:"index"`
			Offset      int    `json:"offset"`
		} `json:"retrieval"`
	}
	if err = json.Unmarshal([]byte(text), &projection); err != nil {
		t.Fatal(err)
	}
	if len(text) > 16*1024 || projection.Publication != "not_applied" || projection.Count != 4500 || len(projection.Decisions) == 0 || len(projection.Decisions) >= 4500 || projection.Retrieval.Collection != "planning_rejection" || projection.Retrieval.RejectionID != projection.RejectionID || projection.Retrieval.Index != len(projection.Decisions) || projection.Retrieval.Offset != 0 {
		t.Fatalf("invalid bounded projection: %d bytes, %d decisions, next %d", len(text), len(projection.Decisions), projection.Retrieval.Index)
	}
	var mutationID string
	var mutationCount int
	if err := pool.QueryRow(ctx, `SELECT count(*),min(id::text) FROM tool_invocations WHERE agent_turn_id=$1 AND kind='MUTATION'`, lease.ID).Scan(&mutationCount, &mutationID); err != nil || mutationCount != 1 || projection.RejectionID != "P"+mutationID {
		t.Fatal("unexpected mutation ledger", err)
	}
	if _, err := database.GetPlanningCheckpoint(ctx, fixture.workflowID, projection.RejectionID); !errors.Is(err, collaboration.ErrInvalid) {
		t.Fatal("rejection created a revision", err)
	}
	receipt, err := databases[1].ReconcilePlanningNonPublication(ctx, fixture.workflowID, lease.ID, mutationID)
	if err != nil || receipt == nil || len(receipt.Decisions) != 4500 || receipt.OperationID != projection.RejectionID {
		t.Fatal("complete immutable rejection missing", err)
	}
	receiptBefore, _ := json.Marshal(receipt)
	if _, err = database.GetCollaborationRecord(ctx, fixture.workflowID, lease.AgentParticipantID, lease.ID, "planning_history", 0, 0); err == nil {
		t.Fatal("frozen absence widened to publication evidence")
	}
	for _, scope := range []struct {
		workflow, participant, turn, id string
		index, offset                   int
	}{
		{fixture.workflowID, "other", lease.ID, projection.RejectionID, 0, 0},
		{fixture.workflowID, lease.AgentParticipantID, "00000000-0000-4000-8000-000000000000", projection.RejectionID, 0, 0},
		{"00000000-0000-4000-8000-000000000000", lease.AgentParticipantID, lease.ID, projection.RejectionID, 0, 0},
		{fixture.workflowID, lease.AgentParticipantID, lease.ID, "missing", 0, 0},
		{fixture.workflowID, lease.AgentParticipantID, lease.ID, projection.RejectionID, 4500, 0},
		{fixture.workflowID, lease.AgentParticipantID, lease.ID, projection.RejectionID, -1, 0},
		{fixture.workflowID, lease.AgentParticipantID, lease.ID, projection.RejectionID, 0, -1},
		{fixture.workflowID, lease.AgentParticipantID, lease.ID, projection.RejectionID, 0, 999999},
	} {
		if _, err := database.GetPlanningRejectionRecord(ctx, scope.workflow, scope.participant, scope.turn, scope.id, scope.index, scope.offset); !errors.Is(err, collaboration.ErrInvalid) {
			t.Fatalf("out-of-scope receipt read: %v", err)
		}
	}
	results := []string{text, replayText}
	all := append([]collaboration.StaleDecision{}, projection.Decisions...)
	for index := projection.Retrieval.Index; index < projection.Count; index++ {
		offset := 0
		var content strings.Builder
		var fingerprint string
		for {
			// Read from a separate Store: no in-memory receipt or frozen projection is
			// needed. The backend/gateway contract is also exercised at both boundaries.
			page, err := databases[1].GetPlanningRejectionRecord(ctx, fixture.workflowID, lease.AgentParticipantID, lease.ID, projection.RejectionID, index, offset)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(page)
			if index == projection.Retrieval.Index || index == projection.Count-1 {
				viaTool := callReplayGatewayTool(t, gateway, registration, mcp.ToolGetCollaborationRecord, map[string]any{"collection": "planning_rejection", "rejection_id": projection.RejectionID, "index": index, "offset": offset}, false)
				if rejectionToolText(t, viaTool) != string(encoded) {
					t.Fatal("gateway changed receipt page")
				}
			}
			results = append(results, string(encoded))
			if page.Index != index || page.Total != projection.Count || (fingerprint != "" && fingerprint != page.RecordSHA256) {
				t.Fatal("unstable page")
			}
			fingerprint = page.RecordSHA256
			content.WriteString(page.Content)
			if page.NextOffset != nil {
				offset = *page.NextOffset
				continue
			}
			if (index+1 < projection.Count && (page.NextIndex == nil || *page.NextIndex != index+1)) || (index+1 == projection.Count && page.NextIndex != nil) {
				t.Fatal("missing continuation")
			}
			break
		}
		if fmt.Sprintf("%x", sha256.Sum256([]byte(content.String()))) != fingerprint {
			t.Fatal("record fingerprint mismatch")
		}
		var decision collaboration.StaleDecision
		if err := json.Unmarshal([]byte(content.String()), &decision); err != nil {
			t.Fatal(err)
		}
		all = append(all, decision)
	}
	if !reflect.DeepEqual(all, receipt.Decisions) {
		t.Fatal("retrieval lost or changed prerequisites")
	}
	for _, result := range results {
		if strings.Contains(result, "private-human-text") || strings.Contains(result, "private-interpretation") || strings.Contains(result, "Private original request") {
			t.Fatal("private text leaked")
		}
	}
	after, err := databases[1].ReconcilePlanningNonPublication(ctx, fixture.workflowID, lease.ID, mutationID)
	afterRaw, _ := json.Marshal(after)
	if err != nil || !bytes.Equal(receiptBefore, afterRaw) {
		t.Fatal("read mutated immutable receipt", err)
	}
	if replay := callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishPlanCheckpoint, args, false); !equalRejectionJSON(t, text, rejectionToolText(t, replay)) {
		t.Fatal("retrieval changed replay")
	}
	// The former unbounded payload is an explicit failing control for the actual
	// selected bridge and durable Session loader, not only a Go byte-length check.
	oversized, _ := json.Marshal(map[string]any{"publication": "not_applied", "decisions": receipt.Decisions})
	var discovery struct {
		Result struct {
			Tools []mcp.ToolDefinition `json:"tools"`
		} `json:"result"`
	}
	listed := replayGatewayRPC(t, gateway, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`)
	if err := json.Unmarshal([]byte(listed), &discovery); err != nil {
		t.Fatal(err)
	}
	var tools []mcp.ToolDefinition
	for _, tool := range discovery.Result.Tools {
		if tool.Name == mcp.ToolPublishPlanCheckpoint || tool.Name == mcp.ToolGetCollaborationRecord {
			tools = append(tools, tool)
		}
	}
	if len(tools) != 2 {
		t.Fatal("missing production tools")
	}
	validateRejectionInPinnedSession(t, ctx, string(oversized), results, tools)
	t.Logf("4500 durable prerequisites; inline=%d, result_bytes=%d; all remaining records retrieved and retained Session reopened", len(projection.Decisions), len(text))
}

// PostgreSQL JSONB may reorder keys and add whitespace to replayed results.
// Compare the exact recorded JSON value, not its incidental text formatting.
func equalRejectionJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var first, second any
	if err := json.Unmarshal([]byte(a), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(b), &second); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(first, second)
}

func rejectionToolText(t *testing.T, response string) string {
	t.Helper()
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(response), &rpc); err != nil || len(rpc.Result.Content) != 1 {
		t.Fatal("invalid MCP result", err)
	}
	return rpc.Result.Content[0].Text
}

func validateRejectionInPinnedSession(t *testing.T, ctx context.Context, oversized string, results []string, tools []mcp.ToolDefinition) {
	t.Helper()
	bundle, err := pibundle.SelectedBundle("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]any{"oversized": oversized, "results": results, "tools": tools})
	command := exec.CommandContext(ctx, "docker", "run", "--pull=missing", "--rm", "-i", "--network", "none", "--entrypoint", "node", bundle.Image, "--input-type=module", "-e", pinnedRejectionSessionProbe)
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("pinned bridge/Session probe: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "bounded rejection and every page survive pinned bridge and Session continuation") {
		t.Fatal(string(output))
	}
}

const pinnedRejectionSessionProbe = `
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdir, mkdtemp, writeFile, rm } from 'node:fs/promises';
import { mcpExtension } from '/opt/omnigrex/pi/mcp-extension.mjs';
import { validateSessionEntries } from '/opt/omnigrex/pi/session-validation.mjs';
import { openDurableSession } from '/opt/omnigrex/pi/session-state.mjs';
let input=''; for await (const chunk of process.stdin) input+=chunk;
const {oversized,results,tools}=JSON.parse(input);
let current=oversized;
const server=createServer(async (req,res)=>{
 if(req.method!=='POST'){res.writeHead(405);res.end();return;}
 let body='';for await(const chunk of req)body+=chunk;
 const call=JSON.parse(body);
 if(call.id===undefined){res.writeHead(202);res.end();return;}
 let result;
 if(call.method==='initialize') result={protocolVersion:call.params.protocolVersion,capabilities:{tools:{}},serverInfo:{name:'rejection-fixture',version:'1'}};
 else if(call.method==='tools/list') result={tools};
 else if(call.method==='tools/call') result={content:[{type:'text',text:current}],isError:false};
 else throw Error('unexpected method');
 res.writeHead(200,{'content-type':'application/json'});res.end(JSON.stringify({jsonrpc:'2.0',id:call.id,result}));
});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
const registered=new Map();const handlers={};
await mcpExtension({mcp:{url:'http://127.0.0.1:'+server.address().port+'/mcp',headers:[]},mcpTools:tools.map(tool=>tool.name),tools:[]})({registerTool(value){registered.set(value.name,value);},on(name,fn){handlers[name]=fn;},registerCommand(){}});
const id='12345678-1234-4234-8234-123456789abc';
const root=await mkdtemp('/tmp/omnigrex-rejection-');
const directory=root+'/sessions/'+id;await mkdir(directory,{recursive:true});
const timestamp=new Date().toISOString();
const header=JSON.stringify({type:'session',version:3,id,timestamp,cwd:'/workspace'});
const file=directory+'/session.jsonl';
const identity={agentSessionId:id,runtimeSessionId:id,mustExist:true};
const entries=[];
async function entry(text,n){
 current=text;
 const tool=registered.get('omnigrex_'+(n<2?'publish_plan_checkpoint':'get_collaboration_record'));
 const result=await tool.execute('call-'+n,{},new AbortController().signal);
 assert.deepEqual(result.content,[{type:'text',text}]);assert.deepEqual(result.details,{});
 return JSON.stringify({type:'message',id:'entry-'+n,parentId:n===0?null:'entry-'+(n-1),timestamp,message:{role:'toolResult',toolCallId:'call-'+n,toolName:tool.name,isError:false,content:result.content,details:result.details,timestamp:Date.now()}});
}
try{
 const bad=await entry(oversized,0);
 assert.ok(Buffer.byteLength(bad)>1<<20);
 assert.throws(()=>validateSessionEntries([bad]),/Session entry limit exceeded/);
 await writeFile(file,header+'\n'+bad+'\n');
 await assert.rejects(()=>openDurableSession(identity,root),/Session entry limit exceeded/);
 for(let n=0;n<results.length;n++){
  const line=await entry(results[n],n);assert.ok(Buffer.byteLength(line)<1<<20);entries.push(line);
 }
 validateSessionEntries(entries);
 await writeFile(file,[header,...entries,''].join('\n'));
 assert.equal((await openDurableSession(identity,root)).disposition,'continued');
 console.log('bounded rejection and every page survive pinned bridge and Session continuation');
}finally{await handlers.session_shutdown();server.close();await rm(root,{recursive:true,force:true});}
`
