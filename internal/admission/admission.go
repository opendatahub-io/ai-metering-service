package admission

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/opendatahub-io/ai-metering-service/internal/api"
)

type AdmissionKeys struct {
	User         string
	Groups       []string
	Organization string
	Tenant       string
}

func ResourceHash(resource map[string]string) string {
	keys := slices.Sorted(maps.Keys(resource))
	parts := make([]string, len(keys))
	for i, key := range keys {
		parts[i] = escape(key) + "=" + escape(resource[key])
	}
	seq := strings.Join(parts, "|")
	hash := sha256.Sum256([]byte(seq))
	return hex.EncodeToString(hash[:8])
}

var escaper = strings.NewReplacer(`\`, `\\`, `=`, `\=`, `|`, `\|`)

func escape(s string) string {
	return escaper.Replace(s)
}

func BuildBalanceKey(tenantID, budgetID string) string {
	return fmt.Sprintf("ai-metering:%s:balance:%s", tenantID, budgetID)
}

func BuildReverseMapEntry(tenantID string, resource map[string]string) (key string, value string, err error) {
	jsonResource, err := json.Marshal(resource)
	if err != nil {
		return "", "", err
	}
	key = fmt.Sprintf("ai-metering:%s:resource:%s", tenantID, ResourceHash(resource))
	value = string(jsonResource)
	return key, value, nil
}

// BuildAdmissionKey builds the admission flag key for one (subject, resource)
// pair. SubjectKind values are the key segments themselves, and the set is
// closed (validated at policy creation), so the kind embeds directly.
func BuildAdmissionKey(tenantID string, subject api.Subject, resource map[string]string) string {
	return fmt.Sprintf("ai-metering:%s:admit:%s:%s:%s",
		tenantID, subject.Kind, subject.Name, ResourceHash(resource))
}

func BuildAdmissionKeys(tenantID string, identity api.Identity, resource map[string]string) AdmissionKeys {
	hash := ResourceHash(resource)

	keys := AdmissionKeys{
		Tenant: fmt.Sprintf("ai-metering:%s:admit:tenant:%s:%s", tenantID, tenantID, hash),
	}

	if identity.User != "" {
		keys.User = fmt.Sprintf("ai-metering:%s:admit:user:%s:%s", tenantID, identity.User, hash)
	}

	for _, group := range identity.Groups {
		keys.Groups = append(keys.Groups, fmt.Sprintf("ai-metering:%s:admit:group:%s:%s", tenantID, group, hash))
	}

	if identity.Organization != "" {
		keys.Organization = fmt.Sprintf("ai-metering:%s:admit:organization:%s:%s", tenantID, identity.Organization, hash)
	}

	return keys
}
