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
//	sfu:room:<room> owner claim "<node>|<generation>", with a TTL the owner renews
type RedisStore struct {
	rdb *redis.Client
}

const (
	nodeKeyPrefix = "sfu:node:"
	nodesSetKey   = "sfu:nodes"
	roomKeyPrefix = "sfu:room:"
)

// claimScript sets the claim ARGV[1] (node ARGV[3]) if there is none, the
// owner's node key is gone, or the owner is the same node. ARGV[4] is the TTL
// in milliseconds.
var claimScript = redis.NewScript(`
local cur = redis.call('GET', KEYS[1])
if cur then
	local node = string.match(cur, '^(.*)|[^|]*$') or cur
	if node ~= ARGV[3] and redis.call('EXISTS', ARGV[2] .. node) == 1 then
		return cur
	end
end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[4])
return ARGV[1]`)

// renewScript extends the TTL only if the claim is still ARGV[1].
var renewScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0`)

// releaseScript deletes the claim only if it is still ARGV[1].
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

func (r *RedisStore) ClaimRoom(ctx context.Context, roomID string, claim Claim, ttl time.Duration) (Claim, error) {
	v, err := claimScript.Run(ctx, r.rdb, []string{roomKeyPrefix + roomID},
		claim.encode(), nodeKeyPrefix, claim.Node, ttl.Milliseconds()).Text()
	if err != nil {
		return Claim{}, err
	}
	return decodeClaim(v), nil
}

func (r *RedisStore) RoomOwner(ctx context.Context, roomID string) (Claim, error) {
	v, err := r.rdb.Get(ctx, roomKeyPrefix+roomID).Result()
	if errors.Is(err, redis.Nil) {
		return Claim{}, nil
	}
	if err != nil {
		return Claim{}, err
	}
	return decodeClaim(v), nil
}

func (r *RedisStore) RenewRoom(ctx context.Context, roomID string, claim Claim, ttl time.Duration) (bool, error) {
	n, err := renewScript.Run(ctx, r.rdb, []string{roomKeyPrefix + roomID}, claim.encode(), ttl.Milliseconds()).Int()
	return n == 1, err
}

func (r *RedisStore) ReleaseRoom(ctx context.Context, roomID string, claim Claim) error {
	return releaseScript.Run(ctx, r.rdb, []string{roomKeyPrefix + roomID}, claim.encode()).Err()
}

func (r *RedisStore) Close() error { return r.rdb.Close() }
