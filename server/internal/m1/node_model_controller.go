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
	if ctx == nil || !validID(nodeID) || !validModelSlug(modelID) || !validID(catalogVersion) {
		return inference.RemoteModelStatus{}, errors.New("invalid model query")
	}
	operationCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	exchange, err := c.nodes.beginNodeExchange(operationCtx, nodeID, "model.query", nodeModelRefPayload{
		ModelID: modelID, CatalogVersion: catalogVersion,
	})
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
		"model_id", "state", "artifact_sha256", "active_slots", "max_slots", "reason_code") != nil ||
		state.ModelID != modelID || !validNodeModelState(state) {
		return inference.RemoteModelStatus{}, errors.New("node returned invalid model state")
	}
	status := inference.RemoteModelStatus{State: inference.ModelResidency(state.State)}
	if state.ArtifactSHA256 != nil {
		status.ArtifactSHA256 = *state.ArtifactSHA256
	}
	return status, nil
}

func (c *nodeModelController) Load(ctx context.Context, nodeID, modelID, catalogVersion string) (string, error) {
	return c.runModelCommand(ctx, nodeID, modelID, catalogVersion, "model.load")
}

func (c *nodeModelController) Download(ctx context.Context, nodeID, modelID, catalogVersion string) (string, error) {
	return c.runModelCommand(ctx, nodeID, modelID, catalogVersion, "model.download")
}

func (c *nodeModelController) runModelCommand(ctx context.Context, nodeID, modelID, catalogVersion, operation string) (string, error) {
	if ctx == nil || !validID(nodeID) || !validModelSlug(modelID) || !validID(catalogVersion) ||
		(operation != "model.load" && operation != "model.download") {
		return "", errors.New("invalid model command")
	}
	commandID, err := domain.NewID()
	if err != nil {
		return "", err
	}
	operationTimeout := 10 * time.Minute
	if operation == "model.load" {
		operationTimeout = 10 * time.Second
	}
	operationCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	deadline, ok := operationCtx.Deadline()
	if !ok {
		return "", errors.New("model command deadline is unavailable")
	}
	deadline = deadline.UTC()
	if !deadline.After(time.Now()) {
		return "", context.DeadlineExceeded
	}
	exchange, err := c.nodes.beginNodeExchange(operationCtx, nodeID, operation, map[string]any{
		"model_id": modelID, "catalog_version": catalogVersion, "command_id": commandID.String(),
		"deadline_at": deadline.Format(time.RFC3339Nano),
	})
	if err != nil {
		return "", err
	}
	defer exchange.Close()
	accepted := false
	for {
		frame, err := exchange.Next(operationCtx)
		if err != nil {
			return "", err
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
				return "", errors.New("node returned invalid model acceptance")
			}
			accepted = true
		case "model.progress":
			if !accepted {
				return "", errors.New("node reported model progress before acceptance")
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
				return "", errors.New("node returned invalid model readiness")
			}
			return response.ArtifactSHA256, nil
		case "model.reject", "error":
			return "", ErrNodeModelCommandRejected
		default:
			return "", errors.New("node returned an unexpected model response")
		}
	}
}
