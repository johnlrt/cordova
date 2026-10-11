package m1

import (
	"context"
	"errors"
	"time"

	"cordova/server/internal/domain"
	"cordova/server/internal/inference"
)

var ErrNodeModelCommandRejected = errors.New("node rejected the model command")

type nodeModelController struct {
	nodes *nodeService
}

func newNodeModelController(nodes *nodeService) (*nodeModelController, error) {
	if nodes == nil {
		return nil, errors.New("node service is required")
	}
	return &nodeModelController{nodes: nodes}, nil
}

var _ inference.ModelController = (*nodeModelController)(nil)

func (c *nodeModelController) Query(ctx context.Context, nodeID, modelID, catalogVersion string) (inference.RemoteModelStatus, error) {
	return c.queryForSession(ctx, nodeID, modelID, catalogVersion, "")
}

func (c *nodeModelController) queryForSession(ctx context.Context, nodeID, modelID, catalogVersion, expectedSession string) (inference.RemoteModelStatus, error) {
	if ctx == nil || !validID(nodeID) || !validModelSlug(modelID) || !validID(catalogVersion) {
		return inference.RemoteModelStatus{}, errors.New("invalid model query")
	}
	operationCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	payload := nodeModelRefPayload{
		ModelID: modelID, CatalogVersion: catalogVersion,
	}
	var exchange *nodeExchange
	var err error
	if expectedSession == "" {
		exchange, err = c.nodes.beginNodeExchange(operationCtx, nodeID, "model.query", payload)
	} else {
		peer, ok := c.nodes.peerSnapshot(nodeID)
		if !ok || peer.sessionID != expectedSession || peer.identity.SessionID != expectedSession {
			return inference.RemoteModelStatus{}, ErrNodeSessionUnavailable
		}
		exchange, err = peer.begin(operationCtx, "model.query", payload)
	}
	if err != nil {
		return inference.RemoteModelStatus{}, err
	}
	defer exchange.Close()
	frame, err := exchange.Next(operationCtx)
	if err != nil {
		return inference.RemoteModelStatus{}, err
	}
	if frame.Type == "error" {
		return inference.RemoteModelStatus{}, ErrNodeModelCommandRejected
	}
	var state nodeModelStatePayload
	if frame.Type != "model.state" || decodeNodePayload(frame.Payload, &state,
		"model_id", "state", "artifact_sha256", "active_slots", "max_slots", "reason_code", "available_disk_bytes") != nil ||
		state.ModelID != modelID || !validNodeModelState(state) {
		return inference.RemoteModelStatus{}, errors.New("node returned invalid model state")
	}
	status := inference.RemoteModelStatus{State: inference.ModelResidency(state.State),
		AvailableDiskBytes: state.AvailableDiskBytes, ReasonCode: state.ReasonCode}
	if state.ArtifactSHA256 != nil {
		status.ArtifactSHA256 = *state.ArtifactSHA256
	}
	if err := c.nodes.recordNodeModelState(ctx, exchange.peer.identity, exchange.peer.sessionID, state, time.Now().UTC()); err != nil {
		return inference.RemoteModelStatus{}, err
	}
	return status, nil
}

func (c *nodeModelController) Load(ctx context.Context, nodeID, modelID, catalogVersion string) (string, error) {
	ready, err := c.runModelCommand(ctx, nodeID, modelID, catalogVersion, "model.load")
	return ready.ArtifactSHA256, err
}

func (c *nodeModelController) Download(ctx context.Context, nodeID, modelID, catalogVersion string) (string, error) {
	ready, err := c.runModelCommand(ctx, nodeID, modelID, catalogVersion, "model.download")
	return ready.ArtifactSHA256, err
}

type nodeModelReady struct {
	ModelID               string
	CommandID             string
	ArtifactSHA256        string
	TokenizerSHA256       string
	TemplateSHA256        string
	MaxSlots              uint32
	VerifiedCapacityBytes uint64
}

func (c *nodeModelController) loadVerified(ctx context.Context, nodeID, modelID, catalogVersion string) (nodeModelReady, error) {
	return c.runModelCommand(ctx, nodeID, modelID, catalogVersion, "model.load")
}

func (c *nodeModelController) loadVerifiedForSession(ctx context.Context, nodeID, modelID, catalogVersion, sessionID string) (nodeModelReady, error) {
	return c.runModelCommandForSession(ctx, nodeID, modelID, catalogVersion, "model.load", sessionID)
}

func (c *nodeModelController) downloadVerified(ctx context.Context, nodeID, modelID, catalogVersion string) (nodeModelReady, error) {
	return c.runModelCommand(ctx, nodeID, modelID, catalogVersion, "model.download")
}

func (c *nodeModelController) downloadVerifiedForSession(ctx context.Context, nodeID, modelID, catalogVersion, sessionID string) (nodeModelReady, error) {
	return c.runModelCommandForSession(ctx, nodeID, modelID, catalogVersion, "model.download", sessionID)
}

func (c *nodeModelController) runModelCommand(ctx context.Context, nodeID, modelID, catalogVersion, operation string) (nodeModelReady, error) {
	return c.runModelCommandForSession(ctx, nodeID, modelID, catalogVersion, operation, "")
}

func (c *nodeModelController) runModelCommandForSession(ctx context.Context, nodeID, modelID, catalogVersion, operation, expectedSession string) (nodeModelReady, error) {
	if ctx == nil || !validID(nodeID) || !validModelSlug(modelID) || !validID(catalogVersion) ||
		(operation != "model.load" && operation != "model.download") {
		return nodeModelReady{}, errors.New("invalid model command")
	}
	commandID, err := domain.NewID()
	if err != nil {
		return nodeModelReady{}, err
	}
	operationTimeout := 10 * time.Minute
	if operation == "model.load" {
		operationTimeout = 10 * time.Second
	}
	operationCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	deadline, ok := operationCtx.Deadline()
	if !ok {
		return nodeModelReady{}, errors.New("model command deadline is unavailable")
	}
	deadline = deadline.UTC()
	if !deadline.After(time.Now()) {
		return nodeModelReady{}, context.DeadlineExceeded
	}
	payload := map[string]any{
		"model_id": modelID, "catalog_version": catalogVersion, "command_id": commandID.String(),
		"deadline_at": deadline.Format(time.RFC3339Nano),
	}
	var exchange *nodeExchange
	if expectedSession == "" {
		exchange, err = c.nodes.beginNodeExchange(operationCtx, nodeID, operation, payload)
	} else {
		peer, ok := c.nodes.peerSnapshot(nodeID)
		if !ok || peer.sessionID != expectedSession || peer.identity.SessionID != expectedSession {
			return nodeModelReady{}, ErrNodeSessionUnavailable
		}
		exchange, err = peer.begin(operationCtx, operation, payload)
	}
	if err != nil {
		return nodeModelReady{}, err
	}
	defer exchange.Close()
	accepted := false
	for {
		frame, err := exchange.Next(operationCtx)
		if err != nil {
			return nodeModelReady{}, err
		}
		switch frame.Type {
		case "model.accepted":
			var response struct {
				ModelID   string `json:"model_id"`
				CommandID string `json:"command_id"`
				Operation string `json:"operation"`
			}
			if accepted || decodeNodePayload(frame.Payload, &response, "model_id", "command_id", "operation") != nil ||
				response.ModelID != modelID || response.CommandID != commandID.String() ||
				response.Operation != operation[len("model."):] {
				return nodeModelReady{}, errors.New("node returned invalid model acceptance")
			}
			accepted = true
		case "model.progress":
			if !accepted {
				return nodeModelReady{}, errors.New("node reported model progress before acceptance")
			}
		case "model.ready":
			var response struct {
				ModelID               string `json:"model_id"`
				CommandID             string `json:"command_id"`
				ArtifactSHA256        string `json:"artifact_sha256"`
				TokenizerSHA256       string `json:"tokenizer_sha256"`
				TemplateSHA256        string `json:"template_sha256"`
				MaxSlots              uint32 `json:"max_slots"`
				VerifiedCapacityBytes uint64 `json:"verified_capacity_bytes"`
			}
			if !accepted || decodeNodePayload(frame.Payload, &response,
				"model_id", "command_id", "artifact_sha256", "tokenizer_sha256", "template_sha256", "max_slots", "verified_capacity_bytes") != nil ||
				response.ModelID != modelID || response.CommandID != commandID.String() || !validNodeHash(response.ArtifactSHA256) ||
				!validNodeHash(response.TokenizerSHA256) || !validNodeHash(response.TemplateSHA256) ||
				response.MaxSlots == 0 || response.MaxSlots > 64 {
				return nodeModelReady{}, errors.New("node returned invalid model readiness")
			}
			return nodeModelReady{ModelID: response.ModelID, CommandID: response.CommandID,
				ArtifactSHA256: response.ArtifactSHA256, TokenizerSHA256: response.TokenizerSHA256,
				TemplateSHA256: response.TemplateSHA256, MaxSlots: response.MaxSlots,
				VerifiedCapacityBytes: response.VerifiedCapacityBytes}, nil
		case "model.reject", "error":
			return nodeModelReady{}, ErrNodeModelCommandRejected
		default:
			return nodeModelReady{}, errors.New("node returned an unexpected model response")
		}
	}
}
