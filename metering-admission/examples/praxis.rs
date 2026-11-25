//! Example: how a consumer (Praxis) imports and uses the metering-admission
//! crate to gate requests against the budgets enforced by the Go metering
//! service.
//!
//! Run against a local Redis:
//!
//! ```sh
//! cargo run --example praxis -- redis://127.0.0.1:6379
//! ```
//!
//! With flags written by the Go service, e.g.:
//!
//! ```sh
//! redis-cli set "ai-metering:acme-corp:admit:group:engineering:<hash>" over
//! ```
//!
//! In Praxis's Cargo.toml the crate is a normal dependency:
//!
//! ```toml
//! [dependencies]
//! metering-admission = { path = "../ai-metering-service/metering-admission" }
//! # or, published: metering-admission = "0.1"
//! ```

use metering_admission::{AdmissionClient, AdmissionResult, Identity, Resource};

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    // Once at startup: open the shared connection. AdmissionClient::new
    // establishes the multiplexed, auto-reconnecting Redis connection and
    // fails fast if Redis is unreachable. Keep one client for the lifetime of
    // the service — it is cheaply shared (the store behind it is an Arc).
    let url = std::env::args()
        .nth(1)
        .unwrap_or_else(|| "redis://127.0.0.1:6379".to_string());
    let admission = AdmissionClient::new(url).await?;

    // Per request: the identity comes from the authenticated caller (in
    // Praxis, extracted from the token/headers), the resource from what is
    // being requested (e.g. the model being invoked).
    let identity = Identity {
        user: "alice".to_string(),
        groups: vec!["engineering".to_string()],
        organization: None,
        tenant: "acme-corp".to_string(),
    };
    let resource: Resource<'_> = [("model", "claude-sonnet-5-5")].into_iter().collect();

    // One bulk read of the caller's user/group/org/tenant flags, folded
    // most-restrictive. Missing flags mean no budget applies — allowed.
    match admission.check_admission(&identity, &resource).await? {
        AdmissionResult::Allowed => {
            println!("admit");
        }
        AdmissionResult::Over => {
            // Soft-overage policy: still admit, but signal over budget.
            println!("admit, over budget");
        }
        AdmissionResult::Denied => {
            println!("deny: budget exhausted");
            // e.g. return a 402/429 to the caller
        }
    }

    Ok(())
}
