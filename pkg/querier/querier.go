package querier

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/weaveworks/common/user"
	"google.golang.org/grpc/health/grpc_health_v1"

	cortex_client "github.com/cortexproject/cortex/pkg/ingester/client"
	"github.com/cortexproject/cortex/pkg/ring"
	"github.com/cortexproject/cortex/pkg/util"

	"github.com/grafana/frigg/pkg/friggpb"
	"github.com/grafana/frigg/pkg/ingester/client"
	"github.com/grafana/frigg/pkg/storage"
	frigg_util "github.com/grafana/frigg/pkg/util"
	"github.com/grafana/frigg/pkg/util/validation"
)

var (
	readinessProbeSuccess = []byte("Ready")

	metricBloomFilterReads = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "frigg",
		Name:      "bloom_filter_reads",
		Help:      "count of bloom filters read",
		Buckets:   prometheus.ExponentialBuckets(0.5, 2, 10),
	})
	metricBloomFilterBytesRead = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "frigg",
		Name:      "bloom_filter_bytes_read",
		Help:      "bytes of bloom filters read",
		Buckets:   prometheus.ExponentialBuckets(512, 2, 10),
	})
	metricIndexReads = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "frigg",
		Name:      "index_reads",
		Help:      "count of indexes read",
		Buckets:   prometheus.ExponentialBuckets(0.5, 2, 10),
	})
	metricIndexBytesRead = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "frigg",
		Name:      "index_bytes_read",
		Help:      "bytes of indexes read",
		Buckets:   prometheus.ExponentialBuckets(512, 2, 10),
	})
	metricBlockReads = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "frigg",
		Name:      "block_reads",
		Help:      "count of blocks read",
		Buckets:   prometheus.ExponentialBuckets(0.5, 2, 10),
	})
	metricBlockBytesRead = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "frigg",
		Name:      "block_bytes_reads",
		Help:      "bytes of blocks read",
		Buckets:   prometheus.ExponentialBuckets(512, 2, 10),
	})
)

// Querier handlers queries.
type Querier struct {
	cfg    Config
	ring   ring.ReadRing
	pool   *cortex_client.Pool
	store  storage.Store
	limits *validation.Overrides
}

type responseFromIngesters struct {
	addr     string
	response interface{}
}

// New makes a new Querier.
func New(cfg Config, clientCfg client.Config, ring ring.ReadRing, store storage.Store, limits *validation.Overrides) (*Querier, error) {
	factory := func(addr string) (grpc_health_v1.HealthClient, error) {
		return client.New(clientCfg, addr)
	}

	return newQuerier(cfg, clientCfg, factory, ring, store, limits)
}

// newQuerier creates a new Querier and allows to pass a custom ingester client factory
// used for testing purposes
func newQuerier(cfg Config, clientCfg client.Config, clientFactory cortex_client.Factory, ring ring.ReadRing, store storage.Store, limits *validation.Overrides) (*Querier, error) {
	return &Querier{
		cfg:    cfg,
		ring:   ring,
		pool:   cortex_client.NewPool(clientCfg.PoolConfig, ring, clientFactory, util.Logger),
		store:  store,
		limits: limits,
	}, nil
}

// FindTraceByID implements friggpb.Querier.
func (q *Querier) FindTraceByID(ctx context.Context, req *friggpb.TraceByIDRequest) (*friggpb.TraceByIDResponse, error) {
	if !validation.ValidTraceID(req.TraceID) {
		return nil, fmt.Errorf("invalid trace id")
	}

	userID, err := user.ExtractOrgID(ctx)
	if err != nil {
		return nil, err
	}

	key := frigg_util.TokenFor(userID, req.TraceID)

	const maxExpectedReplicationSet = 3 // 3.  b/c frigg it
	var descs [maxExpectedReplicationSet]ring.IngesterDesc
	replicationSet, err := q.ring.Get(key, ring.Read, descs[:0])
	if err != nil {
		return nil, err
	}

	// todo:  does this wait for every ingester?  we only need one successful return
	responses, err := q.forGivenIngesters(ctx, replicationSet, func(client friggpb.QuerierClient) (interface{}, error) {
		return client.FindTraceByID(ctx, req)
	})
	if err != nil {
		return nil, err
	}

	var trace *friggpb.Trace
	for _, r := range responses {
		trace = r.response.(*friggpb.TraceByIDResponse).Trace
		if trace != nil {
			break
		}
	}

	// if the ingester didn't have it check the store.  todo: parallelize
	if trace == nil {
		var metrics storage.FindMetrics
		trace, metrics, err = q.store.FindTrace(userID, req.TraceID)
		if err != nil {
			return nil, err
		}

		metricBloomFilterReads.Observe(float64(metrics.BloomFilterReads))
		metricBloomFilterBytesRead.Observe(float64(metrics.BloomFilterBytesRead))
		metricIndexReads.Observe(float64(metrics.IndexReads))
		metricIndexBytesRead.Observe(float64(metrics.IndexBytesRead))
		metricBlockReads.Observe(float64(metrics.BlockReads))
		metricBlockBytesRead.Observe(float64(metrics.BlockBytesRead))
	}

	return &friggpb.TraceByIDResponse{
		Trace: trace,
	}, nil
}

// forGivenIngesters runs f, in parallel, for given ingesters
// TODO taken from Loki taken from Cortex, see if we can refactor out an usable interface.
func (q *Querier) forGivenIngesters(ctx context.Context, replicationSet ring.ReplicationSet, f func(friggpb.QuerierClient) (interface{}, error)) ([]responseFromIngesters, error) {
	results, err := replicationSet.Do(ctx, q.cfg.ExtraQueryDelay, func(ingester *ring.IngesterDesc) (interface{}, error) {
		client, err := q.pool.GetClientFor(ingester.Addr)
		if err != nil {
			return nil, err
		}

		resp, err := f(client.(friggpb.QuerierClient))
		if err != nil {
			return nil, err
		}

		return responseFromIngesters{ingester.Addr, resp}, nil
	})
	if err != nil {
		return nil, err
	}

	responses := make([]responseFromIngesters, 0, len(results))
	for _, result := range results {
		responses = append(responses, result.(responseFromIngesters))
	}

	return responses, err
}

// ReadinessHandler is used to indicate to k8s when the querier is ready.
// Returns 200 when the querier is ready, 500 otherwise.
func (q *Querier) ReadinessHandler(w http.ResponseWriter, r *http.Request) {
	_, err := q.ring.GetAll()
	if err != nil {
		http.Error(w, "Not ready: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(readinessProbeSuccess); err != nil {
		level.Error(util.Logger).Log("msg", "error writing success message", "error", err)
	}
}

// Check implements the grpc healthcheck
func (*Querier) Check(_ context.Context, _ *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}
