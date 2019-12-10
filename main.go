package main

import (
	"context"
	"io/ioutil"
	"runtime"

	"github.com/alecthomas/kingpin"
	"github.com/ericchiang/k8s"
	corev1 "github.com/ericchiang/k8s/apis/core/v1"
	foundation "github.com/estafette/estafette-foundation"
	"github.com/opentracing/opentracing-go"
	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v2"
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
	configPath                    = kingpin.Flag("config-path", "Path to config file").Default("/configs/config.yaml").OverrideDefaultFromEnvar("CONFIG_PATH").String()
	configmapName                 = kingpin.Flag("configmap-name", "Name of the configmap to update config in").Default("estafette-ci-log-migrator").OverrideDefaultFromEnvar("CONFIGMAP_NAME").String()
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

	// init kube client
	kubeClient, err := k8s.NewInClusterClient()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed creating Kubernetes API client")
	}

	// read config if it exists
	var config Config
	if foundation.FileExists(*configPath) {
		configData, err := ioutil.ReadFile(*configPath)
		if err != nil {
			log.Fatal().Err(err).Msgf("Failed reading config from %v", *configPath)
		}

		// unmarshal strict, so non-defined properties or incorrect nesting will fail
		if err := yaml.Unmarshal(configData, &config); err != nil {
			log.Fatal().Err(err).Msgf("Failed unmarshalling config from %v", *configPath)
		}
	}

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

		if foundation.StringArrayContains(config.FinishedPipelines, pl.GetFullRepoPath()) {
			// already finished, continue with next pipeline
			continue
		}

		err = apiClient.CopyLogsToCloudStorage(ctx, *pl)
		if err != nil {
			span.Finish()
			log.Fatal().Err(err).Msgf("Failed copying logs to cloud storage for pipeline %v", pl.GetFullRepoPath())
		}

		config.FinishedPipelines = append(config.FinishedPipelines, pl.GetFullRepoPath())

		writeConfigToConfigmap(kubeClient, config)
	}

	log.Info().Msg("Finished migrating logs to cloud storage")
}

func writeConfigToConfigmap(kubeClient *k8s.Client, config Config) {

	// retrieve configmap
	var configMap corev1.ConfigMap
	err := kubeClient.Get(context.Background(), kubeClient.Namespace, *configmapName, &configMap)
	if err != nil {
		log.Error().Err(err).Msgf("Failed retrieving configmap %v", *configmapName)
	}

	configData, err := yaml.Marshal(config)

	if configMap.Data == nil {
		configMap.Data = make(map[string]string)
	}

	configMap.Data["config.yaml"] = string(configData)

	// update configmap to have finished pipelines available when the application runs the next time
	err = kubeClient.Update(context.Background(), &configMap)
	if err != nil {
		log.Fatal().Err(err).Msgf("Failed updating configmap %v", *configmapName)
	}
}
