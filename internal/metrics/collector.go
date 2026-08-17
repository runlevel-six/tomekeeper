package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// archiveCollector reports the archive's state, read from the database when
// Prometheus scrapes rather than counted as events happen.
//
// The alternative — incrementing counters in the application — would mean two
// records of the same fact, and the in-memory one would be the wrong one after
// every restart. "How many feeds are failing" has an answer in Postgres that is
// correct by construction; there is no reason to maintain a second, worse copy.
//
// The cost is a handful of aggregate queries per scrape. At the size this archive
// reaches that is a few milliseconds, and Prometheus scrapes on the order of once
// a minute.
type archiveCollector struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// scrapeTimeout bounds the queries. A scrape that hangs is worse than a scrape
// that reports nothing: Prometheus will wait, and a stuck monitoring endpoint is
// how a slow database turns into a second outage.
const scrapeTimeout = 5 * time.Second

var (
	feedsDesc = prometheus.NewDesc(
		Namespace+"_feeds",
		"Subscribed feeds by health.",
		[]string{"state"}, nil,
	)
	articlesDesc = prometheus.NewDesc(
		Namespace+"_articles",
		"Articles by fetch outcome.",
		[]string{"fetch_status"}, nil,
	)
	bodiesDesc = prometheus.NewDesc(
		Namespace+"_bodies",
		"Current article bodies by the extractor that produced them.",
		[]string{"extractor"}, nil,
	)
	assetsStateDesc = prometheus.NewDesc(
		Namespace+"_articles_by_assets_status",
		"Articles by the state of their image localization.",
		[]string{"assets_status"}, nil,
	)
	assetsDesc = prometheus.NewDesc(
		Namespace+"_assets",
		"Distinct images stored in the archive.",
		nil, nil,
	)
	assetBytesDesc = prometheus.NewDesc(
		Namespace+"_asset_bytes",
		"Total bytes of stored images.",
		nil, nil,
	)
	bodyBytesDesc = prometheus.NewDesc(
		Namespace+"_body_bytes",
		"Total bytes of stored article bodies, HTML and text.",
		nil, nil,
	)
	jobsDesc = prometheus.NewDesc(
		Namespace+"_jobs",
		"Background jobs by state.",
		[]string{"state"}, nil,
	)
	scrapeErrorsDesc = prometheus.NewDesc(
		Namespace+"_scrape_errors_total",
		"Queries that failed while collecting these metrics.",
		nil, nil,
	)
	scrapeDurationDesc = prometheus.NewDesc(
		Namespace+"_scrape_duration_seconds",
		"Time taken to collect these metrics from the database.",
		nil, nil,
	)
)

// Describe implements prometheus.Collector.
func (c *archiveCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		feedsDesc, articlesDesc, bodiesDesc, assetsStateDesc,
		assetsDesc, assetBytesDesc, bodyBytesDesc, jobsDesc,
		scrapeErrorsDesc, scrapeDurationDesc,
	} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *archiveCollector) Collect(ch chan<- prometheus.Metric) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()

	var failures float64

	// Each query is independent, so one failing does not cost the others. A
	// database that is up but slow should still report what it can.
	labeled := func(desc *prometheus.Desc, query string) {
		rows, err := c.pool.Query(ctx, query)
		if err != nil {
			c.log.Warn("collecting metrics failed", "error", err)
			failures++
			return
		}
		defer rows.Close()

		for rows.Next() {
			var (
				label string
				n     float64
			)
			if err := rows.Scan(&label, &n); err != nil {
				c.log.Warn("scanning a metric row failed", "error", err)
				failures++
				return
			}
			ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, n, label)
		}
		if err := rows.Err(); err != nil {
			failures++
		}
	}

	scalar := func(desc *prometheus.Desc, query string) {
		var n float64
		if err := c.pool.QueryRow(ctx, query).Scan(&n); err != nil {
			c.log.Warn("collecting a metric failed", "error", err)
			failures++
			return
		}
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, n)
	}

	// Deliberately no per-feed labels. Which feed is failing is a question the
	// feed health page answers better, and a label carrying a feed title would put
	// a list of subscriptions into the monitoring system.
	labeled(feedsDesc, `
		SELECT CASE WHEN disabled THEN 'disabled'
		            WHEN consecutive_failures > 0 THEN 'failing'
		            ELSE 'ok' END,
		       count(*)::float8
		FROM feeds GROUP BY 1`)

	labeled(articlesDesc, `SELECT fetch_status, count(*)::float8 FROM articles GROUP BY 1`)

	labeled(assetsStateDesc, `SELECT assets_status, count(*)::float8 FROM articles GROUP BY 1`)

	labeled(bodiesDesc, `
		SELECT extractor_name, count(*)::float8
		FROM article_content WHERE is_current GROUP BY 1`)

	// River owns this table, so a missing one means the queue has never been
	// migrated rather than that something is broken; the failure counter says so
	// without the scrape failing.
	labeled(jobsDesc, `SELECT state::text, count(*)::float8 FROM river_job GROUP BY 1`)

	scalar(assetsDesc, `SELECT count(*)::float8 FROM assets`)
	scalar(assetBytesDesc, `SELECT COALESCE(sum(byte_size), 0)::float8 FROM assets`)
	scalar(bodyBytesDesc, `
		SELECT COALESCE(sum(length(content_html) + length(content_text)), 0)::float8
		FROM article_content WHERE is_current`)

	ch <- prometheus.MustNewConstMetric(scrapeErrorsDesc, prometheus.CounterValue, failures)
	ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue, time.Since(started).Seconds())
}
