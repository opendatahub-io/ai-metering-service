use async_trait::async_trait;
use redis::aio::ConnectionManager;
use redis::AsyncCommands;

use super::{AdmissionError, KVStore};

impl From<redis::RedisError> for AdmissionError {
    fn from(error: redis::RedisError) -> Self {
        AdmissionError::BackendError(error.to_string())
    }
}

/// RedisKVStore reads admission flags over a single multiplexed connection
/// with automatic reconnection (with backoff) after a Redis restart or
/// failover. Failing in new means Redis was unreachable at construction;
/// later reads reuse this connection instead of paying a fresh handshake
/// per call.
pub struct RedisKVStore {
    conn: ConnectionManager,
}

impl RedisKVStore {
    pub async fn new(url: String) -> Result<Self, AdmissionError> {
        let client = redis::Client::open(url)?;
        Ok(Self {
            conn: ConnectionManager::new(client).await?,
        })
    }
}

#[async_trait]
impl KVStore for RedisKVStore {
    async fn get(&self, keys: &[String]) -> Result<Vec<Option<String>>, AdmissionError> {
        // ConnectionManager is a cheap Arc clone; commands take &mut self.
        let mut conn = self.conn.clone();
        Ok(conn.mget(keys).await?)
    }
}
