package redis

import (
	"context"
	"errors"

	"github.com/opendatahub-io/ai-metering-service/internal/store"
	"github.com/redis/go-redis/v9"
)

type RedisStore struct {
	client *redis.Client
}

func New(client *redis.Client) *RedisStore {
	return &RedisStore{client: client}
}

func (r *RedisStore) SetKey(ctx context.Context, tenantID, key string, value string) error {
	return r.client.Set(ctx, key, value, 0).Err()
}

func (r *RedisStore) GetKey(ctx context.Context, tenantID, key string) (string, error) {
	val, err := r.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", store.ErrKeyNotFound
	}
	return val, err
}

func (r *RedisStore) DeleteKey(ctx context.Context, tenantID, key string) error {
	return r.client.Del(ctx, key).Err()
}

func (r *RedisStore) DecrKey(ctx context.Context, tenantID, key string, value int64) (int64, error) {
	return r.client.DecrBy(ctx, key, value).Result()
}
