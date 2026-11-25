package admission

import (
	"testing"

	"github.com/opendatahub-io/ai-metering-service/internal/api"
)

func TestResourceHash_Deterministic(t *testing.T) {
	resource := map[string]string{"model": "claude-sonnet-5-5", "provider": "anthropic"}
	h1 := ResourceHash(resource)
	h2 := ResourceHash(resource)
	if h1 != h2 {
		t.Fatalf("hash not deterministic: %s != %s", h1, h2)
	}
}

func TestResourceHash_Length(t *testing.T) {
	resource := map[string]string{"model": "claude-sonnet-5-5", "provider": "anthropic"}
	h := ResourceHash(resource)
	if len(h) != 16 {
		t.Fatalf("expected 16 hex chars, got %d: %s", len(h), h)
	}
}

func TestResourceHash_OrderIndependent(t *testing.T) {
	r1 := map[string]string{"model": "claude-sonnet-5-5", "provider": "anthropic"}
	r2 := map[string]string{"provider": "anthropic", "model": "claude-sonnet-5-5"}
	if ResourceHash(r1) != ResourceHash(r2) {
		t.Fatal("hash should be order-independent")
	}
}

func TestResourceHash_DifferentResources(t *testing.T) {
	r1 := map[string]string{"model": "claude-sonnet-5-5", "provider": "anthropic"}
	r2 := map[string]string{"model": "granite-3.3-8b", "provider": "ibm"}
	if ResourceHash(r1) == ResourceHash(r2) {
		t.Fatal("different resources should produce different hashes")
	}
}

func TestResourceHash_NoCollisionOnDelimiters(t *testing.T) {
	r1 := map[string]string{"a": "b|c=d"}
	r2 := map[string]string{"a": "b", "c": "d"}
	if ResourceHash(r1) == ResourceHash(r2) {
		t.Fatal("delimiter characters in values should not cause collisions")
	}
}

func TestBalanceKey(t *testing.T) {
	key := BuildBalanceKey("acme-corp", "budget-premium")
	expected := "ai-metering:acme-corp:balance:budget-premium"
	if key != expected {
		t.Fatalf("expected %s, got %s", expected, key)
	}
}

func TestBuildAdmissionKey(t *testing.T) {
	resource := map[string]string{"model": "claude-sonnet-5-5", "provider": "anthropic"}
	hash := ResourceHash(resource)

	cases := []struct {
		subject api.Subject
		want    string
	}{
		{api.Subject{Kind: "user", Name: "alice"}, "ai-metering:acme-corp:admit:user:alice:" + hash},
		{api.Subject{Kind: "group", Name: "engineering"}, "ai-metering:acme-corp:admit:group:engineering:" + hash},
		{api.Subject{Kind: "organization", Name: "research"}, "ai-metering:acme-corp:admit:organization:research:" + hash},
		{api.Subject{Kind: "tenant", Name: "acme-corp"}, "ai-metering:acme-corp:admit:tenant:acme-corp:" + hash},
	}
	for _, c := range cases {
		got := BuildAdmissionKey("acme-corp", c.subject, resource)
		if got != c.want {
			t.Errorf("BuildAdmissionKey(%+v): expected %s, got %s", c.subject, c.want, got)
		}
	}
}

func TestBuildAdmissionKey_MatchesBuildAdmissionKeys(t *testing.T) {
	// The subject-based builder must agree with the identity-based builder
	// for the same (kind, name, resource) — the Rust crate reads these keys.
	identity := api.Identity{
		User:         "alice",
		Groups:       []string{"engineering"},
		Organization: "research",
		Tenant:       "acme-corp",
	}
	resource := map[string]string{"model": "granite-3.3-8b", "provider": "ibm"}

	keys := BuildAdmissionKeys("acme-corp", identity, resource)

	pairs := map[string]string{
		BuildAdmissionKey("acme-corp", api.Subject{Kind: "user", Name: "alice"}, resource): keys.User,
		BuildAdmissionKey("acme-corp", api.Subject{Kind: "group", Name: "engineering"}, resource): keys.Groups[0],
		BuildAdmissionKey("acme-corp", api.Subject{Kind: "organization", Name: "research"}, resource): keys.Organization,
		BuildAdmissionKey("acme-corp", api.Subject{Kind: "tenant", Name: "acme-corp"}, resource): keys.Tenant,
	}
	for subjectKey, identityKey := range pairs {
		if subjectKey != identityKey {
			t.Errorf("subject-based key %s does not match identity-based key %s", subjectKey, identityKey)
		}
	}
}

func TestBuildAdmitKeys_AllFields(t *testing.T) {
	identity := api.Identity{
		User:         "alice",
		Groups:       []string{"engineering", "ml-team"},
		Organization: "research",
		Tenant:       "acme-corp",
	}
	resource := map[string]string{"model": "claude-sonnet-5-5", "provider": "anthropic"}
	hash := ResourceHash(resource)

	keys := BuildAdmissionKeys("acme-corp", identity, resource)

	expected := "ai-metering:acme-corp:admit:user:alice:" + hash
	if keys.User != expected {
		t.Errorf("User key: expected %s, got %s", expected, keys.User)
	}

	if len(keys.Groups) != 2 {
		t.Fatalf("expected 2 group keys, got %d", len(keys.Groups))
	}
	if keys.Groups[0] != "ai-metering:acme-corp:admit:group:engineering:"+hash {
		t.Errorf("Group[0]: got %s", keys.Groups[0])
	}
	if keys.Groups[1] != "ai-metering:acme-corp:admit:group:ml-team:"+hash {
		t.Errorf("Group[1]: got %s", keys.Groups[1])
	}

	expected = "ai-metering:acme-corp:admit:organization:research:" + hash
	if keys.Organization != expected {
		t.Errorf("Organization key: expected %s, got %s", expected, keys.Organization)
	}

	expected = "ai-metering:acme-corp:admit:tenant:acme-corp:" + hash
	if keys.Tenant != expected {
		t.Errorf("Tenant key: expected %s, got %s", expected, keys.Tenant)
	}
}

func TestBuildAdmitKeys_EmptyGroups(t *testing.T) {
	identity := api.Identity{
		User:         "bob",
		Organization: "research",
		Tenant:       "acme-corp",
	}
	resource := map[string]string{"model": "granite-3.3-8b", "provider": "ibm"}

	keys := BuildAdmissionKeys("acme-corp", identity, resource)

	if len(keys.Groups) != 0 {
		t.Fatalf("expected 0 group keys, got %d", len(keys.Groups))
	}
	if keys.User == "" {
		t.Error("expected user key to be set")
	}
	if keys.Tenant == "" {
		t.Error("expected tenant key to be set")
	}
}

func TestBuildAdmitKeys_NoUser(t *testing.T) {
	identity := api.Identity{
		Groups: []string{"data-science"},
		Tenant: "acme-corp",
	}
	resource := map[string]string{"model": "granite-3.3-8b", "provider": "ibm"}

	keys := BuildAdmissionKeys("acme-corp", identity, resource)

	if keys.User != "" {
		t.Errorf("expected empty user key, got %s", keys.User)
	}
	if keys.Organization != "" {
		t.Errorf("expected empty org key, got %s", keys.Organization)
	}
	if len(keys.Groups) != 1 {
		t.Fatalf("expected 1 group key, got %d", len(keys.Groups))
	}
}
