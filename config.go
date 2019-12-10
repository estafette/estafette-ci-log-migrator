package main

// Config is used to know which pipelines have already been processed and can be skipped on a restart
type Config struct {
	FinishedPipelines []string `json:"finishedPipelines,omitempty" yaml:"finishedPipelines,omitempty"`
}
