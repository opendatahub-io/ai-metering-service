//! The read side of the admission flags. The Go metering service resolves
//! every budget applying to an identity and writes one flag per
//! (identity object, resource) pair; this client recomputes the same keys and
//! reads the flags back, so the decision reflects the service's last write.

use super::keys::build_admission_keys;
use super::redis::RedisKVStore;
use super::{AdmissionError, Identity, KVStore, Resource};

/// AdmissionResult is the read-side mirror of the Go service's admission
/// State values ("allow", "over", "deny" in internal/admission/state.go).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AdmissionResult {
    Allowed,
    Over,
    Denied,
}

impl AdmissionResult {
    /// from_value parses a flag value as the Go service writes it. An
    /// unexpected value is corrupt state: deny is the defense-in-depth
    /// answer (mirroring Go's OverageState treatment of invalid rows),
    /// since reading it as absent would fail open.
    fn from_value(value: &str) -> AdmissionResult {
        match value {
            "allow" => AdmissionResult::Allowed,
            "over" => AdmissionResult::Over,
            _ => AdmissionResult::Denied,
        }
    }

    /// most_restrictive mirrors Go's admission.MostRestrictive: deny beats
    /// over beats allow.
    fn most_restrictive(self, other: AdmissionResult) -> AdmissionResult {
        use AdmissionResult::*;
        if self == Denied || other == Denied {
            Denied
        } else if self == Over || other == Over {
            Over
        } else {
            Allowed
        }
    }
}

pub struct AdmissionClient {
    store: Box<dyn KVStore + Send + Sync>,
}

impl AdmissionClient {
    /// with_store builds a client over any KVStore — the seam for an
    /// in-memory mock in tests and for backends other than Redis.
    pub fn new_with_store(store: Box<dyn KVStore + Send + Sync>) -> Self {
        Self { store }
    }

    /// new builds the default Redis-backed client (see RedisKVStore). Note
    /// that it performs I/O: it opens the Redis connection eagerly so an
    /// unreachable Redis fails at construction rather than on first check.
    pub async fn new(url: String) -> Result<Self, AdmissionError> {
        Ok(Self::new_with_store(Box::new(RedisKVStore::new(url).await?)))
    }


    /// check_admission reads the admission flags for one (identity, resource)
    /// pair with a single bulk get and folds them most-restrictive — the
    /// read-time precedence across a user's group, organization and tenant
    /// flags. A key the service never wrote contributes nothing (no budget
    /// applies to that identity object); if no key exists at all the pair is
    /// allowed, matching the service's no-budget design. The tenant is taken
    /// from the identity, mirroring how the service scopes every key.
    pub async fn check_admission(
        &self,
        identity: &Identity,
        resource: &Resource<'_>,
    ) -> Result<AdmissionResult, AdmissionError> {
        let keys = build_admission_keys(&identity.tenant, identity, resource);
        let all: Vec<String> = keys.all().into_iter().map(str::to_string).collect();

        let values = self.store.get(&all).await?;

        Ok(values
            .into_iter()
            .flatten()
            .map(|value| AdmissionResult::from_value(&value))
            .fold(AdmissionResult::Allowed, AdmissionResult::most_restrictive))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use async_trait::async_trait;

    struct MockStore(Vec<Option<String>>);

    #[async_trait]
    impl KVStore for MockStore {
        async fn get(&self, _keys: &[String]) -> Result<Vec<Option<String>>, AdmissionError> {
            Ok(self.0.clone())
        }
    }

    fn client(values: Vec<Option<String>>) -> AdmissionClient {
        AdmissionClient::new_with_store(Box::new(MockStore(values)))
    }

    fn identity() -> Identity {
        Identity {
            user: "alice".to_string(),
            groups: vec!["engineering".to_string(), "ml-team".to_string()],
            organization: Some("research".to_string()),
            tenant: "acme-corp".to_string(),
        }
    }

    fn resource() -> Resource<'static> {
        [("model", "claude-sonnet-5-5")].into_iter().collect()
    }

    fn check(values: Vec<Option<String>>) -> AdmissionResult {
        let client = client(values);
        tokio::runtime::Builder::new_current_thread()
            .build()
            .unwrap()
            .block_on(client.check_admission(&identity(), &resource()))
            .unwrap()
    }

    #[test]
    fn most_restrictive_deny_beats_over_beats_allow() {
        use AdmissionResult::*;
        assert_eq!(Allowed.most_restrictive(Allowed), Allowed);
        assert_eq!(Allowed.most_restrictive(Over), Over);
        assert_eq!(Over.most_restrictive(Allowed), Over);
        assert_eq!(Over.most_restrictive(Over), Over);
        assert_eq!(Over.most_restrictive(Denied), Denied);
        assert_eq!(Allowed.most_restrictive(Denied), Denied);
        assert_eq!(Denied.most_restrictive(Allowed), Denied);
    }

    #[test]
    fn from_value_parses_go_states() {
        assert_eq!(AdmissionResult::from_value("allow"), AdmissionResult::Allowed);
        assert_eq!(AdmissionResult::from_value("over"), AdmissionResult::Over);
        assert_eq!(AdmissionResult::from_value("deny"), AdmissionResult::Denied);
        // Corrupt state fails closed.
        assert_eq!(AdmissionResult::from_value(""), AdmissionResult::Denied);
        assert_eq!(AdmissionResult::from_value("bogus"), AdmissionResult::Denied);
    }

    // The identity above expands to 5 keys: user, 2 groups, organization,
    // tenant — matching the MockStore value positions below.
    #[test]
    fn check_admission_folds_most_restrictive() {
        // user=allow, groups=over/allow, org=missing, tenant=allow -> over
        assert_eq!(
            check(vec![
                Some("allow".into()),
                Some("over".into()),
                Some("allow".into()),
                None,
                Some("allow".into()),
            ]),
            AdmissionResult::Over
        );
        // One deny anywhere denies.
        assert_eq!(
            check(vec![
                Some("allow".into()),
                None,
                None,
                Some("deny".into()),
                Some("over".into()),
            ]),
            AdmissionResult::Denied
        );
    }

    #[test]
    fn check_admission_allows_when_no_flags_exist() {
        assert_eq!(check(vec![None; 5]), AdmissionResult::Allowed);
    }

    #[test]
    fn check_admission_fails_closed_on_corrupt_value() {
        assert_eq!(
            check(vec![Some("bogus".into()), None, None, None, None]),
            AdmissionResult::Denied
        );
    }
}
