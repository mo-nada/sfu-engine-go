package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore is a Store in Redis or Valkey, shared by all nodes.
//
// Keys:
//
//	sfu:node:<id>   JSON NodeInfo, with the heartbeat TTL
//	sfu:nodes       set of node IDs (members whose key expired are pruned)
//	sfu:room:<room> owner node ID
type RedisStore struct {
	rdb *redis.Client
}

const (
	nodeKeyPrefix = "sfu:node:"
	nodesSetKey   = "sfu:nodes"
	roomKeyPrefix = "sfu:room:"
)

// claimScript sets the owner if there is none or the owner's node key is gone.
var claimScript = redis.NewScript(`
local cur = redis.call('GET', KEYS[1])
if (not cur) or redis.call('EXISTS', ARGV[2] .. cur) == 0 then
	redis.call('SET', KEYS[1], ARGV[1])
	return ARGV[1]
end
return cur`)

// releaseScript deletes the claim only if ARGV[1] owns it.
var releaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('DEL', KEYS[1])
end
return 0`)

// OpenRedis connects to url, for example "redis://localhost:6379/0".
func OpenRedis(ctx context.Context, url string) (*RedisStore, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	rdb := redis.NewClient(opts)
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("redis: %w", err)
	}
	return &RedisStore{rdb: rdb}, nil
}

func (r *RedisStore) PutNode(ctx context.Context, n NodeInfo, ttl time.Duration) error {
	data, err := json.Marshal(n)
	if err != nil {
		return err
	}
	pipe := r.rdb.TxPipeline()
	pipe.Set(ctx, nodeKeyPrefix+n.ID, data, ttl)
	pipe.SAdd(ctx, nodesSetKey, n.ID)
	_, err = pipe.Exec(ctx)
	return err
}

func (r *RedisStore) DeleteNode(ctx context.Context, id string) error {
	pipe := r.rdb.TxPipeline()
	pipe.Del(ctx, nodeKeyPrefix+id)
	pipe.SRem(ctx, nodesSetKey, id)
	_, err := pipe.Exec(ctx)
	return err
}

func (r *RedisStore) Nodes(ctx context.Context) ([]NodeInfo, error) {
	ids, err := r.rdb.SMembers(ctx, nodesSetKey).Result()
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = nodeKeyPrefix + id
	}
	vals, err := r.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	var out []NodeInfo
	for i, v := range vals {
		s, ok := v.(string)
		if !ok { // expired: prune from the set
			r.rdb.SRem(ctx, nodesSetKey, ids[i])
			continue
		}
		var n NodeInfo
		if json.Unmarshal([]byte(s), &n) == nil {
			out = append(out, n)
		}
	}
	return out, nil
}

func (r *RedisStore) ClaimRoom(ctx context.Context, roomID, nodeID string) (string, error) {
	return claimScript.Run(ctx, r.rdb, []string{roomKeyPrefix + roomID}, nodeID, nodeKeyPrefix).Text()
}

func (r *RedisStore) RoomOwner(ctx context.Context, roomID string) (string, error) {
	v, err := r.rdb.Get(ctx, roomKeyPrefix+roomID).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}

func (r *RedisStore) ReleaseRoom(ctx context.Context, roomID, nodeID string) error {
	return releaseScript.Run(ctx, r.rdb, []string{roomKeyPrefix + roomID}, nodeID).Err()
}

func (r *RedisStore) Close() error { return r.rdb.Close() }
