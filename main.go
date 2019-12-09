package main

import (
	"context"
	"runtime"
	"sync"

	"github.com/alecthomas/kingpin"
	contracts "github.com/estafette/estafette-ci-contracts"
	foundation "github.com/estafette/estafette-foundation"
	"github.com/opentracing/opentracing-go"
	"github.com/rs/zerolog/log"
)

var (
	appgroup  string
	app       string
	version   string
	branch    string
	revision  string
	buildDate string
	goVersion = runtime.Version()

	apiKey = kingpin.Flag("api-key", "The Estafette server passes in this json structure to parameterize the build, set trusted images and inject credentials.").Envar("API_KEY").String()
	apiURL = kingpin.Flag("api-url", "The location of estafette-ci-api to communicate with").Envar("API_URL").String()
)

func main() {

	// parse command line parameters
	kingpin.Parse()

	// init log format from envvar ESTAFETTE_LOG_FORMAT
	foundation.InitLoggingFromEnv(foundation.NewApplicationInfo(appgroup, app, version, branch, revision, buildDate))

	// init context
	ctx := foundation.InitCancellationContext(context.Background())

	// init tracing
	closer := foundation.InitTracingFromEnv(app)
	defer closer.Close()

	// init span
	span := opentracing.StartSpan("MigrateLogs")
	defer span.Finish()

	ctx = opentracing.ContextWithSpan(ctx, span)

	apiClient, err := NewAPIClient(*apiURL, *apiKey)
	if err != nil {
		span.Finish()
		log.Fatal().Err(err).Msg("Failed initializing api client")
	}

	// get pipelines
	pipelines, err := apiClient.GetPipelines(ctx)
	if err != nil {
		span.Finish()
		log.Fatal().Err(err).Msg("Failed retrieving pipelines")
	}

	log.Info().Msgf("Retrieved %v pipelines", len(pipelines))

	parallelPipelineCount := 5
	startIndex := 0
	for true {
		endIndex := startIndex + parallelPipelineCount
		if startIndex > len(pipelines) {
			// we're done, exit loop
			break
		}
		if endIndex > len(pipelines) {
			// don't try to pick more than there's left
			endIndex = len(pipelines)
		}
		parallelPipelines := pipelines[startIndex:endIndex]

		var wg sync.WaitGroup
		wg.Add(len(parallelPipelines))

		for _, pl := range parallelPipelines {
			go func(ctx context.Context, pipeline contracts.Pipeline) {
				defer wg.Done()
				err = apiClient.CopyLogsToCloudStorage(ctx, pipeline)
				if err != nil {
					span.Finish()
					log.Fatal().Err(err).Msgf("Failed copying logs to cloud storage for pipeline %v", pl.GetFullRepoPath())
				}
			}(ctx, *pl)
		}

		wg.Wait()

		startIndex = startIndex + parallelPipelineCount
	}

	log.Info().Msg("Finished migrating logs to cloud storage")
}
