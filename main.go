package main

import (
	"context"
	"runtime"

	"github.com/alecthomas/kingpin"
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

	apiKey                        = kingpin.Flag("api-key", "The Estafette server passes in this json structure to parameterize the build, set trusted images and inject credentials.").Envar("API_KEY").String()
	apiURL                        = kingpin.Flag("api-url", "The location of estafette-ci-api to communicate with").Envar("API_URL").String()
	pageSizeForPipelinesRetrieval = kingpin.Flag("page-size-for-pipelines-retrieval", "Page size for retrieving pipelines from api").Default("10").OverrideDefaultFromEnvar("PAGE_SIZE_FOR_PIPELINES_RETRIEVAL").Int()
	pageSizeForMigration          = kingpin.Flag("page-size-for-migration", "Page size for migrating logs to cloud storage via api").Default("5").OverrideDefaultFromEnvar("PAGE_SIZE_FOR_MIGRATION").Int()
	pagesToMigrateInParallel      = kingpin.Flag("pages-to-migrate-in-parallel", "Number of pages to migrate in parallel via api").Default("2").OverrideDefaultFromEnvar("PAGES_TO_MIGRATE_IN_PARALLEL").Int()
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

	apiClient, err := NewAPIClient(*apiURL, *apiKey, *pageSizeForPipelinesRetrieval, *pageSizeForMigration, *pagesToMigrateInParallel)
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

	for _, pl := range pipelines {
		err = apiClient.CopyLogsToCloudStorage(ctx, *pl)
		if err != nil {
			span.Finish()
			log.Fatal().Err(err).Msgf("Failed copying logs to cloud storage for pipeline %v", pl.GetFullRepoPath())
		}
	}

	log.Info().Msg("Finished migrating logs to cloud storage")
}
