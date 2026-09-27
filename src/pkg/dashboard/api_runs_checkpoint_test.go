package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/timeline"
)

func checkpointTestServer(t *testing.T, title string, gen uint64) (*Server, *beads.Store, string, string) {
	t.Helper()
	s, deps := runsTestServer(t)
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	epic, err := store.Create("mobile checkpoint plan", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	runKey := "myorg/repo1#8618"
	if err := store.Update(epic.ID, func(b *beads.Bead) {
		b.Metadata[planning.MetaPlanStatus] = planning.PlanStatusDraft
		b.Metadata[planning.MetaIssueRepo] = "myorg/repo1"
		b.Metadata[planning.MetaIssueNumber] = "8618"
		b.Metadata[planning.MetaRunKey] = runKey
	}); err != nil {
		t.Fatalf("update epic: %v", err)
	}
	deps.BeadStores = map[string]*beads.Store{"architect": store}
	if title == "" {
		title = "Approve mobile checkpoint contract"
	}
	if err := s.contributeHub.recordLeaseForKeyStage("alice", "task-8618", "myorg/repo1", 8618, runKey, "contributor", StagePlan, gen, time.Now()); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	s.contributeHub.leaseMu.Lock()
	for _, l := range s.contributeHub.leases {
		if l != nil && l.taskID == "task-8618" {
			l.title = title
		}
	}
	s.contributeHub.leaseMu.Unlock()
	return s, store, epic.ID, runKey
}

func doPostNoOwner(s *Server, path string, body interface{}) *httptest.ResponseRecorder {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, &b)
	req.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(rec, req)
	return rec
}

func decodeCheckpointPayload(t *testing.T, rec *httptest.ResponseRecorder) RunCheckpointPayload {
	t.Helper()
	var payload RunCheckpointPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode checkpoint payload: %v body=%s", err, rec.Body.String())
	}
	return payload
}

func TestRunCheckpointPayloadCapsSummary(t *testing.T) {
	s, _, _, runKey := checkpointTestServer(t, strings.Repeat("å", RunCheckpointSummaryMaxBytes), 7)

	rec := doOwnerGet(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}

	payload := decodeCheckpointPayload(t, rec)
	if len(payload.Summary) > RunCheckpointSummaryMaxBytes {
		t.Fatalf("summary len = %d, want <= %d", len(payload.Summary), RunCheckpointSummaryMaxBytes)
	}
	if !utf8.ValidString(payload.Summary) {
		t.Fatalf("summary is not valid UTF-8")
	}
	if payload.SummaryMaxBytes != RunCheckpointSummaryMaxBytes {
		t.Fatalf("summary max = %d, want %d", payload.SummaryMaxBytes, RunCheckpointSummaryMaxBytes)
	}
}

func TestRunCheckpointPayloadIncludesApprovalProvenance(t *testing.T) {
	s, _, epicID, runKey := checkpointTestServer(t, "approved by ACMM", 7)
	s.LifecycleTimeline().Record(timeline.Event{
		IssueRef: runKey,
		Kind:     timeline.KindStageApproval,
		Agent:    runCheckpointAutoActor,
		At:       time.Now().UnixMilli(),
		Attrs: map[string]string{
			stageAttrStage:               StagePlan,
			stageAttrGen:                 "7",
			runCheckpointActorKey:        runCheckpointAutoActor,
			runCheckpointReasonKey:       "checkpoint_disabled; ACMM L6 plan_auto_approve",
			runCheckpointConfigSourceKey: "hive.yaml",
			runCheckpointEpicKey:         epicID,
		},
	})

	rec := doOwnerGet(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}
	payload := decodeCheckpointPayload(t, rec)
	if payload.Approval[runCheckpointActorKey] != runCheckpointAutoActor ||
		payload.Approval[runCheckpointConfigSourceKey] != "hive.yaml" ||
		payload.Approval[runCheckpointEpicKey] != epicID ||
		!strings.Contains(payload.Approval[runCheckpointReasonKey], "ACMM L6") {
		t.Fatalf("approval provenance = %+v", payload.Approval)
	}
}

func TestRunCheckpointDecisionRefusesStaleGeneration(t *testing.T) {
	s, store, epicID, runKey := checkpointTestServer(t, "", 9)

	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint", runCheckpointDecisionRequest{Action: runCheckpointDecisionApprove, Gen: 8})
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale approve = %d body=%s", rec.Code, rec.Body.String())
	}
	epic, _ := store.Get(epicID)
	if got := epic.Meta(planning.MetaPlanStatus); got != planning.PlanStatusDraft {
		t.Fatalf("plan status after stale approve = %q, want draft", got)
	}
}

func TestRunCheckpointDecisionRequiresOwner(t *testing.T) {
	s, store, epicID, runKey := checkpointTestServer(t, "", 3)

	rec := doPostNoOwner(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint", runCheckpointDecisionRequest{Action: runCheckpointDecisionApprove, Gen: 3})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner approve = %d body=%s", rec.Code, rec.Body.String())
	}
	epic, _ := store.Get(epicID)
	if got := epic.Meta(planning.MetaPlanStatus); got != planning.PlanStatusDraft {
		t.Fatalf("plan status after unauthorized approve = %q, want draft", got)
	}
}

func TestRunCheckpointDecisionApprovesCurrentGeneration(t *testing.T) {
	s, store, epicID, runKey := checkpointTestServer(t, "", 4)

	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint", runCheckpointDecisionRequest{Action: runCheckpointDecisionApprove, Gen: 4})
	if rec.Code != http.StatusOK {
		t.Fatalf("current-generation approve = %d body=%s", rec.Code, rec.Body.String())
	}
	epic, _ := store.Get(epicID)
	if got := epic.Meta(planning.MetaPlanStatus); got != planning.PlanStatusApproved {
		t.Fatalf("plan status after approve = %q, want approved", got)
	}
}

func TestRunCheckpointRejectResetsImplementLease(t *testing.T) {
	s, store, epicID, runKey := checkpointTestServer(t, "", 6)
	markCheckpointDesign(t, s, store, epicID, StagePlan, planning.DesignStatusRequested)
	if err := store.Update(epicID, func(b *beads.Bead) {
		b.Metadata[planning.MetaPlanStatus] = planning.PlanStatusApproved
		b.Metadata[planning.MetaRunWaitingReason] = planning.WaitingReasonStalePlan
	}); err != nil {
		t.Fatalf("mark stale approved plan: %v", err)
	}
	s.contributeHub.leaseMu.Lock()
	for _, l := range s.contributeHub.leases {
		if l != nil && l.taskID == "task-8618" {
			l.stage = StageImplement
			l.gen = 6
		}
	}
	s.contributeHub.leaseMu.Unlock()

	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint", runCheckpointDecisionRequest{Action: runCheckpointDecisionReject, Gen: 6})
	if rec.Code != http.StatusOK {
		t.Fatalf("reject checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}
	epic, _ := store.Get(epicID)
	if got := epic.Meta(planning.MetaPlanStatus); got != planning.PlanStatusDraft {
		t.Fatalf("plan status after reject = %q, want draft", got)
	}
	lease, ok := s.contributeHub.runLeaseHolder(runKey, time.Now())
	if !ok {
		t.Fatalf("run lease not found after reject")
	}
	if lease.stage != StagePlan || lease.gen <= 6 {
		t.Fatalf("lease after reject = stage %q gen %d, want plan and new generation", lease.stage, lease.gen)
	}
}

func TestRunCheckpointPayloadIdenticalAcrossSurfaces(t *testing.T) {
	s, _, _, runKey := checkpointTestServer(t, "", 5)

	rec := doOwnerGet(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}

	fromHTTP := decodeCheckpointPayload(t, rec)
	fromChat, err := s.RunCheckpointNotificationPayload(runKey, "chat")
	if err != nil {
		t.Fatalf("chat payload: %v", err)
	}
	fromPush, err := s.RunCheckpointNotificationPayload(runKey, "push")
	if err != nil {
		t.Fatalf("push payload: %v", err)
	}
	if !reflect.DeepEqual(fromHTTP, fromChat) || !reflect.DeepEqual(fromHTTP, fromPush) {
		t.Fatalf("payloads differ\nhttp=%+v\nchat=%+v\npush=%+v", fromHTTP, fromChat, fromPush)
	}
}

func TestRunCheckpointPayloadSummarizesSpecDocument(t *testing.T) {
	s, store, epicID, runKey := checkpointTestServer(t, "Review spec checkpoint", 12)
	oldReceipts := runReceiptsDir
	runReceiptsDir = t.TempDir()
	t.Cleanup(func() { runReceiptsDir = oldReceipts })
	if err := store.Update(epicID, func(b *beads.Bead) {
		b.Metadata[planning.MetaDesignVia] = planning.DesignViaSpektacular
		b.Metadata[planning.MetaDesignStatus] = planning.DesignStatusRequested
	}); err != nil {
		t.Fatalf("mark design epic: %v", err)
	}
	s.contributeHub.leaseMu.Lock()
	for _, l := range s.contributeHub.leases {
		if l != nil && l.taskID == "task-8618" {
			l.stage = StageSpec
			l.gen = 12
			l.key = "myorg/repo1!" + runKey + ":" + StageSpec
		}
	}
	s.contributeHub.leaseMu.Unlock()
	if _, err := writeStageReceipt(runKey, StageSpec, 12, []byte(`{}`)); err != nil {
		t.Fatalf("write spec receipt: %v", err)
	}
	if err := writeSpekStageCapture(runKey, StageSpec, 12, RunDetailStageCapture{
		SchemaVersion: "1",
		RunKey:        runKey,
		Stage:         StageSpec,
		Generation:    12,
		Documents: []RunDetailStageDocument{{
			Path:     "spec.md",
			Markdown: "# Checkout redesign\n\n## Goals\n\n## Non-goals",
			Content:  "# Checkout redesign\n\n## Goals\n\n## Non-goals",
		}},
	}); err != nil {
		t.Fatalf("write spec capture: %v", err)
	}

	rec := doOwnerGet(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET spec checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}
	payload := decodeCheckpointPayload(t, rec)
	if payload.Stage != StageSpec || payload.Gen != 12 {
		t.Fatalf("payload stage/gen = %s/%d, want spec/12", payload.Stage, payload.Gen)
	}
	for _, want := range []string{"Checkout redesign", "- Goals", "- Non-goals", "Spec detail:"} {
		if !strings.Contains(payload.Summary, want) {
			t.Fatalf("spec summary missing %q: %s", want, payload.Summary)
		}
	}
}

func markCheckpointDesign(t *testing.T, s *Server, store *beads.Store, epicID, stage, status string) {
	t.Helper()
	if err := store.Update(epicID, func(b *beads.Bead) {
		b.Metadata[planning.MetaDesignVia] = planning.DesignViaSpektacular
		b.Metadata[planning.MetaDesignStatus] = status
	}); err != nil {
		t.Fatal(err)
	}
	s.contributeHub.leaseMu.Lock()
	for _, l := range s.contributeHub.leases {
		if l != nil && l.taskID == "task-8618" {
			l.stage = stage
		}
	}
	s.contributeHub.leaseMu.Unlock()
}

func TestRunCheckpointPlanDecisionWithUnapprovedDesign(t *testing.T) {
	for _, action := range []string{"approve", "reject"} {
		t.Run(action, func(t *testing.T) {
			s, store, epicID, runKey := checkpointTestServer(t, "", 7)
			markCheckpointDesign(t, s, store, epicID, StagePlan, planning.DesignStatusRequested)
			rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint", runCheckpointDecisionRequest{Action: action, Gen: 7})
			if rec.Code != http.StatusOK {
				t.Fatalf("decision = %d: %s", rec.Code, rec.Body.String())
			}
			epic, _ := store.Get(epicID)
			lease, ok := s.contributeHub.runLeaseHolder(runKey, time.Now())
			wantStage, wantStatus := StageImplement, planning.PlanStatusApproved
			if action == "reject" {
				wantStage, wantStatus = StagePlan, planning.PlanStatusDraft
			}
			if !ok || lease.stage != wantStage || (action == "approve" && lease.gen <= 7) || epic.Meta(planning.MetaPlanStatus) != wantStatus {
				t.Fatalf("plan decision no-op: lease=%+v status=%s", lease, epic.Meta(planning.MetaPlanStatus))
			}
			if planning.DesignStatus(epic) != planning.DesignStatusRequested {
				t.Fatal("plan decision modified design")
			}
		})
	}
}

func TestRunCheckpointSpecApprovalSignalFailure(t *testing.T) {
	s, store, epicID, runKey := checkpointTestServer(t, "", 7)
	markCheckpointDesign(t, s, store, epicID, StageSpec, planning.DesignStatusRequested)
	var calls atomic.Int32
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "GitHub unavailable", http.StatusInternalServerError)
	}))
	defer ghServer.Close()
	s.deps.GHClient = github.NewClient("token", "myorg", []string{"repo1"}, s.logger, ghServer.URL)
	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint", runCheckpointDecisionRequest{Action: "approve", Gen: 7})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", rec.Code, rec.Body.String())
	}
	epic, _ := store.Get(epicID)
	lease, ok := s.contributeHub.runLeaseHolder(runKey, time.Now())
	if !ok || lease.stage != StagePlan || planning.DesignStatus(epic) != planning.DesignStatusApproved {
		t.Fatalf("spec wedged: %+v", lease)
	}
	if calls.Load() == 0 {
		t.Fatal("GitHub signal not attempted")
	}
	found := false
	for _, event := range s.LifecycleTimeline().ByIssue(runKey) {
		found = found || event.Attrs["design_signal_error"] != ""
	}
	if !found {
		t.Fatal("missing signal failure timeline event")
	}
	rec = doOwnerPost(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint", runCheckpointDecisionRequest{Action: "approve", Gen: 7})
	if rec.Code != http.StatusConflict {
		t.Fatalf("replay = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRunCheckpointSpecApprovalAlreadyApproved(t *testing.T) {
	s, store, epicID, runKey := checkpointTestServer(t, "", 7)
	markCheckpointDesign(t, s, store, epicID, StageSpec, planning.DesignStatusApproved)
	if err := store.SetMetadata(epicID, planning.MetaPlanStatus, ""); err != nil {
		t.Fatal(err)
	}
	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint", runCheckpointDecisionRequest{Action: "approve", Gen: 7})
	if rec.Code != http.StatusOK {
		t.Fatalf("retry = %d: %s", rec.Code, rec.Body.String())
	}
	lease, ok := s.contributeHub.runLeaseHolder(runKey, time.Now())
	if !ok || lease.stage != StagePlan {
		t.Fatalf("retry did not advance spec: %+v", lease)
	}
}

func TestRunCheckpointDisabledSpecApprovesDesign(t *testing.T) {
	s, store, epicID, runKey := checkpointTestServer(t, "", 7)
	markCheckpointDesign(t, s, store, epicID, StageSpec, planning.DesignStatusRequested)
	oldReceipts := runReceiptsDir
	runReceiptsDir = t.TempDir()
	t.Cleanup(func() { runReceiptsDir = oldReceipts })
	off := false
	s.deps.Config.Runs.Checkpoints.Spec = &off
	if err := s.AdvanceStageLease("alice", "task-8618", StagePlan, time.Now(), []byte(`{}`), map[string]string{stageAttrRunKey: runKey}); err != nil {
		t.Fatal(err)
	}
	epic, _ := store.Get(epicID)
	if planning.DesignStatus(epic) != planning.DesignStatusApproved || epic.Meta(planning.MetaPlanStatus) != planning.PlanStatusDraft {
		t.Fatalf("auto approval: design=%s plan=%s", planning.DesignStatus(epic), epic.Meta(planning.MetaPlanStatus))
	}
	attrs := s.runCheckpointApprovalAttrs(runKey, StageSpec, 7)
	if attrs[runCheckpointActorKey] != runCheckpointAutoActor || attrs[runCheckpointEpicKey] != epicID {
		t.Fatalf("missing provenance: %+v", attrs)
	}
}

func TestAdvanceApprovedCheckpointRequiresMatchingLease(t *testing.T) {
	s, _, epicID, runKey := checkpointTestServer(t, "", 7)
	if err := s.advanceApprovedSpecLease(runKey, epicID, "owner", time.Now()); err == nil {
		t.Fatal("approval succeeded without spec lease")
	}
}

func TestRunCheckpointSpecLeaseFailurePreservesDesign(t *testing.T) {
	s, store, epicID, runKey := checkpointTestServer(t, "", 7)
	markCheckpointDesign(t, s, store, epicID, StageSpec, planning.DesignStatusRequested)
	// A directory cannot be replaced by the lease registry's atomic rename.
	s.contributeHub.persistTaskLedgers = true
	s.contributeHub.taskLeasesFile = t.TempDir()
	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint", runCheckpointDecisionRequest{Action: "approve", Gen: 7})
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve = %d: %s", rec.Code, rec.Body.String())
	}
	epic, _ := store.Get(epicID)
	if planning.DesignStatus(epic) != planning.DesignStatusRequested {
		t.Fatal("failed lease advance published design approval")
	}
	s.contributeHub.persistTaskLedgers = false
	rec = doOwnerPost(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint", runCheckpointDecisionRequest{Action: "approve", Gen: 7})
	if rec.Code != http.StatusOK {
		t.Fatalf("retry = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRunCheckpointSpecApprovalFencesRetriedGeneration(t *testing.T) {
	s, store, epicID, runKey := checkpointTestServer(t, "", 7)
	markCheckpointDesign(t, s, store, epicID, StageSpec, planning.DesignStatusRequested)
	if err := s.RetryStageLease("alice", "task-8618", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.advanceApprovedSpecLease(runKey, epicID, "owner", time.Now(), 7); err == nil {
		t.Fatal("stale approval advanced a retried spec lease")
	}
	lease, ok := s.contributeHub.runLeaseHolder(runKey, time.Now())
	if !ok || lease.stage != StageSpec || lease.gen != 8 {
		t.Fatalf("stale approval changed lease: %+v", lease)
	}
}
