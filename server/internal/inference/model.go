package inference

import "context"

type ModelResidency string

const (
	ModelLoaded ModelResidency = "loaded"
	ModelOnDisk ModelResidency = "on_disk"
	ModelAbsent ModelResidency = "absent"
)

type RemoteModelStatus struct {
	State              ModelResidency
	ArtifactSHA256     string
	AvailableDiskBytes uint64
	ReasonCode         *string
}

type ModelController interface {
	Query(context.Context, string, string, string) (RemoteModelStatus, error)
	Load(context.Context, string, string, string) (artifactSHA256 string, err error)
	Download(context.Context, string, string, string) (artifactSHA256 string, err error)
}
