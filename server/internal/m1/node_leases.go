package m1

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"cordova/server/internal/domain"
	"cordova/server/internal/request"

	"github.com/redis/go-redis/v9"
)

const (
	maxNodeLeaseLifetime      = 120 * time.Second
	nodeCapacityProfileMaxAge = 24 * time.Hour
	maxReportedActiveLeases   = 4096
)

var (
	ErrNodeSlotUnavailable = errors.New("node has no verified available slot")
	ErrNodeNoCapacity      = errors.New("node has no free verified capacity")
)

type nodeCapacityProfile struct {
	NodeID                string `json:"node_id"`
	ModelID               string `json:"model_id"`
	CatalogVersion        string `json:"catalog_version"`
	ArtifactSHA256        string `json:"artifact_sha256"`
	BenchmarkID           string `json:"benchmark_id"`
	BenchmarkedAtUnix     int64  `json:"benchmarked_at_unix"`
	ExpiresAtUnix         int64  `json:"expires_at_unix"`
	BenchmarkPassed       bool   `json:"benchmark_passed"`
	VerifiedSlots         uint32 `json:"verified_slots"`
	VerifiedCapacityBytes uint64 `json:"verified_capacity_bytes"`
	Rank                  int    `json:"rank"`
	FirstTokenLatencyMS   uint64 `json:"first_token_latency_ms"`
}

type nodeLeaseRecord struct {
	NodeID       string `json:"node_id"`
	ModelID      string `json:"model_id"`
	SessionID    string `json:"session_id"`
	Generation   string `json:"generation"`
	AttemptID    string `json:"attempt_id"`
	LeaseID      string `json:"lease_id"`
	ExpiresAtMS  int64  `json:"expires_at_ms"`
	ReleasedAtMS int64  `json:"released_at_ms,omitempty"`
}

type nodeSlotLease struct {
	nodes        *nodeService
	nodeID       string
	modelID      string
	sessionID    string
	generation   uint64
	attemptID    string
	leaseID      string
}

func nodeLeaseIndexKey(nodeID string) string { return "cv:v1:node-leases:" + nodeID }
func nodeLeaseDetailsKey(nodeID string) string { return "cv:v1:node-lease-details:" + nodeID }
func nodeCapacityProfileKey(nodeID, modelID string) string {
	return "cv:v1:node-capacity:" + nodeID + ":" + modelID
}

func (s *nodeService) claimNodeSlot(ctx context.Context, nodeID, modelID, catalogVersion, artifactSHA256 string,
	requiredCapacityBytes uint64, attemptID, leaseID string, expiresAt time.Time) (*nodeSlotLease, error) {
	if ctx == nil || !validID(nodeID) || !validModelSlug(modelID) || !validID(catalogVersion) ||
		!validNodeHash(artifactSHA256) || requiredCapacityBytes == 0 || requiredCapacityBytes > (1<<53)-1 ||
		!validID(attemptID) || !validID(leaseID) {
		return nil, ErrInvalid
	}
	s.peerMu.RLock()
	peer := s.peers[nodeID]
	if peer != nil {
		identity, sessionID := peer.identity, peer.sessionID
		s.peerMu.RUnlock()
		if identity.Generation == 0 || identity.SessionID != sessionID {
			return nil, ErrNodeSessionUnavailable
		}
		expiresAt = expiresAt.UTC()
		now := time.Now().UTC()
		if !expiresAt.After(now) || expiresAt.After(now.Add(maxNodeLeaseLifetime)) {
			return nil, ErrInvalid
		}
		record := nodeLeaseRecord{NodeID: nodeID, ModelID: modelID, SessionID: sessionID,
			Generation: strconv.FormatUint(identity.Generation, 10), AttemptID: attemptID, LeaseID: leaseID,
			ExpiresAtMS: expiresAt.UnixMilli()}
		encoded, err := json.Marshal(record)
		if err != nil {
			return nil, err
		}
		result, err := s.redis.Eval(ctx, claimNodeSlotScript, []string{
			nodeSessionKey(nodeID), nodeRecordKey(nodeID), nodeHeartbeatKey(nodeID),
			nodeModelStateKey(nodeID, modelID), nodeCapacityProfileKey(nodeID, modelID),
			nodeLeaseIndexKey(nodeID), nodeLeaseDetailsKey(nodeID),
		}, sessionID, record.Generation, nodeID, identity.OwnerID, identity.SecretVersion,
			modelID, catalogVersion, artifactSHA256, strconv.FormatUint(requiredCapacityBytes, 10), attemptID, leaseID,
			record.ExpiresAtMS, string(encoded)).Int()
		if err != nil {
			return nil, err
		}
		switch result {
		case 1:
			return &nodeSlotLease{nodes: s, nodeID: nodeID, modelID: modelID, sessionID: sessionID,
				generation: identity.Generation, attemptID: attemptID, leaseID: leaseID}, nil
		case -1:
			return nil, ErrNodeConflict
		case -2:
			return nil, ErrNodeNoCapacity
		default:
			return nil, ErrNodeSlotUnavailable
		}
	}
	s.peerMu.RUnlock()
	return nil, ErrNodeSessionUnavailable
}

func (s *nodeService) acquireNodeInferenceAttempt(ctx context.Context, nodeID, modelID, catalogVersion,
	artifactSHA256 string, requiredCapacityBytes uint64, deadline time.Time) (request.Attempt, error) {
	if ctx == nil {
		return nil, ErrInvalid
	}
	if deadline.IsZero() {
		deadline = time.Now().UTC().Add(maxNodeLeaseLifetime)
	}
	if supplied, ok := ctx.Deadline(); ok && supplied.Before(deadline) {
		deadline = supplied
	}
	expiresAt := time.Now().UTC().Add(maxNodeLeaseLifetime)
	if deadline.Before(expiresAt) {
		expiresAt = deadline.UTC()
	}
	expiresAt = time.UnixMilli(expiresAt.UnixMilli()).UTC()
	attemptID, err := domain.NewID()
	if err != nil {
		return nil, err
	}
	leaseID, err := domain.NewID()
	if err != nil {
		return nil, err
	}
	lease, err := s.claimNodeSlot(ctx, nodeID, modelID, catalogVersion, artifactSHA256,
		requiredCapacityBytes, attemptID.String(), leaseID.String(), expiresAt)
	if err != nil {
		return nil, err
	}
	attempt, err := newNodeInferenceAttempt(s, nodeID, modelID, catalogVersion, attemptID.String(), leaseID.String(), lease.Release)
	if err != nil {
		lease.Release(context.Background())
		return nil, err
	}
	attempt.deadline = expiresAt
	return attempt, nil
}

func (s *nodeService) setVerifiedNodeCapacity(ctx context.Context, profile nodeCapacityProfile) error {
	if ctx == nil || !validID(profile.NodeID) || !validModelSlug(profile.ModelID) || !validID(profile.CatalogVersion) ||
		!validNodeHash(profile.ArtifactSHA256) || !validID(profile.BenchmarkID) || !profile.BenchmarkPassed ||
		profile.VerifiedSlots == 0 || profile.VerifiedSlots > 64 || profile.VerifiedCapacityBytes == 0 ||
		profile.VerifiedCapacityBytes > (1<<53)-1 || profile.Rank < 0 || profile.Rank > 100 || profile.FirstTokenLatencyMS == 0 ||
		profile.FirstTokenLatencyMS > 120_000 {
		return ErrInvalid
	}
	now := time.Now().Unix()
	if profile.BenchmarkedAtUnix <= 0 || profile.BenchmarkedAtUnix > now || profile.ExpiresAtUnix <= now ||
		profile.ExpiresAtUnix > profile.BenchmarkedAtUnix+int64(nodeCapacityProfileMaxAge.Seconds()) {
		return ErrInvalid
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		return err
	}
	result, err := s.redis.Eval(ctx, `
local node_type=redis.call('TYPE',KEYS[1]).ok
local profile_type=redis.call('TYPE',KEYS[2]).ok
if node_type~='string' or (profile_type~='none' and profile_type~='string') then return redis.error_reply('node capacity profile key type mismatch') end
local ok,node=pcall(cjson.decode,redis.call('GET',KEYS[1]))
if not ok or type(node)~='table' or node['id']~=ARGV[1] or node['revoked']~=false then return 0 end
if profile_type=='string' then
  local old_ok,old=pcall(cjson.decode,redis.call('GET',KEYS[2]))
  if not old_ok or type(old)~='table' then return redis.error_reply('node capacity profile is invalid') end
  if old['catalog_version']==ARGV[2] and old['artifact_sha256']==ARGV[3] and tonumber(old['benchmarked_at_unix'] or '0')>tonumber(ARGV[4]) then return -1 end
end
redis.call('SET',KEYS[2],ARGV[5],'EX',ARGV[6])
return 1`, []string{nodeRecordKey(profile.NodeID), nodeCapacityProfileKey(profile.NodeID, profile.ModelID)},
		profile.NodeID, profile.CatalogVersion, profile.ArtifactSHA256, profile.BenchmarkedAtUnix,
		string(encoded), profile.ExpiresAtUnix-now).Int()
	if err != nil {
		return err
	}
	if result == 1 {
		return nil
	}
	if result == -1 {
		return ErrNodeConflict
	}
	return ErrNodeUnauthorized
}

func (l *nodeSlotLease) Release(ctx context.Context) {
	if l == nil || l.nodes == nil {
		return
	}
	if ctx == nil || ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), nodeAttemptCleanupTimeout)
		defer cancel()
	}
	_, _ = l.nodes.redis.Eval(ctx, releaseNodeSlotScript, []string{
		nodeLeaseIndexKey(l.nodeID), nodeLeaseDetailsKey(l.nodeID),
	}, l.nodeID, l.modelID, l.sessionID, strconv.FormatUint(l.generation, 10),
		l.attemptID, l.leaseID).Int()
}

func (s *nodeService) verifyReportedNodeLeases(ctx context.Context, identity nodeIdentity, sessionID string, leaseIDs []string) error {
	if len(leaseIDs) > maxReportedActiveLeases {
		return ErrNodeUnauthorized
	}
	if len(leaseIDs) == 0 {
		return nil
	}
	arguments := make([]any, 0, 3+len(leaseIDs))
	arguments = append(arguments, identity.NodeID, sessionID, strconv.FormatUint(identity.Generation, 10))
	for _, leaseID := range leaseIDs {
		if !validID(leaseID) {
			return ErrNodeUnauthorized
		}
		arguments = append(arguments, leaseID)
	}
	result, err := s.redis.Eval(ctx, verifyReportedNodeLeasesScript, []string{
		nodeLeaseIndexKey(identity.NodeID), nodeLeaseDetailsKey(identity.NodeID),
	}, arguments...).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return ErrNodeConflict
	}
	return nil
}

func (s *nodeService) hasActiveNodeLeases(ctx context.Context, nodeID, sessionID string, generation uint64) (bool, error) {
	result, err := s.redis.Eval(ctx, hasActiveNodeLeasesScript, []string{
		nodeLeaseIndexKey(nodeID), nodeLeaseDetailsKey(nodeID),
	}, nodeID, sessionID, strconv.FormatUint(generation, 10)).Int()
	return result == 1, err
}

func (s *nodeService) activeNodeModelLeaseCount(ctx context.Context, nodeID, sessionID string, generation uint64, modelID string) (int64, error) {
	if ctx == nil || !validID(nodeID) || !validID(sessionID) || generation == 0 || !validModelSlug(modelID) {
		return 0, ErrInvalid
	}
	return s.redis.Eval(ctx, activeNodeModelLeaseCountScript, []string{
		nodeLeaseIndexKey(nodeID), nodeLeaseDetailsKey(nodeID),
	}, nodeID, sessionID, strconv.FormatUint(generation, 10), modelID).Int64()
}

const activeNodeModelLeaseCountScript = `
local leases_type=redis.call('TYPE',KEYS[1]).ok
local details_type=redis.call('TYPE',KEYS[2]).ok
if (leases_type~='none' and leases_type~='zset') or (details_type~='none' and details_type~='hash') then return redis.error_reply('node lease key type mismatch') end
local clock=redis.call('TIME')
local now_ms=tonumber(clock[1])*1000+math.floor(tonumber(clock[2])/1000)
local members=redis.call('ZRANGEBYSCORE',KEYS[1],now_ms+1,'+inf','WITHSCORES')
if #members>65536 then return redis.error_reply('node lease index exceeds safe bounds') end
local active=0
for i=1,#members,2 do
  local lease_id=members[i]
  local raw=redis.call('HGET',KEYS[2],lease_id)
  if not raw then return redis.error_reply('node lease index has no record') end
  local ok,record=pcall(cjson.decode,raw)
  if not ok or type(record)~='table' or record['lease_id']~=lease_id then return redis.error_reply('node lease record is invalid') end
  if record['node_id']==ARGV[1] and record['session_id']==ARGV[2] and record['generation']==ARGV[3] and
     record['model_id']==ARGV[4] and tonumber(record['released_at_ms'] or '0')==0 then active=active+1 end
end
return active`

const claimNodeSlotScript = `
local session_type=redis.call('TYPE',KEYS[1]).ok
local node_type=redis.call('TYPE',KEYS[2]).ok
local heartbeat_type=redis.call('TYPE',KEYS[3]).ok
local state_type=redis.call('TYPE',KEYS[4]).ok
local profile_type=redis.call('TYPE',KEYS[5]).ok
local leases_type=redis.call('TYPE',KEYS[6]).ok
local details_type=redis.call('TYPE',KEYS[7]).ok
if session_type~='string' or node_type~='string' or heartbeat_type~='string' or state_type~='string' or profile_type~='string' or
   (leases_type~='none' and leases_type~='zset') or (details_type~='none' and details_type~='hash') then return redis.error_reply('node lease key type mismatch') end
if redis.call('PTTL',KEYS[3])<=0 or redis.call('PTTL',KEYS[4])<=0 then return 0 end
if redis.call('GET',KEYS[1])~=ARGV[1]..':'..ARGV[2] then return -1 end
local node_ok,node=pcall(cjson.decode,redis.call('GET',KEYS[2]))
local state_ok,state=pcall(cjson.decode,redis.call('GET',KEYS[4]))
local profile_ok,profile=pcall(cjson.decode,redis.call('GET',KEYS[5]))
if not node_ok or type(node)~='table' or node['id']~=ARGV[3] or node['owner_id']~=ARGV[4] or
   node['secret_version']~=tonumber(ARGV[5]) or node['revoked']~=false or
   not state_ok or type(state)~='table' or state['node_id']~=ARGV[3] or state['session_id']~=ARGV[1] or
   type(state['model'])~='table' or state['model']['model_id']~=ARGV[6] or state['model']['state']~='loaded' or
   state['model']['artifact_sha256']~=ARGV[8] or
   not profile_ok or type(profile)~='table' or profile['node_id']~=ARGV[3] or profile['model_id']~=ARGV[6] or
   profile['catalog_version']~=ARGV[7] or profile['artifact_sha256']~=ARGV[8] or profile['benchmark_passed']~=true then return 0 end
local clock=redis.call('TIME')
local now_sec=tonumber(clock[1])
local now_ms=now_sec*1000+math.floor(tonumber(clock[2])/1000)
local lease_expiry=tonumber(ARGV[12])
local verified_slots=tonumber(profile['verified_slots'] or '0')
local capacity=tonumber(profile['verified_capacity_bytes'] or '0')
if tonumber(profile['expires_at_unix'] or '0')<=now_sec or verified_slots<1 or verified_slots>64 or
   capacity<tonumber(ARGV[9]) or lease_expiry<=now_ms or lease_expiry>now_ms+120000 then return 0 end
local members=redis.call('ZRANGE',KEYS[6],0,-1,'WITHSCORES')
if #members>32768 then return redis.error_reply('node lease index exceeds safe bounds') end
local expired={}
local active_for_model=0
local existing=redis.call('HGET',KEYS[7],ARGV[11])
if existing then
  local existing_ok,record=pcall(cjson.decode,existing)
  if not existing_ok or type(record)~='table' then return redis.error_reply('node lease record is invalid') end
  if record['node_id']~=ARGV[3] or record['model_id']~=ARGV[6] or record['session_id']~=ARGV[1] or
     record['generation']~=ARGV[2] or record['attempt_id']~=ARGV[10] or record['lease_id']~=ARGV[11] then return -1 end
  if tonumber(record['released_at_ms'] or '0')>0 then return -1 end
  local existing_score=redis.call('ZSCORE',KEYS[6],ARGV[11])
  if not existing_score then return redis.error_reply('node lease record has no expiry') end
  if tonumber(record['expires_at_ms'] or '0')>now_ms and tonumber(existing_score)>now_ms then return 1 end
  table.insert(expired,ARGV[11])
end
for i=1,#members,2 do
  local lease_id=members[i]
  local score=tonumber(members[i+1])
  if score<=now_ms then
    table.insert(expired,lease_id)
  else
    local raw=redis.call('HGET',KEYS[7],lease_id)
    if not raw then return redis.error_reply('node lease index has no record') end
    local record_ok,record=pcall(cjson.decode,raw)
    if not record_ok or type(record)~='table' or record['lease_id']~=lease_id then return redis.error_reply('node lease record is invalid') end
    if record['session_id']==ARGV[1] and record['generation']==ARGV[2] and record['model_id']==ARGV[6] then
      active_for_model=active_for_model+1
    end
  end
end
for _,lease_id in ipairs(expired) do redis.call('ZREM',KEYS[6],lease_id); redis.call('HDEL',KEYS[7],lease_id) end
if active_for_model>=verified_slots then return -2 end
if existing then return -2 end
redis.call('ZADD',KEYS[6],lease_expiry,ARGV[11])
redis.call('HSET',KEYS[7],ARGV[11],ARGV[13])
local last=redis.call('ZREVRANGE',KEYS[6],0,0,'WITHSCORES')
if #last==2 then
  local expire_at=tonumber(last[2])+60000
  redis.call('PEXPIREAT',KEYS[6],expire_at)
  redis.call('PEXPIREAT',KEYS[7],expire_at)
end
return 1`

const releaseNodeSlotScript = `
local leases_type=redis.call('TYPE',KEYS[1]).ok
local details_type=redis.call('TYPE',KEYS[2]).ok
if (leases_type~='none' and leases_type~='zset') or (details_type~='none' and details_type~='hash') then return redis.error_reply('node lease key type mismatch') end
local raw=redis.call('HGET',KEYS[2],ARGV[6])
if not raw then redis.call('ZREM',KEYS[1],ARGV[6]); return 0 end
local ok,record=pcall(cjson.decode,raw)
if not ok or type(record)~='table' then return redis.error_reply('node lease record is invalid') end
if record['node_id']~=ARGV[1] or record['model_id']~=ARGV[2] or record['session_id']~=ARGV[3] or
   record['generation']~=ARGV[4] or record['attempt_id']~=ARGV[5] or record['lease_id']~=ARGV[6] then return 0 end
if tonumber(record['released_at_ms'] or '0')>0 then return 0 end
redis.call('ZREM',KEYS[1],ARGV[6])
local clock=redis.call('TIME')
record['released_at_ms']=tonumber(clock[1])*1000+math.floor(tonumber(clock[2])/1000)
redis.call('HSET',KEYS[2],ARGV[6],cjson.encode(record))
if redis.call('PTTL',KEYS[2])<60000 then redis.call('PEXPIRE',KEYS[2],60000) end
return 1`

const verifyReportedNodeLeasesScript = `
local leases_type=redis.call('TYPE',KEYS[1]).ok
local details_type=redis.call('TYPE',KEYS[2]).ok
if (leases_type~='none' and leases_type~='zset') or (details_type~='none' and details_type~='hash') then return redis.error_reply('node lease key type mismatch') end
local clock=redis.call('TIME')
local now_ms=tonumber(clock[1])*1000+math.floor(tonumber(clock[2])/1000)
for i=4,#ARGV do
  local lease_id=ARGV[i]
  local raw=redis.call('HGET',KEYS[2],lease_id)
  local score=redis.call('ZSCORE',KEYS[1],lease_id)
  if not raw then return 0 end
  local ok,record=pcall(cjson.decode,raw)
  if not ok or type(record)~='table' or record['node_id']~=ARGV[1] or record['session_id']~=ARGV[2] or
     record['generation']~=ARGV[3] or record['lease_id']~=lease_id then return 0 end
  local released_at=tonumber(record['released_at_ms'] or '0')
  if score and tonumber(score)>now_ms then
    if released_at>0 then return 0 end
  elseif released_at==0 or now_ms-released_at>45000 then
    return 0
  end
end
return 1`

const hasActiveNodeLeasesScript = `
local leases_type=redis.call('TYPE',KEYS[1]).ok
local details_type=redis.call('TYPE',KEYS[2]).ok
if (leases_type~='none' and leases_type~='zset') or (details_type~='none' and details_type~='hash') then return redis.error_reply('node lease key type mismatch') end
local clock=redis.call('TIME')
local now_ms=tonumber(clock[1])*1000+math.floor(tonumber(clock[2])/1000)
local members=redis.call('ZRANGE',KEYS[1],0,-1,'WITHSCORES')
if #members>32768 then return redis.error_reply('node lease index exceeds safe bounds') end
for i=1,#members,2 do
  if tonumber(members[i+1])>now_ms then
    local raw=redis.call('HGET',KEYS[2],members[i])
    if not raw then return redis.error_reply('node lease index has no record') end
    local ok,record=pcall(cjson.decode,raw)
    if not ok or type(record)~='table' then return redis.error_reply('node lease record is invalid') end
    if record['node_id']==ARGV[1] and record['session_id']==ARGV[2] and record['generation']==ARGV[3] then return 1 end
  end
end
return 0`
