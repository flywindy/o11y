package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/flywindy/o11y/internal/repeat"
)

// scrapeErrorRepeatWindow is how long an identical gather error is
// suppressed after it was last logged. promhttp reports the same error on
// every scrape while the offending series exists, which for a cumulative
// aggregation is until the process restarts; one line per 15 s scrape would
// drown the log for nothing new.
const scrapeErrorRepeatWindow = 5 * time.Minute

// newMetricsHandler builds the /metrics handler over gatherer.
//
// The handler continues on gather errors rather than failing the scrape:
// promhttp's default is to answer HTTP 500 whenever any family fails to
// gather, which turns one bad metric into "no metrics at all" for the pod —
// runtime, HTTP and database series included — until it restarts. With
// ContinueOnError the healthy families are still served and the error is
// logged (rate-limited) so the offending family can be found and fixed.
//
// EnableOpenMetrics lets the handler content-negotiate to OpenMetrics format,
// the only exposition format otelprom renders per-bucket exemplars in; see
// Config.Exemplars for the `le` label side effect and how to opt out.
func newMetricsHandler(gatherer prometheus.Gatherer, openMetrics bool, logger *slog.Logger) http.Handler {
	return promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{
		EnableOpenMetrics: openMetrics,
		ErrorHandling:     promhttp.ContinueOnError,
		ErrorLog:          newScrapeErrorLogger(logger),
	})
}

// maxTrackedScrapeErrors bounds the per-message suppression table. Distinct
// gather errors are rare (one per broken family, and the text is stable for a
// given family), so the bound is a memory guard, not a working-set size.
const maxTrackedScrapeErrors = 32

// scrapeErrorLogger adapts promhttp's Println-style logger to slog and
// suppresses repeats of each distinct message within scrapeErrorRepeatWindow.
// Suppression is tracked per message, so two families failing on alternate
// scrapes are each logged once per window rather than on every scrape.
type scrapeErrorLogger struct {
	logger   *slog.Logger
	now      func() time.Time
	suppress *repeat.Suppressor
}

// newScrapeErrorLogger returns a scrapeErrorLogger writing to logger. A nil
// logger discards everything, so the handler never dereferences nil.
func newScrapeErrorLogger(logger *slog.Logger) *scrapeErrorLogger {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &scrapeErrorLogger{
		logger:   logger,
		now:      time.Now,
		suppress: repeat.NewSuppressor(scrapeErrorRepeatWindow, maxTrackedScrapeErrors),
	}
}

// Println implements promhttp.Logger.
func (l *scrapeErrorLogger) Println(v ...any) {
	msg := fmt.Sprintln(v...)
	msg = msg[:len(msg)-1] // drop the trailing newline Sprintln adds

	if l.suppress.SuppressedAt(msg, l.now()) {
		return
	}

	l.logger.WarnContext(context.Background(),
		"metrics scrape served with errors; the affected family is missing from /metrics",
		slog.String("error", msg),
		slog.String("repeat_suppressed_for", scrapeErrorRepeatWindow.String()),
	)
}
