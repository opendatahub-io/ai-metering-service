
mod utils;
mod keys;
mod redis;
mod client;

pub use client::{AdmissionClient, AdmissionResult};
pub use redis::RedisKVStore;

use std::collections::HashMap;
use async_trait::async_trait;

pub struct Identity {
    pub user: String,
    pub groups: Vec<String>,
    pub organization: Option<String>,
    pub tenant: String,
}

/// One resource the caller wants to use, as label-like string pairs. The Go
/// service's api.UsageRecord.Resource has the same shape.
pub type Resource<'a> = HashMap<&'a str, &'a str>;

#[derive(Debug, thiserror::Error)]
pub enum AdmissionError {
    #[error("backend error: {0}")]
    BackendError(String),
}

#[async_trait]
pub trait KVStore {
    async fn get(&self, keys: &[String]) -> Result<Vec<Option<String>>, AdmissionError>;
}
