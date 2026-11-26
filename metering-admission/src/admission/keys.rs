use sha2::{Digest, Sha256};

use super::utils::escape;
use super::{Identity, Resource};

/// resource_hash mirrors Go's admission.ResourceHash: sort the resource keys
/// byte-wise (Go's slices.Sorted over map keys), join `escape(key)=escape(value)`
/// pairs with `|`, SHA-256 the sequence and hex-encode the first 8 bytes —
/// 16 lowercase hex chars.
pub fn resource_hash(resource: &Resource) -> String {
    let mut keys: Vec<&str> = resource.keys().copied().collect();
    keys.sort_unstable();

    let seq = keys
        .iter()
        .map(|k| format!("{}={}", escape(k), escape(resource[*k])))
        .collect::<Vec<_>>()
        .join("|");

    let digest = Sha256::digest(seq.as_bytes());
    digest[..8].iter().map(|b| format!("{b:02x}")).collect()
}

/// AdmissionKeys mirrors Go's admission.AdmissionKeys: the admission flag
/// keys for one (identity, resource) pair. The Go service leaves optional
/// fields as empty strings; Option makes their absence explicit here.
#[derive(Debug, PartialEq, Eq)]
pub struct AdmissionKeys {
    pub user: Option<String>,
    pub groups: Vec<String>,
    pub organization: Option<String>,
    pub tenant: String,
}

impl AdmissionKeys {
    /// all returns every key in one list, for a single bulk read.
    pub fn all(&self) -> Vec<&str> {
        let mut all = Vec::with_capacity(2 + self.groups.len());
        if let Some(user) = &self.user {
            all.push(user.as_str());
        }
        all.extend(self.groups.iter().map(String::as_str));
        if let Some(organization) = &self.organization {
            all.push(organization.as_str());
        }
        all.push(self.tenant.as_str());
        all
    }
}

/// build_admission_keys mirrors Go's admission.BuildAdmissionKeys. Key shape:
/// `ai-metering:{tenant}:admit:{kind}:{name}:{resource_hash}` where kind is
/// one of user, group, organization, tenant (the closed set in api.SubjectKind).
/// The tenant key always exists; user and organization keys exist only when
/// the identity carries those fields.
pub fn build_admission_keys(tenant_id: &str, identity: &Identity, resource: &Resource) -> AdmissionKeys {
    let hash = resource_hash(resource);

    AdmissionKeys {
        tenant: format!("ai-metering:{tenant_id}:admit:tenant:{tenant_id}:{hash}"),
        user: non_empty(&identity.user).map(|user| format!("ai-metering:{tenant_id}:admit:user:{user}:{hash}")),
        groups: identity
            .groups
            .iter()
            .map(|group| format!("ai-metering:{tenant_id}:admit:group:{group}:{hash}"))
            .collect(),
        organization: identity
            .organization
            .as_deref()
            .and_then(non_empty)
            .map(|organization| format!("ai-metering:{tenant_id}:admit:organization:{organization}:{hash}")),
    }
}

fn non_empty(s: &str) -> Option<&str> {
    (!s.is_empty()).then_some(s)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn resource<'a>(pairs: &'a [(&'a str, &'a str)]) -> Resource<'a> {
        pairs.iter().copied().collect()
    }

    fn identity(user: &str, groups: &[&str], organization: Option<&str>, tenant: &str) -> Identity {
        Identity {
            user: user.to_string(),
            groups: groups.iter().map(|g| g.to_string()).collect(),
            organization: organization.map(|o| o.to_string()),
            tenant: tenant.to_string(),
        }
    }

    // Cross-language vectors: the expected values were produced by the Go
    // implementation (internal/admission.ResourceHash), so a change on
    // either side that breaks key equality fails here.
    #[test]
    fn resource_hash_matches_go() {
        assert_eq!(
            resource_hash(&resource(&[("model", "claude-sonnet-5-5"), ("provider", "anthropic")])),
            "8bd52968850872af"
        );
        assert_eq!(
            resource_hash(&resource(&[("model", "granite-3.3-8b"), ("provider", "ibm")])),
            "beea25129068c3ed"
        );
    }

    #[test]
    fn resource_hash_is_order_independent() {
        let r1 = resource(&[("model", "claude-sonnet-5-5"), ("provider", "anthropic")]);
        let r2 = resource(&[("provider", "anthropic"), ("model", "claude-sonnet-5-5")]);
        assert_eq!(resource_hash(&r1), resource_hash(&r2));
    }

    #[test]
    fn resource_hash_escapes_delimiters() {
        // Mirrors Go's TestResourceHash_NoCollisionOnDelimiters: a value
        // containing the delimiters must not collide with extra pairs.
        let h3 = resource_hash(&resource(&[("a", "b|c=d")]));
        let h4 = resource_hash(&resource(&[("a", "b"), ("c", "d")]));
        assert_eq!(h3, "721039bd3717cc71");
        assert_eq!(h4, "507e4871f0d5e620");
        assert_ne!(h3, h4);
    }

    // Mirrors Go's TestBuildAdmitKeys_AllFields.
    #[test]
    fn build_admission_keys_all_fields() {
        let id = identity("alice", &["engineering", "ml-team"], Some("research"), "acme-corp");
        let resource = resource(&[("model", "claude-sonnet-5-5"), ("provider", "anthropic")]);
        let hash = resource_hash(&resource);

        let keys = build_admission_keys("acme-corp", &id, &resource);

        assert_eq!(
            keys.user.as_deref(),
            Some("ai-metering:acme-corp:admit:user:alice:8bd52968850872af")
        );
        assert_eq!(
            keys.groups,
            vec![
                format!("ai-metering:acme-corp:admit:group:engineering:{hash}"),
                format!("ai-metering:acme-corp:admit:group:ml-team:{hash}")
            ]
        );
        assert_eq!(
            keys.organization.as_deref(),
            Some("ai-metering:acme-corp:admit:organization:research:8bd52968850872af")
        );
        assert_eq!(
            keys.tenant,
            format!("ai-metering:acme-corp:admit:tenant:acme-corp:{hash}")
        );
    }

    // Mirrors Go's TestBuildAdmitKeys_NoUser and TestBuildAdmitKeys_EmptyGroups.
    #[test]
    fn build_admission_keys_skips_absent_fields() {
        let id = identity("", &["data-science"], None, "acme-corp");
        let resource = resource(&[("model", "granite-3.3-8b"), ("provider", "ibm")]);
        let keys = build_admission_keys("acme-corp", &id, &resource);

        assert_eq!(keys.user, None);
        assert_eq!(keys.organization, None);
        assert_eq!(keys.groups.len(), 1);
        assert!(keys.tenant.starts_with("ai-metering:acme-corp:admit:tenant:acme-corp:"));
    }

    #[test]
    fn all_lists_every_key() {
        let id = identity("alice", &["engineering", "ml-team"], Some("research"), "acme-corp");
        let keys = build_admission_keys("acme-corp", &id, &resource(&[("model", "m1")]));
        assert_eq!(keys.all().len(), 5);
    }
}
