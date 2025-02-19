// This file and its contents are licensed under the Apache License 2.0.
// Please see the included NOTICE for copyright information and
// LICENSE for a copy of the license.

package querier

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgconn"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v4"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/storage"
	"github.com/timescale/promscale/pkg/pgmodel/cache"
	"github.com/timescale/promscale/pkg/pgmodel/common/errors"
	"github.com/timescale/promscale/pkg/pgmodel/common/schema"
	"github.com/timescale/promscale/pkg/pgmodel/lreader"
	"github.com/timescale/promscale/pkg/pgmodel/model"
	"github.com/timescale/promscale/pkg/pgxconn"
	"github.com/timescale/promscale/pkg/prompb"
	"github.com/timescale/promscale/pkg/tenancy"
)

// Reader reads the data based on the provided read request.
type Reader interface {
	Read(*prompb.ReadRequest) (*prompb.ReadResponse, error)
}

type QueryHints struct {
	StartTime   time.Time
	EndTime     time.Time
	CurrentNode parser.Node
	Lookback    time.Duration
}

// SerieSet adds a Close method to storage.SeriesSet to provide a way to free memory
type SeriesSet interface {
	storage.SeriesSet
	Close()
}

// Querier queries the data using the provided query data and returns the
// matching timeseries.
type Querier interface {
	// Query returns resulting timeseries for a query.
	Query(*prompb.Query) ([]*prompb.TimeSeries, error)
	// Select returns a series set that matches the supplied query parameters.
	Select(mint int64, maxt int64, sortSeries bool, hints *storage.SelectHints, queryHints *QueryHints, path []parser.Node, ms ...*labels.Matcher) (SeriesSet, parser.Node)
}

const (
	getMetricsTableSQL = "SELECT table_schema, table_name, series_table FROM " + schema.Catalog + ".get_metric_table_name_if_exists($1, $2)"
)

// NewQuerier returns a new pgxQuerier that reads from PostgreSQL using PGX
// and caches metric table names and label sets using the supplied caches.
func NewQuerier(conn pgxconn.PgxConn, metricCache cache.MetricCache, labelsReader lreader.LabelsReader, rAuth tenancy.ReadAuthorizer) Querier {
	return &pgxQuerier{
		conn:             conn,
		labelsReader:     labelsReader,
		metricTableNames: metricCache,
		rAuth:            rAuth,
	}
}

type metricTimeRangeFilter struct {
	metric      string
	schema      string
	column      string
	seriesTable string
	startTime   string
	endTime     string
}

type pgxQuerier struct {
	conn             pgxconn.PgxConn
	metricTableNames cache.MetricCache
	labelsReader     lreader.LabelsReader
	rAuth            tenancy.ReadAuthorizer
}

var _ Querier = (*pgxQuerier)(nil)

// Select implements the Querier interface. It is the entry point for our
// own version of the Prometheus engine.
func (q *pgxQuerier) Select(mint int64, maxt int64, sortSeries bool, hints *storage.SelectHints, qh *QueryHints, path []parser.Node, ms ...*labels.Matcher) (SeriesSet, parser.Node) {
	rows, topNode, err := q.getResultRows(mint, maxt, hints, qh, path, ms)
	if err != nil {
		return errorSeriesSet{err: err}, nil
	}

	ss := buildSeriesSet(rows, q.labelsReader)
	return ss, topNode
}

// Query implements the Querier interface. It is the entry point for
// remote-storage queries.
func (q *pgxQuerier) Query(query *prompb.Query) ([]*prompb.TimeSeries, error) {
	if query == nil {
		return []*prompb.TimeSeries{}, nil
	}

	matchers, err := fromLabelMatchers(query.Matchers)
	if err != nil {
		return nil, err
	}

	rows, _, err := q.getResultRows(query.StartTimestampMs, query.EndTimestampMs, nil, nil, nil, matchers)
	if err != nil {
		return nil, err
	}

	results, err := buildTimeSeries(rows, q.labelsReader)
	return results, err
}

// fromLabelMatchers parses protobuf label matchers to Prometheus label matchers.
func fromLabelMatchers(matchers []*prompb.LabelMatcher) ([]*labels.Matcher, error) {
	result := make([]*labels.Matcher, 0, len(matchers))
	for _, matcher := range matchers {
		var mtype labels.MatchType
		switch matcher.Type {
		case prompb.LabelMatcher_EQ:
			mtype = labels.MatchEqual
		case prompb.LabelMatcher_NEQ:
			mtype = labels.MatchNotEqual
		case prompb.LabelMatcher_RE:
			mtype = labels.MatchRegexp
		case prompb.LabelMatcher_NRE:
			mtype = labels.MatchNotRegexp
		default:
			return nil, fmt.Errorf("invalid matcher type")
		}
		matcher, err := labels.NewMatcher(mtype, matcher.Name, matcher.Value)
		if err != nil {
			return nil, err
		}
		result = append(result, matcher)
	}
	return result, nil
}

// getResultRows fetches the result row datasets from the database using the
// supplied query parameters.
func (q *pgxQuerier) getResultRows(startTimestamp int64, endTimestamp int64, hints *storage.SelectHints, qh *QueryHints, path []parser.Node, matchers []*labels.Matcher) ([]timescaleRow, parser.Node, error) {
	if q.rAuth != nil {
		matchers = q.rAuth.AppendTenantMatcher(matchers)
	}
	// Build a subquery per metric matcher.
	builder, err := BuildSubQueries(matchers)
	if err != nil {
		return nil, nil, err
	}

	metric := builder.GetMetricName()

	filter := metricTimeRangeFilter{
		metric:    metric,
		schema:    builder.GetSchemaName(),
		column:    builder.GetColumnName(),
		startTime: toRFC3339Nano(startTimestamp),
		endTime:   toRFC3339Nano(endTimestamp),
	}

	// If all metric matchers match on a single metric (common case),
	// we query only that single metric.
	if metric != "" {
		clauses, values, err := builder.Build(false)
		if err != nil {
			return nil, nil, err
		}
		return q.querySingleMetric(metric, filter, clauses, values, hints, qh, path)
	}

	clauses, values, err := builder.Build(true)
	if err != nil {
		return nil, nil, err
	}
	return q.queryMultipleMetrics(filter, clauses, values)
}

// querySingleMetric returns all the result rows for a single metric using the
// supplied query parameters. It uses the hints and node path to try to push
// down query functions where possible.
func (q *pgxQuerier) querySingleMetric(metric string, filter metricTimeRangeFilter, cases []string, values []interface{}, hints *storage.SelectHints, qh *QueryHints, path []parser.Node) ([]timescaleRow, parser.Node, error) {
	mInfo, err := q.getMetricTableName(filter.schema, metric)
	if err != nil {
		// If the metric table is missing, there are no results for this query.
		if err == errors.ErrMissingTableName {
			return nil, nil, nil
		}

		return nil, nil, err
	}

	filter.metric = mInfo.TableName
	filter.schema = mInfo.TableSchema
	filter.seriesTable = mInfo.SeriesTable

	sqlQuery, values, topNode, tsSeries, err := buildTimeseriesByLabelClausesQuery(filter, cases, values, hints, qh, path)
	if err != nil {
		return nil, nil, err
	}

	rows, err := q.conn.Query(context.Background(), sqlQuery, values...)
	if err != nil {
		if e, ok := err.(*pgconn.PgError); ok {
			switch e.Code {
			case pgerrcode.UndefinedTable:
				// If we are getting undefined table error, it means the metric we are trying to query
				// existed at some point but the underlying relation was removed from outside of the system.
				return nil, nil, fmt.Errorf(errors.ErrTmplMissingUnderlyingRelation, mInfo.TableSchema, mInfo.TableName)
			case pgerrcode.UndefinedColumn:
				// If we are getting undefined column error, it means the column we are trying to query
				// does not exist in the metric table so we return empty results.
				// Empty result is more consistent and in-line with PromQL assumption of a missing series based on matchers.
				return nil, nil, nil
			}
		}

		return nil, nil, err
	}

	defer rows.Close()

	updatedMetricName := ""
	// If the table name and series table name don't match, this is a custom metric view which
	// shares the series table with the raw metric, hence we have to update the metric name label.
	if mInfo.TableName != mInfo.SeriesTable {
		updatedMetricName = metric
	}

	// TODO this allocation assumes we usually have 1 row, if not, refactor
	tsRows, err := appendTsRows(make([]timescaleRow, 0, 1), rows, tsSeries, updatedMetricName, filter.schema, filter.column)
	return tsRows, topNode, err
}

// queryMultipleMetrics returns all the result rows for across multiple metrics
// using the supplied query parameters.
func (q *pgxQuerier) queryMultipleMetrics(filter metricTimeRangeFilter, cases []string, values []interface{}) ([]timescaleRow, parser.Node, error) {
	// First fetch series IDs per metric.
	sqlQuery := BuildMetricNameSeriesIDQuery(cases)
	rows, err := q.conn.Query(context.Background(), sqlQuery, values...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	metrics, schemas, series, err := GetSeriesPerMetric(rows)
	if err != nil {
		return nil, nil, err
	}

	// TODO this assume on average on row per-metric. Is this right?
	results := make([]timescaleRow, 0, len(metrics))

	numQueries := 0
	batch := q.conn.NewBatch()

	// Generate queries for each metric and send them in a single batch.
	for i, metric := range metrics {
		//TODO batch getMetricTableName
		mInfo, err := q.getMetricTableName(schemas[i], metric)
		if err != nil {
			// If the metric table is missing, there are no results for this query.
			if err == errors.ErrMissingTableName {
				continue
			}
			return nil, nil, err
		}

		// We only support default data schema for multi-metric queries
		// NOTE: this needs to be updated once we add support for storing
		// non-view metrics into multiple schemas
		if mInfo.TableSchema != schema.Data {
			return nil, nil, fmt.Errorf("found unsupported metric schema in multi-metric matching query")
		}

		filter.metric = mInfo.TableName
		filter.schema = mInfo.TableSchema
		filter.seriesTable = mInfo.SeriesTable

		sqlQuery = buildTimeseriesBySeriesIDQuery(filter, series[i])
		batch.Queue(sqlQuery)
		numQueries += 1
	}

	batchResults, err := q.conn.SendBatch(context.Background(), batch)
	if err != nil {
		return nil, nil, err
	}
	defer batchResults.Close()

	for i := 0; i < numQueries; i++ {
		rows, err = batchResults.Query()
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		// Append all rows into results. Metric name and additional labels are
		// ignored for multi-metric queries
		results, err = appendTsRows(results, rows, nil, "", "", "")
		// Can't defer because we need to Close before the next loop iteration.
		rows.Close()
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
	}

	return results, nil, nil
}

// getMetricTableName gets the table name for a specific metric from internal
// cache. If not found, fetches it from the database and updates the cache.
func (q *pgxQuerier) getMetricTableName(schema, metric string) (model.MetricInfo, error) {

	mInfo, err := q.metricTableNames.Get(schema, metric)

	if err == nil || err != errors.ErrEntryNotFound {
		return mInfo, err
	}

	mInfo, err = q.queryMetricTableName(schema, metric)

	if err != nil {
		return mInfo, err
	}

	err = q.metricTableNames.Set(schema, metric, mInfo)

	return mInfo, err
}

func (q *pgxQuerier) queryMetricTableName(schema, metric string) (mInfo model.MetricInfo, err error) {
	row := q.conn.QueryRow(
		context.Background(),
		getMetricsTableSQL,
		schema,
		metric,
	)

	if err = row.Scan(&mInfo.TableSchema, &mInfo.TableName, &mInfo.SeriesTable); err != nil {
		if err == pgx.ErrNoRows {
			err = errors.ErrMissingTableName
		}
		return mInfo, err
	}

	return mInfo, nil
}

// errorSeriesSet represents an error result in a form of a series set.
// This behavior is inherited from Prometheus codebase.
type errorSeriesSet struct {
	err error
}

func (errorSeriesSet) Next() bool                   { return false }
func (errorSeriesSet) At() storage.Series           { return nil }
func (e errorSeriesSet) Err() error                 { return e.err }
func (e errorSeriesSet) Warnings() storage.Warnings { return nil }
func (e errorSeriesSet) Close()                     {}

type labelQuerier interface {
	LabelsForIdMap(idMap map[int64]labels.Label) (err error)
}
