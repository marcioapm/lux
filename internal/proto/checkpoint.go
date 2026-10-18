package proto

const DefaultCheckpointBytes int64 = 512 << 20

// CheckpointRepo is a checkout to bundle as the workload user.
type CheckpointRepo struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Previous string `json:"previous,omitempty"`
}

type CheckpointArgs struct {
	Dir      string           `json:"dir"`
	Repos    []CheckpointRepo `json:"repos"`
	MaxBytes int64            `json:"maxBytes"`
}

// StagedCheckpoint names files relative to CheckpointArgs.Dir.
type StagedCheckpoint struct {
	Repo      string            `json:"repo"`
	Head      string            `json:"head,omitempty"`
	Branch    string            `json:"branch,omitempty"`
	Refs      map[string]string `json:"refs,omitempty"`
	Size      int64             `json:"size"`
	File      string            `json:"file,omitempty"`
	Unchanged bool              `json:"unchanged,omitempty"`
	Error     string            `json:"error,omitempty"`
}
