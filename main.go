package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"

	"net/http"
	"os/signal"

	"github.com/fsnotify/fsnotify"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// UserMaildir collects the exported metrics and
// the internal representation of the underlying
// Maildir structure for one user in the system.
type UserMaildir struct {
	userPath     string
	watcher      *fsnotify.Watcher
	walkTrigger  chan struct{}
	watchTrigger chan struct{}
	done         chan struct{}
}

// Metrics aggregates all four gauges we expose
// to Prometheus for insights into underlying Maildirs.
type Metrics struct {
	elements *prometheus.GaugeVec
	folders  *prometheus.GaugeVec
	files    *prometheus.GaugeVec
	size     *prometheus.GaugeVec
}

// initLogger initializes a JSON gokit-logger set
// to the according log level supplied via CLI flag.
func initLogger(loglevel string) log.Logger {

	logger := log.NewJSONLogger(log.NewSyncWriter(os.Stdout))
	logger = log.With(logger,
		"ts", log.DefaultTimestampUTC,
		"caller", log.Caller(4),
	)

	switch strings.ToLower(loglevel) {
	case "info":
		logger = level.NewFilter(logger, level.AllowInfo())
	case "warn":
		logger = level.NewFilter(logger, level.AllowWarn())
	case "error":
		logger = level.NewFilter(logger, level.AllowError())
	default:
		logger = level.NewFilter(logger, level.AllowDebug())
	}

	return logger
}

// createMetrics initializes and registers all four
// Prometheus-exposed metrics.
func createMetrics() *Metrics {

	// Prepare four Prometheus-exposed gauge vectors.
	maildirElements := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "maildir_elements",
		Help: "Number of elements (folders and files) in a user's Maildir.",
	}, []string{"user"})

	maildirFolders := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "maildir_folders",
		Help: "Number of folders in a user's Maildir.",
	}, []string{"user"})

	maildirFiles := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "maildir_files",
		Help: "Number of files in a user's Maildir.",
	}, []string{"user"})

	maildirSize := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "maildir_size",
		Help: "Size of a user's Maildir (folders and files) in bytes, associated with a SHA512 checksum of all folder and file names.",
	}, []string{"user", "sha512"})

	// Register all of them with Prometheus.
	prometheus.MustRegister(maildirElements)
	prometheus.MustRegister(maildirFolders)
	prometheus.MustRegister(maildirFiles)
	prometheus.MustRegister(maildirSize)

	return &Metrics{
		elements: maildirElements,
		folders:  maildirFolders,
		files:    maildirFiles,
		size:     maildirSize,
	}
}

func main() {

	// metricsPath := flag.String("metricsPath", "/metrics", "Specify where to expose collected Maildir metrics.")
	maildirRootPath := flag.String("maildirRootPath", "", "Specify path to directory containing all users' Maildirs.")
	logLevel := flag.String("logLevel", "", "Set verbosity level of logging.")
	flag.Parse()

	// Create gokit-logger based on specified verbosity level.
	logger := initLogger(*logLevel)

	if *maildirRootPath == "" {
		level.Error(logger).Log("msg", "maildirElementsplease specify a maildirRootPath")
		os.Exit(1)
	}

	// Catch SIGINT and SIGTERM signals.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	// Create metrics struct.
	metrics := createMetrics()

	// Retrieve internal representation of all
	// folders and files per user in specified
	// Maildir directory.
	userMaildirs, err := walkRootMaildir(*maildirRootPath)
	if err != nil {
		level.Error(logger).Log(
			"msg", fmt.Sprintf("failed to walk user Maildirs at %s", *maildirRootPath),
			"err", err,
		)
	}

	walkDone := make(chan struct{}, len(userMaildirs))

	// Walk the Maildirs of all users present in the
	// service in background and await re-walk triggers.
	for _, m := range userMaildirs {
		go m.walk(logger, metrics, walkDone)
		m.walkTrigger <- struct{}{}
	}

	for i := 0; i < len(userMaildirs); i++ {
		<-walkDone
	}

	// Kick-off fsnotify trigger processing for
	// all watched Maildirs.
	for _, m := range userMaildirs {
		go m.watch(logger)
	}

	// Define where we want to expose metrics via HTTP.
	http.Handle("/metrics", promhttp.Handler())
	server := &http.Server{Addr: ":9275"}

	go func() {

		// Start HTTP server for exposing /metrics to
		// the Prometheus scraper in background.
		err := server.ListenAndServe()
		if err != nil {
			level.Error(logger).Log(
				"msg", "error while running HTTP server for /metrics",
				"err", err,
			)
			os.Exit(1)
		}
	}()

	// Wait until we receive a program termination.
	<-sigs
	fmt.Println()

	// Perform graceful shutdown of HTTP server.
	level.Info(logger).Log("msg", "shutting down HTTP server for /metrics")
	server.Shutdown(context.Background())

	// Instruct watcher to finish.
	for _, m := range userMaildirs {
		m.done <- struct{}{}
		m.done <- struct{}{}
	}
}
