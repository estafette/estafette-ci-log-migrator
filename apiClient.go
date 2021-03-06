package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"sync"
	"time"

	contracts "github.com/estafette/estafette-ci-contracts"
	"github.com/opentracing-contrib/go-stdlib/nethttp"
	"github.com/opentracing/opentracing-go"
	"github.com/rs/zerolog/log"
	"github.com/sethgrid/pester"
)

// APIClient allows communication with the Estafette CI api for migrating logs
type APIClient interface {
	GetPipelines(ctx context.Context) (pipelines []*contracts.Pipeline, err error)
	CopyLogsToCloudStorage(ctx context.Context, pipeline contracts.Pipeline) (err error)
}

type apiClientImpl struct {
	apiURL                        string
	apiKey                        string
	pageSizeForPipelinesRetrieval int
	pageSizeForMigration          int
	pagesToMigrateInParallel      int
}

// NewAPIClient returns an instance for the APIClient interface
func NewAPIClient(apiURL, apiKey string, pageSizeForPipelinesRetrieval, pageSizeForMigration, pagesToMigrateInParallel int) (APIClient, error) {
	return &apiClientImpl{
		apiURL:                        apiURL,
		apiKey:                        apiKey,
		pageSizeForPipelinesRetrieval: pageSizeForPipelinesRetrieval,
		pageSizeForMigration:          pageSizeForMigration,
		pagesToMigrateInParallel:      pagesToMigrateInParallel,
	}, nil
}

func (impl *apiClientImpl) GetPipelines(ctx context.Context) (pipelines []*contracts.Pipeline, err error) {

	span, ctx := opentracing.StartSpanFromContext(ctx, "APIClient::GetPipelines")
	defer span.Finish()

	log.Info().Msg("Start retrieving pipelines...")

	pipelines = []*contracts.Pipeline{}

	pageNumber := 1
	pageSize := impl.pageSizeForPipelinesRetrieval

	for true {
		pl, err := impl.getPipelinesPerPage(ctx, pageNumber, pageSize)
		if err != nil {
			return pipelines, err
		}
		if len(pl) == 0 {
			break
		}

		pipelines = append(pipelines, pl...)
		pageNumber++
	}

	log.Info().Msg("Finished retrieving pipelines")

	return pipelines, nil
}

func (impl *apiClientImpl) getPipelinesPerPage(ctx context.Context, pageNumber, pageSize int) (pipelines []*contracts.Pipeline, err error) {

	span, ctx := opentracing.StartSpanFromContext(ctx, "APIClient::getPipelinesPerPage")
	span.SetTag("pageNumber", pageNumber)
	span.SetTag("pageSize", pageSize)
	defer span.Finish()

	pipelines = []*contracts.Pipeline{}

	getPipelinesURL := fmt.Sprintf("%v/api/pipelines?filter[status]=all&filter[since]=eternity&page[number]=%v&page[size]=%v", impl.apiURL, pageNumber, pageSize)

	body, err := impl.request(ctx, span, "GET", getPipelinesURL, []int{http.StatusOK})
	if err != nil {
		return
	}

	var listResponse struct {
		Pipelines []*contracts.Pipeline `json:"items"`
	}

	// unmarshal pipelines from body
	err = json.Unmarshal(body, &listResponse)
	if err != nil {
		log.Error().Err(err).Str("body", string(body)).Msg("Failed unmarshalling pipelines body")
		return
	}

	return listResponse.Pipelines, nil
}

func (impl *apiClientImpl) CopyLogsToCloudStorage(ctx context.Context, pipeline contracts.Pipeline) (err error) {
	span, ctx := opentracing.StartSpanFromContext(ctx, "APIClient::CopyLogsToCloudStorage")
	span.SetTag("pipeline", pipeline.GetFullRepoPath())
	defer span.Finish()

	log.Info().Msgf("Start copying logs to cloud storage for pipeline %v...", pipeline.GetFullRepoPath())

	// migrate build logs
	err = impl.copyLogsToCloudStorageInParallel(ctx, pipeline, impl.pageSizeForMigration, impl.pagesToMigrateInParallel, "builds")
	if err != nil {
		return err
	}

	// migrate releases logs
	err = impl.copyLogsToCloudStorageInParallel(ctx, pipeline, impl.pageSizeForMigration, impl.pagesToMigrateInParallel, "releases")
	if err != nil {
		return err
	}

	log.Info().Msgf("Finished copying logs to cloud storage for pipeline %v", pipeline.GetFullRepoPath())

	return nil
}

func (impl *apiClientImpl) copyLogsToCloudStorageInParallel(ctx context.Context, pipeline contracts.Pipeline, pageSize, parallelPageRuns int, jobType string) (err error) {

	span, ctx := opentracing.StartSpanFromContext(ctx, "APIClient::copyLogsToCloudStorageInParallel")
	span.SetTag("pipeline", pipeline.GetFullRepoPath())
	span.SetTag("pageSize", pageSize)
	span.SetTag("parallelPageRuns", parallelPageRuns)
	span.SetTag("jobType", jobType)
	defer span.Finish()

	// migrate build logs
	pageNumber := 1
	for true {

		copiedLogsCounts := make(chan int, parallelPageRuns)
		errors := make(chan error, parallelPageRuns)

		var wg sync.WaitGroup
		wg.Add(parallelPageRuns)

		for j := pageNumber; j < pageNumber+parallelPageRuns; j++ {
			go func(ctx context.Context, pipeline contracts.Pipeline, pageNumber, pageSize int, jobType string) {
				defer wg.Done()

				copiedLogsCount, err := impl.copyLogsToCloudStoragePerPage(ctx, pipeline, pageNumber, pageSize, jobType)
				if err != nil {
					errors <- err
				}

				copiedLogsCounts <- copiedLogsCount
			}(ctx, pipeline, j, pageSize, jobType)
		}

		// wait for all parallel runs to finish
		wg.Wait()

		// return error if any of them have been generated
		close(errors)
		for e := range errors {
			return e
		}

		// break loop if any of the calls have processed less logs than the page size
		close(copiedLogsCounts)
		for cl := range copiedLogsCounts {
			if cl < pageSize {
				return nil
			}
		}

		// otherwise continue with the next pages
		pageNumber += parallelPageRuns
	}

	return nil
}

func (impl *apiClientImpl) copyLogsToCloudStoragePerPage(ctx context.Context, pipeline contracts.Pipeline, pageNumber, pageSize int, jobType string) (copiedLogsCount int, err error) {

	span, ctx := opentracing.StartSpanFromContext(ctx, "APIClient::copyLogsToCloudStoragePerPage")
	span.SetTag("pipeline", pipeline.GetFullRepoPath())
	span.SetTag("pageNumber", pageNumber)
	span.SetTag("pageSize", pageSize)
	span.SetTag("jobType", jobType)
	defer span.Finish()

	copyLogsToCloudStorageURL := fmt.Sprintf("%v/api/copylogstocloudstorage/%v?page[number]=%v&page[size]=%v&filter[search]=%v", impl.apiURL, pipeline.GetFullRepoPath(), pageNumber, pageSize, jobType)

	body, err := impl.request(ctx, span, "GET", copyLogsToCloudStorageURL, []int{http.StatusOK})
	if err != nil {
		return
	}

	copiedLogsCount, err = strconv.Atoi(string(body))
	if err != nil {
		log.Error().Err(err).Str("body", string(body)).Msg("Failed reading int value from response")
		return
	}

	return copiedLogsCount, nil
}

func (impl *apiClientImpl) request(ctx context.Context, span opentracing.Span, method, url string, validStatusCodes []int) (body []byte, err error) {

	log.Debug().
		Str("method", method).
		Str("url", url).
		Msg("Handling request")

	// create client, in order to add headers
	client := pester.NewExtendedClient(&http.Client{Transport: &nethttp.Transport{}})
	client.MaxRetries = 5
	client.Backoff = pester.ExponentialJitterBackoff
	client.KeepLog = true
	client.Timeout = time.Second * 30

	request, err := http.NewRequest(method, url, nil)
	if err != nil {
		return
	}

	// add tracing context
	request = request.WithContext(opentracing.ContextWithSpan(ctx, span))

	// collect additional information on setting up connections
	request, ht := nethttp.TraceRequest(span.Tracer(), request)

	// add headers
	request.Header.Add("Authorization", fmt.Sprintf("Bearer %v", impl.apiKey))
	request.Header.Add("Content-Type", "application/json")

	// perform actual request
	response, err := client.Do(request)
	if err != nil {
		return
	}
	defer response.Body.Close()
	ht.Finish()

	// verify that status code is a valid one for this request
	hasValidStatusCode := false
	for _, sc := range validStatusCodes {
		if response.StatusCode == sc {
			hasValidStatusCode = true
		}
	}
	if !hasValidStatusCode {
		return body, fmt.Errorf("Status code %v for '%v %v' is not one of the valid status codes %v for this request. Body: %v", response.StatusCode, method, url, validStatusCodes, string(body))
	}

	body, err = ioutil.ReadAll(response.Body)
	if err != nil {
		return
	}

	return
}
