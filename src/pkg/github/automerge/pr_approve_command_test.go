package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestPRApproveReply(t *testing.T) {
	for _, tc := range []struct {
		name, author, actor, verdict, file, want               string
		noHold, stale, bot, disabled, readError, trustedAuthor bool
	}{
		{name: "App lane", author: testHiveAppBotLogin, actor: "owner", want: "acknowledged as your decision"},
		{name: "trusted owner on own contributor PR", author: "owner", actor: "owner", want: "acknowledged as your decision"},
		{name: "trusted merger on contributor PR", author: "contributor", actor: "owner", want: "acknowledged as your decision"},
		{name: "untrusted self release", author: "contributor", actor: "contributor", want: "cannot release their own PR"},
		{name: "untrusted other release", author: "contributor", actor: "stranger", want: "not a trusted merger"},
		{name: "bot", author: "contributor", actor: "owner", bot: true, want: "a bot cannot release"},
		{name: "trusted-author review only", author: "maintainer", actor: "owner", trustedAuthor: true, want: "trusted-author lane does not consume"},
		{name: "not held", author: "contributor", actor: "owner", noHold: true, want: "no active review-verdict hold"},
		{name: "stale", author: "contributor", actor: "owner", stale: true, want: "predates the latest holding verdict"},
		{name: "rejected contributor", author: "contributor", actor: "owner", verdict: "reject", want: "cannot override this contributor-lane verdict"},
		{name: "manual workflow", author: "contributor", actor: "owner", file: ".github/workflows/ci.yml", want: "requires a person to merge manually"},
		{name: "manual path", author: testHiveAppBotLogin, actor: "owner", file: "sensitive/config.yaml", want: "requires a person to merge manually"},
		{name: "disabled lane", author: "contributor", actor: "owner", disabled: true, want: "must merge it manually"},
		{name: "missing path evidence", author: "contributor", actor: "owner", readError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict := tc.verdict
			if verdict == "" {
				verdict = "requires_human"
			}
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("feedback must not mutate: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusForbidden)
					return
				}
				switch r.URL.Path {
				case "/repos/acme/widget/pulls/7":
					json.NewEncoder(w).Encode(&gh.PullRequest{Number: gh.Ptr(7), State: gh.Ptr("open"), User: &gh.User{Login: gh.Ptr(tc.author)}, Head: &gh.PullRequestBranch{SHA: gh.Ptr("sha7")}})
				case "/repos/acme/widget/pulls/7/files":
					if tc.readError {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					file := tc.file
					if file == "" {
						file = "src/main.go"
					}
					json.NewEncoder(w).Encode([]*gh.CommitFile{{Filename: gh.Ptr(file), Status: gh.Ptr("modified")}})
				default:
					t.Errorf("unexpected read: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer api.Close()
			client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, api.URL)
			client.SetAppBotLogin(testHiveAppBotLogin)
			e := New(client, Options{
				MergerAuthorizer:      func(login string) bool { return login == "owner" },
				TrustedAuthorPolicy:   func() TrustedAuthorPolicy { return TrustedAuthorPolicy{Enabled: tc.trustedAuthor} },
				TrustedAuthorizer:     func(string, string) TrustedAuthorDecision { return TrustedAuthorDecision{Allowed: tc.trustedAuthor} },
				ContributorLanePolicy: func() ContributorLanePolicy { return ContributorLanePolicy{Enabled: true} },
				ContributorLaneVerdict: func(string, int, string) ContributorLaneVerdict {
					return ContributorLaneVerdict{Configured: true, Verdict: verdict}
				},
				ReviewVerdictHold: func(string, int) ReviewVerdictHold {
					return ReviewVerdictHold{Held: !tc.noHold, Verdict: verdict, RecordedAt: reviewHoldVerdictAt}
				},
				HumanMergePaths:         func(string) []string { return []string{"sensitive/**"} },
				RepoAutoMergeEnabled:    func(string) bool { return !tc.disabled },
				RecordReviewHoldRelease: func(string, int, hgithub.ReviewHoldRelease) { t.Error("feedback must not record a release") },
			})
			at := reviewHoldVerdictAt.Add(time.Minute)
			if tc.stale {
				at = reviewHoldVerdictAt.Add(-time.Minute)
			}
			userType := "User"
			if tc.bot {
				userType = "Bot"
			}
			reply, err := e.PRApproveReply(context.Background(), "acme/widget", 7, &gh.IssueComment{Body: gh.Ptr("/hive approve"), User: &gh.User{Login: gh.Ptr(tc.actor), Type: gh.Ptr(userType)}, CreatedAt: &gh.Timestamp{Time: at}})
			if tc.readError {
				if err == nil || reply != "" {
					t.Fatalf("failed evidence: reply=%q err=%v", reply, err)
				}
				return
			}
			if err != nil || !strings.Contains(reply, tc.want) {
				t.Fatalf("reply=%q err=%v, want %q", reply, err, tc.want)
			}
		})
	}
}

func TestDecisionCommentNamesLaneAction(t *testing.T) {
	for _, tc := range []struct {
		name, lane, reason, want string
		holds                    []string
		manual                   bool
	}{
		{name: "self", lane: stageLaneSelf, want: "owner or a trusted merger can reply `/hive approve`"},
		{name: "contributor", lane: stageLaneContributor, want: "ordinary contributor cannot release their own PR"},
		{name: "trusted author", lane: stageLaneTrusted, want: "GitHub **Approve** review for the current head"},
		{name: "workflow", lane: stageLaneContributor, reason: contributorLaneReasonWorkflowPath, want: "merge this PR manually", manual: true},
		{name: "tests removed", lane: stageLaneContributor, reason: contributorLaneReasonTestsRemoved, want: "merge this PR manually", manual: true},
		{name: "human path", lane: stageLaneSelf, reason: autoMergeReasonHumanMergePath, want: "merge this PR manually", manual: true},
		{name: "label hold", lane: stageLaneSelf, holds: []string{"hold"}, want: "remove that label"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := stagelabels.Evidence{Verdict: stagelabels.VerdictRequiresHuman, Holds: tc.holds}
			route := stagelabels.RouteHuman(e)
			route.Actions, route.ManualMerge = decisionActions(stageObservation{Lane: tc.lane, Reason: tc.reason}, e)
			body := stagelabels.DecisionComment(route)
			if !strings.Contains(body, tc.want) || strings.Contains(body, "Clear the cause") || route.ManualMerge != tc.manual {
				t.Fatalf("manual=%v body=%q", route.ManualMerge, body)
			}
			if tc.manual && strings.Contains(body, "not a merge") {
				t.Fatalf("contradictory manual guidance: %q", body)
			}
		})
	}
}
