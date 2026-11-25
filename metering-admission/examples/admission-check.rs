//! CLI harness for the cross-language e2e suite: reads the admission flags
//! the Go metering service wrote to Redis and prints the folded decision
//! ("allow", "over" or "deny"). The Go e2e suite (e2e-tests/crate_test.go)
//! builds and invokes this binary, asserting its output — so any drift
//! between the Go service's key computation and the crate's fails the suite.
//!
//! Usage:
//!
//! ```sh
//! cargo run --example admission-check -- \
//!   --url redis://localhost:6379 \
//!   --tenant acme-corp \
//!   [--user alice] [--group engineering]... [--org research] \
//!   --resource model=claude-sonnet-5-5 [--resource provider=anthropic]
//! ```

use std::collections::HashMap;

use metering_admission::{AdmissionClient, AdmissionResult, Identity, Resource};

fn next_value(args: &[String], i: &mut usize, flag: &str) -> Result<String, String> {
    *i += 1;
    args.get(*i)
        .cloned()
        .ok_or_else(|| format!("missing value for {flag}"))
}

#[tokio::main]
async fn main() -> Result<(), String> {
    let mut url = None;
    let mut user = String::new();
    let mut groups: Vec<String> = Vec::new();
    let mut organization = None;
    let mut tenant = String::new();
    let mut resource: HashMap<String, String> = HashMap::new();

    let args: Vec<String> = std::env::args().skip(1).collect();
    let mut i = 0;
    while i < args.len() {
        // i points at the flag; next_value advances it onto the value, and
        // the increment below moves past it.
        let flag = args[i].clone();
        match flag.as_str() {
            "--url" => url = Some(next_value(&args, &mut i, &flag)?),
            "--tenant" => tenant = next_value(&args, &mut i, &flag)?,
            "--user" => user = next_value(&args, &mut i, &flag)?,
            "--group" => groups.push(next_value(&args, &mut i, &flag)?),
            "--org" => organization = Some(next_value(&args, &mut i, &flag)?),
            "--resource" => {
                let pair = next_value(&args, &mut i, &flag)?;
                let (key, value) = pair
                    .split_once('=')
                    .ok_or_else(|| format!("--resource expects k=v, got {pair}"))?;
                resource.insert(key.to_string(), value.to_string());
            }
            other => return Err(format!("unknown argument {other}")),
        }
        i += 1;
    }

    let url = url.ok_or("--url is required")?;
    if tenant.is_empty() {
        return Err("--tenant is required".to_string());
    }
    if resource.is_empty() {
        return Err("at least one --resource k=v is required".to_string());
    }

    let identity = Identity {
        user,
        groups,
        organization,
        tenant,
    };
    let resource: Resource<'_> = resource
        .iter()
        .map(|(k, v)| (k.as_str(), v.as_str()))
        .collect();

    let client = AdmissionClient::new(url).await.map_err(|e| e.to_string())?;
    let result = client
        .check_admission(&identity, &resource)
        .await
        .map_err(|e| e.to_string())?;

    println!(
        "{}",
        match result {
            AdmissionResult::Allowed => "allow",
            AdmissionResult::Over => "over",
            AdmissionResult::Denied => "deny",
        }
    );
    Ok(())
}
