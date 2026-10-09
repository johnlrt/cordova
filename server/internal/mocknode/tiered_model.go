package mocknode

import (
	"context"
	"errors"
	"time"

	requestcoord "cordova/server/internal/request"
)

func (r *TieredRequestRouter) loadAndClaim(ctx context.Context, node *tieredNode, deadline time.Time) (requestcoord.Attempt, error) {
	r.mu.Lock()
	if r.closed || node.loading || node.warming || !r.baseEligible(node.status, r.now()) ||
		node.status.ModelState != ModelOnDisk || len(node.active) != 0 {
		r.mu.Unlock()
		return nil, requestcoord.ErrNoCapacity
	}
	node.loading = true
	node.loadToken++
	loadToken := node.loadToken
	generation, sessionID := node.status.Generation, node.status.SessionID
	if boundedDeadline := r.now().Add(modelLoadDeadline); boundedDeadline.Before(deadline) {
		deadline = boundedDeadline
	}
	loadCtx, cancel := context.WithDeadline(ctx, deadline)
	node.loadCancel = cancel
	nodeID := node.status.NodeID
	r.mu.Unlock()

	remote, err := r.controller.Query(loadCtx, nodeID, r.modelID, r.catalogVersion)
	remoteAbsent := false
	if err == nil && remote.State != ModelAbsent && remote.ArtifactSHA256 != r.artifactSHA256 {
		err = errors.New("remote model hash differs from the verified catalog")
	}
	if err == nil {
		switch remote.State {
		case ModelLoaded:
		case ModelOnDisk:
			var hash string
			hash, err = r.controller.Load(loadCtx, nodeID, r.modelID, r.catalogVersion)
			if err == nil && hash != r.artifactSHA256 {
				err = errors.New("loaded model hash differs from the verified catalog")
			}
		case ModelAbsent:
			remoteAbsent = true
			err = requestcoord.ErrModelWarming
		default:
			err = errors.New("node returned an unsupported model state")
		}
	}
	if err == nil && loadCtx.Err() != nil {
		err = loadCtx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil && deadline.After(r.now()) {
		err = requestcoord.ErrModelWarming
	}
	cancel()

	r.mu.Lock()
	if node.loadToken == loadToken {
		node.loading = false
		node.loadCancel = nil
	}
	if r.closed || node.loadToken != loadToken || node.status.Generation != generation || node.status.SessionID != sessionID ||
		!r.baseEligible(node.status, r.now()) {
		r.mu.Unlock()
		return nil, requestcoord.ErrNoCapacity
	}
	if errors.Is(err, requestcoord.ErrModelWarming) {
		var jobs []warmJob
		if remoteAbsent && node.status.AvailableDiskBytes >= r.requiredArtifactBytes {
			node.status.ModelState = ModelAbsent
			node.status.ArtifactSHA256 = ""
			jobs = r.markWarmLocked([]*tieredNode{node})
		} else if remoteAbsent {
			err = requestcoord.ErrNoCapacity
		}
		r.mu.Unlock()
		for _, job := range jobs {
			go r.warmNode(job.node, job.generation, job.sessionID, job.token)
		}
		return nil, err
	}
	if err != nil {
		r.mu.Unlock()
		return nil, requestcoord.ErrNoCapacity
	}
	node.status.ModelState = ModelLoaded
	node.status.ArtifactSHA256 = r.artifactSHA256
	attempt := r.claimLocked(node)
	r.mu.Unlock()
	return attempt, nil
}

type warmJob struct {
	node       *tieredNode
	generation uint64
	sessionID  string
	token      uint64
}

func (r *TieredRequestRouter) markWarmLocked(nodes []*tieredNode) []warmJob {
	if r.controller == nil {
		return nil
	}
	jobs := make([]warmJob, 0, len(nodes))
	for _, node := range nodes {
		if node.warming || node.loading || len(node.active) != 0 {
			continue
		}
		node.warming = true
		node.warmToken++
		jobs = append(jobs, warmJob{node: node, generation: node.status.Generation, sessionID: node.status.SessionID, token: node.warmToken})
	}
	return jobs
}

func (r *TieredRequestRouter) warmNode(node *tieredNode, generation uint64, sessionID string, token uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), modelWarmDeadline)
	defer cancel()
	r.mu.Lock()
	if r.closed || node.warmToken != token || node.status.Generation != generation || node.status.SessionID != sessionID || !node.status.Online {
		r.mu.Unlock()
		return
	}
	node.warmCancel = cancel
	nodeID := node.status.NodeID
	r.mu.Unlock()

	hash, err := r.controller.Download(ctx, nodeID, r.modelID, r.catalogVersion)
	if err == nil && hash != r.artifactSHA256 {
		err = errors.New("downloaded model hash differs from the verified catalog")
	}
	if err == nil {
		hash, err = r.controller.Load(ctx, nodeID, r.modelID, r.catalogVersion)
		if err == nil && hash != r.artifactSHA256 {
			err = errors.New("loaded model hash differs from the verified catalog")
		}
	}
	r.mu.Lock()
	if node.warmToken == token {
		node.warming = false
		node.warmCancel = nil
		if err == nil && !r.closed && node.status.Generation == generation && node.status.SessionID == sessionID && node.status.Online {
			node.status.ModelState = ModelLoaded
			node.status.ArtifactSHA256 = r.artifactSHA256
		}
	}
	r.mu.Unlock()
}
