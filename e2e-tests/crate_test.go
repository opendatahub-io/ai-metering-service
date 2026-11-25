package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Cross-language e2e: the admission flags the Go service writes to Redis are
// read back through the metering-admission Rust crate — the same crate
// Praxis embeds — via its admission-check CLI harness. Each test drives the
// service through the API exactly like admission_test.go, then asserts the
// crate's folded decision matches the flags the Go side computed. Any drift
// between the two key computations (resource hash, key shape, state values)
// fails here.
//
// Requires cargo in PATH; the suite skips these tests otherwise.

var (
	crateBinOnce sync.Once
	crateBinPath string
	crateBinErr  error
)

// admissionCheckBin builds the crate's CLI harness once per test run and
// returns its path.
func admissionCheckBin(t *testing.T) string {
	t.Helper()
	crateBinOnce.Do(func() {
		if _, err := exec.LookPath("cargo"); err != nil {
			crateBinErr = fmt.Errorf("cargo not found in PATH: %w", err)
			return
		}
		dir := "../metering-admission"
		cmd := exec.Command("cargo", "build", "--example", "admission-check")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			crateBinErr = fmt.Errorf("cargo build: %v\n%s", err, out)
			return
		}
		crateBinPath = filepath.Join(dir, "target", "debug", "examples", "admission-check")
	})
	if crateBinErr != nil {
		t.Skipf("admission crate e2e disabled: %v", crateBinErr)
	}
	return crateBinPath
}

// crateIdentity is the caller identity handed to the crate — the same fields
// the service's event identity carries.
type crateIdentity struct {
	user         string
	groups       []string
	organization string
}

// crateCheck runs the crate's harness against the stack's Redis for the
// test's resource and returns its folded decision: "allow", "over" or "deny".
func crateCheck(t *testing.T, at *admissionTest, id crateIdentity) string {
	t.Helper()

	args := []string{"--url", redisURL(), "--tenant", tenant}
	if id.user != "" {
		args = append(args, "--user", id.user)
	}
	for _, group := range id.groups {
		args = append(args, "--group", group)
	}
	if id.organization != "" {
		args = append(args, "--org", id.organization)
	}
	for k, v := range at.resource {
		args = append(args, "--resource", k+"="+v)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, admissionCheckBin(t), args...).Output()
	if err != nil {
		t.Fatalf("admission-check: %v\n%s", err, out)
	}
	result := strings.TrimSpace(string(out))
	switch result {
	case "allow", "over", "deny":
		return result
	default:
		t.Fatalf("admission-check: unexpected output %q", result)
		return ""
	}
}

// expectCrate asserts the crate's decision for the given identity.
func expectCrate(t *testing.T, at *admissionTest, id crateIdentity, want string) {
	t.Helper()
	if got := crateCheck(t, at, id); got != want {
		t.Fatalf("crate decision for %+v: expected %s, got %s", id, want, got)
	}
}

func TestAdmissionCrate_MatchesServiceFlags(t *testing.T) {
	at := newAdmissionTest(t, "crate-balance", budgetSpec{limit: 1000}) // hard enforcement

	// Cost 300 of 1000: the service writes allow; the crate must read it.
	at.ingest(t, "evt-crate-001", 100)
	at.expectFlag(t, "allow")
	expectCrate(t, at, crateIdentity{user: "alice"}, "allow")

	// Exhaust the budget: the service writes deny; the crate must read it.
	at.ingest(t, "evt-crate-002", 400) // cost 1200 → balance -500
	at.expectFlag(t, "deny")
	expectCrate(t, at, crateIdentity{user: "alice"}, "deny")
}

func TestAdmissionCrate_SoftOverage(t *testing.T) {
	at := newAdmissionTest(t, "crate-soft", budgetSpec{limit: 100, overage: map[string]any{"type": "soft"}})

	at.ingest(t, "evt-crate-soft-001", 200) // cost 600 → balance -500
	at.expectFlag(t, "over")
	expectCrate(t, at, crateIdentity{user: "alice"}, "over")
}

func TestAdmissionCrate_MultiBudgetFold(t *testing.T) {
	// A hard and a soft budget both exhaust: the service folds to deny, and
	// the crate's read-time fold must agree — hard deny beats soft over.
	at := newAdmissionTest(t, "crate-fold",
		budgetSpec{limit: 100},
		budgetSpec{limit: 100, overage: map[string]any{"type": "soft"}},
	)

	at.ingest(t, "evt-crate-fold-001", 400) // cost 1200: both balances -1100
	at.expectFlag(t, "deny")
	expectCrate(t, at, crateIdentity{user: "alice"}, "deny")
}

func TestAdmissionCrate_UserFlagBeatsTenantFlag(t *testing.T) {
	// Alice has a small user-level budget under a large tenant budget. Her
	// user flag is deny while the tenant flag is allow: the crate must fold
	// her user deny over the tenant allow, while Bob (no user flag) falls
	// through to the tenant allow.
	at := newAdmissionTest(t, "crate-specificity", budgetSpec{limit: 10000})

	resp := doRequest(t, "POST", "/api/v1/budgets", map[string]any{
		"name":            "crate-specificity-alice",
		"limit":           100,
		"period_start":    "2026-11-01T00:00:00Z",
		"period_duration": "P1M",
		"renewal_type":    "drain",
	})
	requireStatus(t, resp, http.StatusCreated)
	var budgetCreated map[string]any
	jsonUnmarshal(t, readBody(t, resp), &budgetCreated)
	aliceBudget := budgetCreated["metadata"].(map[string]any)["id"].(string)

	resp = doRequest(t, "POST", "/api/v1/budget_policies", map[string]any{
		"budget_ref": aliceBudget,
		"rate_ref":   at.rateID,
		"subjects":   []map[string]string{{"kind": "user", "name": "alice"}},
	})
	requireStatus(t, resp, http.StatusCreated)

	at.ingest(t, "evt-crate-spec-001", 200) // cost 600: alice -500, tenant 9400
	at.expectFlagFor(t, "user", "alice", "deny")
	at.expectFlagFor(t, "tenant", tenant, "allow")

	expectCrate(t, at, crateIdentity{user: "alice"}, "deny")
	expectCrate(t, at, crateIdentity{user: "bob"}, "allow")
}

func TestAdmissionCrate_GroupFold(t *testing.T) {
	// A group-subject policy exhausts for engineering. Alice (member) folds
	// the group deny across her identity objects; Bob (non-member) has no
	// flags at all — the crate's no-budget case, allowed.
	at := newAdmissionTestForSubjects(t, "crate-group",
		[]map[string]string{{"kind": "group", "name": "engineering"}},
		budgetSpec{limit: 100},
	)

	at.ingestAs(t, "evt-crate-grp-001", map[string]any{
		"user": "alice", "groups": []string{"engineering"}, "tenant": tenant,
	}, 200) // cost 600 → balance -500
	at.expectFlagFor(t, "group", "engineering", "deny")

	expectCrate(t, at, crateIdentity{user: "alice", groups: []string{"engineering", "data-science"}}, "deny")
	expectCrate(t, at, crateIdentity{user: "bob", groups: []string{"data-science"}}, "allow")
}

func TestAdmissionCrate_OrganizationFlag(t *testing.T) {
	// The organization key segment is the full "organization" spelling; the
	// crate must build the same key.
	at := newAdmissionTestForSubjects(t, "crate-org",
		[]map[string]string{{"kind": "organization", "name": "research"}},
		budgetSpec{limit: 100},
	)

	at.ingestAs(t, "evt-crate-org-001", map[string]any{
		"user": "alice", "organization": "research", "tenant": tenant,
	}, 200) // cost 600 → balance -500
	at.expectFlagFor(t, "organization", "research", "deny")

	expectCrate(t, at, crateIdentity{user: "alice", organization: "research"}, "deny")
}

func TestAdmissionCrate_NoFlagsIsAllowed(t *testing.T) {
	// No policy matches this identity at all: no flags exist for the
	// resource, and the crate must treat that as allowed (the service's
	// no-budget fail-open design).
	at := newAdmissionTestForSubjects(t, "crate-nomatch",
		[]map[string]string{{"kind": "user", "name": "carol"}},
		budgetSpec{limit: 100},
	)

	at.ingest(t, "evt-crate-nm-001", 100)
	at.expectFlagAbsent(t, "user", "alice")
	expectCrate(t, at, crateIdentity{user: "alice"}, "allow")
}
