package m1

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const maxNodePageSize = 100

type nodeInventoryPage struct {
	Data       []nodeView `json:"data"`
	NextCursor *string    `json:"next_cursor"`
}

type nodeView struct {
	ID                        string            `json:"id"`
	Name                      string            `json:"name"`
	Status                    string            `json:"status"`
	Rank                      *int              `json:"rank"`
	RankUpdatedAt             *string           `json:"rank_updated_at"`
	Hardware                  *nodeHardware     `json:"hardware"`
	VerifiedCapacityBytes     *uint64           `json:"verified_capacity_bytes"`
	MeasuredTokensPerSecond   *float64          `json:"measured_tokens_per_second"`
	LatencyMS                 *uint64           `json:"latency_ms"`
	UptimeRatio               *float64          `json:"uptime_ratio"`
	SuccessRatio              *float64          `json:"success_ratio"`
	LastSeenAt                *string           `json:"last_seen_at"`
	Models                    []nodeModelReport `json:"models"`
	EarningsPendingPiconero   *string           `json:"earnings_pending_piconero"`
	EarningsAvailablePiconero *string           `json:"earnings_available_piconero"`
	HoldUntil                 *string           `json:"hold_until"`
}

func nodeOwnerIndexKey(accountID string) string      { return "cv:v1:account-nodes:" + accountID }
func nodeOwnerIndexReadyKey(accountID string) string { return "cv:v1:account-nodes-ready:" + accountID }

func nodeIndexMember(record nodeRecord) (string, error) {
	created, err := time.Parse(time.RFC3339Nano, record.CreatedAt)
	if err != nil || !validID(record.ID) || created.UnixNano() < 0 {
		return "", ErrInvalid
	}
	return fmt.Sprintf("%019d|%s", created.UnixNano(), record.ID), nil
}

func nodeIDFromIndexMember(member string) (string, bool) {
	parts := strings.Split(member, "|")
	if len(parts) != 2 || len(parts[0]) != 19 || !validID(parts[1]) {
		return "", false
	}
	if _, err := strconv.ParseInt(parts[0], 10, 64); err != nil {
		return "", false
	}
	return parts[1], true
}

func (s *nodeService) encodeNodeCursor(accountID, member string) string {
	position := base64.RawURLEncoding.EncodeToString([]byte(member))
	signature := s.verifier("node-cursor-v1\n"+accountID, member)
	return position + "." + signature
}

func (s *nodeService) decodeNodeCursor(accountID, cursor string) (string, error) {
	parts := strings.Split(cursor, ".")
	if len(parts) != 2 || len(cursor) > 512 {
		return "", ErrInvalid
	}
	encoded, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", ErrInvalid
	}
	member := string(encoded)
	if _, ok := nodeIDFromIndexMember(member); !ok || !hmacEqual(parts[1], s.verifier("node-cursor-v1\n"+accountID, member)) {
		return "", ErrInvalid
	}
	return member, nil
}

func (s *nodeService) ensureNodeIndex(ctx context.Context, accountID string) error {
	ready, err := s.redis.Exists(ctx, nodeOwnerIndexReadyKey(accountID)).Result()
	if err != nil || ready != 0 {
		return err
	}
	var cursor uint64
	for {
		keys, next, err := s.redis.Scan(ctx, cursor, "cv:v1:node:*", 200).Result()
		if err != nil {
			return err
		}
		for _, key := range keys {
			id := strings.TrimPrefix(key, "cv:v1:node:")
			if !validID(id) {
				continue
			}
			record, err := s.getNode(ctx, id)
			if errors.Is(err, ErrNodeUnauthorized) {
				continue
			}
			if err != nil {
				return err
			}
			if record.OwnerID != accountID || record.Revoked {
				continue
			}
			member, err := nodeIndexMember(record)
			if err != nil {
				continue
			}
			if err := s.redis.ZAdd(ctx, nodeOwnerIndexKey(accountID), redis.Z{Score: 0, Member: member}).Err(); err != nil {
				return err
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return s.redis.Set(ctx, nodeOwnerIndexReadyKey(accountID), "1", 0).Err()
}

func (s *nodeService) listNodes(ctx context.Context, accountID string, limit int, cursor string) (nodeInventoryPage, error) {
	if !validID(accountID) || limit < 1 || limit > maxNodePageSize {
		return nodeInventoryPage{}, ErrInvalid
	}
	if err := s.ensureNodeIndex(ctx, accountID); err != nil {
		return nodeInventoryPage{}, err
	}
	max := "+"
	if cursor != "" {
		member, err := s.decodeNodeCursor(accountID, cursor)
		if err != nil {
			return nodeInventoryPage{}, err
		}
		max = "(" + member
	}
	members, err := s.redis.ZRevRangeByLex(ctx, nodeOwnerIndexKey(accountID), &redis.ZRangeBy{
		Max: max, Min: "-", Offset: 0, Count: int64(limit + 1),
	}).Result()
	if err != nil {
		return nodeInventoryPage{}, err
	}
	hasMore := len(members) > limit
	if hasMore {
		members = members[:limit]
	}
	page := nodeInventoryPage{Data: make([]nodeView, 0, len(members))}
	for _, member := range members {
		id, ok := nodeIDFromIndexMember(member)
		if !ok {
			return nodeInventoryPage{}, errors.New("node owner index is malformed")
		}
		record, err := s.getNode(ctx, id)
		if errors.Is(err, ErrNodeUnauthorized) {
			continue
		}
		if err != nil {
			return nodeInventoryPage{}, err
		}
		if record.OwnerID != accountID || record.Revoked {
			continue
		}
		view, err := s.nodeView(ctx, record)
		if err != nil {
			return nodeInventoryPage{}, err
		}
		page.Data = append(page.Data, view)
	}
	if hasMore && len(members) > 0 {
		next := s.encodeNodeCursor(accountID, members[len(members)-1])
		page.NextCursor = &next
	}
	return page, nil
}

func (s *nodeService) getNodeForOwner(ctx context.Context, accountID, nodeID string) (nodeView, error) {
	if !validID(accountID) || !validID(nodeID) {
		return nodeView{}, ErrNotFound
	}
	record, err := s.getNode(ctx, nodeID)
	if errors.Is(err, ErrNodeUnauthorized) {
		return nodeView{}, ErrNotFound
	}
	if err != nil {
		return nodeView{}, err
	}
	if record.OwnerID != accountID || record.Revoked {
		return nodeView{}, ErrNotFound
	}
	return s.nodeView(ctx, record)
}

func (s *nodeService) nodeView(ctx context.Context, record nodeRecord) (nodeView, error) {
	view := nodeView{ID: record.ID, Name: record.Name, Status: "offline", Hardware: record.Hardware,
		Models: make([]nodeModelReport, 0)}
	if record.LastSeenAt != "" {
		lastSeen := record.LastSeenAt
		view.LastSeenAt = &lastSeen
	}
	heartbeatData, err := s.redis.Get(ctx, nodeHeartbeatKey(record.ID)).Bytes()
	if err != nil && err != redis.Nil {
		return nodeView{}, err
	}
	if err == nil {
		var heartbeat nodeSessionHeartbeat
		if decodeStrictBytes(heartbeatData, &heartbeat) != nil || heartbeat.NodeID != record.ID {
			return nodeView{}, errors.New("node heartbeat record is invalid")
		}
		view.Models = append(view.Models, heartbeat.Payload.Models...)
		view.Status = "online"
		if len(heartbeat.Payload.ActiveLeases) > 0 {
			view.Status = "busy"
		}
		for _, model := range heartbeat.Payload.Models {
			if model.State == "loading" || model.State == "downloading" {
				view.Status = "warming"
				break
			}
		}
	} else {
		approved, parseErr := time.Parse(time.RFC3339Nano, record.ApprovedAt)
		if parseErr == nil && time.Since(approved) >= 0 && time.Since(approved) <= nodeHeartbeatLimit {
			view.Status = "enrolling"
		}
	}
	return view, nil
}

func (s *nodeService) renameNode(ctx context.Context, accountID, nodeID, name string) (nodeView, error) {
	if !validID(accountID) || !validID(nodeID) || !validNodeText(name, 64) {
		return nodeView{}, ErrInvalid
	}
	result, err := s.redis.Eval(ctx, `
local node_type=redis.call('TYPE',KEYS[1]).ok
if node_type=='none' then return 0 end
if node_type~='string' then return redis.error_reply('node record key type mismatch') end
local ok,node=pcall(cjson.decode,redis.call('GET',KEYS[1]))
if not ok or type(node)~='table' or node['id']~=ARGV[1] or node['owner_id']~=ARGV[2] or node['revoked']~=false then return 0 end
node['name']=ARGV[3]
redis.call('SET',KEYS[1],cjson.encode(node))
return 1`, []string{nodeRecordKey(nodeID)}, nodeID, accountID, name).Int()
	if err != nil {
		return nodeView{}, err
	}
	if result != 1 {
		return nodeView{}, ErrNotFound
	}
	return s.getNodeForOwner(ctx, accountID, nodeID)
}

func (s *nodeService) revokeNode(ctx context.Context, accountID, nodeID string) error {
	if !validID(accountID) || !validID(nodeID) {
		return ErrNotFound
	}
	record, err := s.getNode(ctx, nodeID)
	if errors.Is(err, ErrNodeUnauthorized) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if record.OwnerID != accountID {
		return ErrNotFound
	}
	member, err := nodeIndexMember(record)
	if err != nil {
		return err
	}
	result, err := s.redis.Eval(ctx, `
local node_type=redis.call('TYPE',KEYS[1]).ok
local index_type=redis.call('TYPE',KEYS[4]).ok
if node_type=='none' then return 0 end
if node_type~='string' or (index_type~='none' and index_type~='zset') then return redis.error_reply('node revoke key type mismatch') end
local ok,node=pcall(cjson.decode,redis.call('GET',KEYS[1]))
if not ok or type(node)~='table' or node['id']~=ARGV[1] or node['owner_id']~=ARGV[2] then return 0 end
if node['revoked']~=true then
  node['revoked']=true
  node['secret_version']=(tonumber(node['secret_version']) or 0)+1
  redis.call('SET',KEYS[1],cjson.encode(node))
end
redis.call('DEL',KEYS[2],KEYS[3])
redis.call('ZREM',KEYS[4],ARGV[3])
return 1`, []string{nodeRecordKey(nodeID), nodeSessionKey(nodeID), nodeHeartbeatKey(nodeID),
		nodeOwnerIndexKey(accountID)}, nodeID, accountID, member).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return ErrNotFound
	}
	s.peerMu.Lock()
	peer := s.peers[nodeID]
	delete(s.peers, nodeID)
	s.peerMu.Unlock()
	if peer != nil {
		peer.close()
		nodeClose(peer.conn, 4001, "node revoked")
		_ = peer.conn.Close()
	}
	return nil
}
