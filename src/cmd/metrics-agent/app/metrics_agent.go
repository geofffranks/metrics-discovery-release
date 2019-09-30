package app

import (
	gendiodes "code.cloudfoundry.org/go-diodes"
	metrics "code.cloudfoundry.org/go-metric-registry"
	"code.cloudfoundry.org/loggregator-agent/pkg/diodes"
	egress_v2 "code.cloudfoundry.org/loggregator-agent/pkg/egress/v2"
	v2 "code.cloudfoundry.org/loggregator-agent/pkg/ingress/v2"
	"code.cloudfoundry.org/loggregator-agent/pkg/scraper"
	"code.cloudfoundry.org/metrics-discovery/internal/collector"
	"code.cloudfoundry.org/metrics-discovery/internal/gatherer"
	"code.cloudfoundry.org/metrics-discovery/internal/target"
	"code.cloudfoundry.org/tlsconfig"
	"context"
	"crypto/tls"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/yaml.v2"
	"log"
	"net/http"
	"os"
	"time"
)

type MetricsAgent struct {
	cfg           Config
	log           *log.Logger
	metrics       Metrics
	metricsServer *http.Server
	scrapeConfigs map[string]scraper.PromScraperConfig
}

type ScrapeConfigProvider func() ([]scraper.PromScraperConfig, error)

type Metrics interface {
	NewCounter(name, helpText string, options ...metrics.MetricOption) metrics.Counter
}

func NewMetricsAgent(cfg Config, scrapeConfigProvider ScrapeConfigProvider, metrics Metrics, log *log.Logger) *MetricsAgent {
	scrapeConfigs, err := scrapeConfigProvider()
	if err != nil {
		log.Printf("error getting scrape config: %s", err)
	}

	ma := &MetricsAgent{
		cfg:           cfg,
		log:           log,
		metrics:       metrics,
		scrapeConfigs: make(map[string]scraper.PromScraperConfig, len(scrapeConfigs)),
	}

	for _, sc := range scrapeConfigs {
		ma.scrapeConfigs[sc.SourceID] = sc
	}

	ma.buildMetricsTargets()

	return ma
}

func (m *MetricsAgent) Run() {
	envelopeBuffer := m.envelopeDiode()
	go m.startIngressServer(envelopeBuffer)

	promCollector := collector.NewEnvelopeCollector(
		m.metrics,
		collector.WithSourceIDExpiration(m.cfg.MetricsExporter.TimeToLive, m.cfg.MetricsExporter.ExpirationInterval),
		collector.WithDefaultTags(m.cfg.MetricsExporter.DefaultLabels),
	)
	go m.startEnvelopeCollection(promCollector, envelopeBuffer)

	m.startMetricsServer(promCollector)
}

func (m *MetricsAgent) envelopeDiode() *diodes.ManyToOneEnvelopeV2 {
	ingressDropped := m.metrics.NewCounter(
		"dropped",
		"Total number of dropped envelopes.",
		metrics.WithMetricLabels(map[string]string{"direction": "ingress"}),
	)
	return diodes.NewManyToOneEnvelopeV2(10000, gendiodes.AlertFunc(func(missed int) {
		ingressDropped.Add(float64(missed))
	}))
}

func (m *MetricsAgent) startIngressServer(diode *diodes.ManyToOneEnvelopeV2) {
	ingressMetric := m.metrics.NewCounter(
		"ingress",
		"Total number of envelopes ingressed by the agent.",
	)
	originMetric := m.metrics.NewCounter(
		"origin_mappings",
		"Total number of envelopes where the origin tag is used as the source_id.",
	)

	receiver := v2.NewReceiver(diode, ingressMetric, originMetric)
	tlsConfig := m.generateServerTLSConfig(m.cfg.GRPC.CertFile, m.cfg.GRPC.KeyFile, m.cfg.GRPC.CAFile)
	server := v2.NewServer(
		fmt.Sprintf("127.0.0.1:%d", m.cfg.GRPC.Port),
		receiver,
		grpc.Creds(credentials.NewTLS(tlsConfig)),
	)

	server.Start()
}

func (m *MetricsAgent) generateServerTLSConfig(certFile, keyFile, caFile string) *tls.Config {
	tlsConfig, err := tlsconfig.Build(
		tlsconfig.WithInternalServiceDefaults(),
		tlsconfig.WithIdentityFromFile(certFile, keyFile),
	).Server(
		tlsconfig.WithClientAuthenticationFromFile(caFile),
	)
	if err != nil {
		log.Fatalf("unable to generate server TLS Config: %s", err)
	}

	return tlsConfig
}

func (m *MetricsAgent) startEnvelopeCollection(promCollector *collector.EnvelopeCollector, diode *diodes.ManyToOneEnvelopeV2) {
	tagger := egress_v2.NewTagger(m.cfg.Tags).TagEnvelope
	timerTagFilterer := egress_v2.NewTimerTagFilterer(m.cfg.MetricsExporter.WhitelistedTimerTags, tagger).Filter
	envelopeWriter := egress_v2.NewEnvelopeWriter(
		promCollector,
		egress_v2.NewCounterAggregator(
			timerTagFilterer,
		),
	)

	for {
		next := diode.Next()
		if m.hasScrapeConfig(next.GetSourceId()) {
			continue
		}

		err := envelopeWriter.Write(next)
		if err != nil {
			log.Printf("unable to write envelope: %s", err)
		}
	}
}

func (m *MetricsAgent) startMetricsServer(envelopeCollector *collector.EnvelopeCollector) {
	router := http.NewServeMux()
	router.Handle(
		"/metrics",
		m.buildMetricHandler(envelopeCollector),
	)

	tlsConfig := m.generateServerTLSConfig(
		m.cfg.MetricsServer.CertFile,
		m.cfg.MetricsServer.KeyFile,
		m.cfg.MetricsServer.CAFile,
	)
	m.metricsServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", m.cfg.MetricsExporter.Port),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		Handler:      router,
		TLSConfig:    tlsConfig,
	}

	log.Printf("Metrics server closing: %s", m.metricsServer.ListenAndServeTLS("", ""))
}

func (m *MetricsAgent) buildMetricHandler(envelopeCollector *collector.EnvelopeCollector) http.Handler {
	envelopeHandler := m.envelopeHandler(envelopeCollector)
	proxyHandlers := m.proxyHandlers()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if id == "" {
			envelopeHandler.ServeHTTP(w, r)
			return
		}

		handler, ok := proxyHandlers[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		handler.ServeHTTP(w, r)
	})
}

func (m *MetricsAgent) envelopeHandler(envelopeCollector *collector.EnvelopeCollector) http.Handler {
	envelopeGatherer := prometheus.NewRegistry()
	envelopeGatherer.MustRegister(envelopeCollector)
	envelopeHandler := promhttp.HandlerFor(
		envelopeGatherer,
		promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError},
	)
	return envelopeHandler
}

func (m *MetricsAgent) proxyHandlers() map[string]http.Handler {
	metricHandlers := make(map[string]http.Handler, len(m.scrapeConfigs))
	for sourceId, sc := range m.scrapeConfigs {
		proxyGatherer := gatherer.NewProxyGatherer(
			sc,
			m.cfg.ScrapeCertPath,
			m.cfg.ScrapeKeyPath,
			m.cfg.ScrapeCACertPath,
			m.metrics,
			m.log,
		)

		metricHandlers[sourceId] = promhttp.HandlerFor(proxyGatherer, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
	}
	return metricHandlers
}

func (m *MetricsAgent) Stop() {
	ctx, cancelFunc := context.WithDeadline(context.Background(), time.Now().Add(15*time.Second))

	go func() {
		defer cancelFunc()

		if m.metricsServer != nil {
			m.metricsServer.Shutdown(ctx)
		}
	}()

	<-ctx.Done()
}

func (m *MetricsAgent) hasScrapeConfig(sourceID string) bool {
	_, ok := m.scrapeConfigs[sourceID]
	return ok
}

func (m *MetricsAgent) buildMetricsTargets() {
	metricsExporterTarget := []string{fmt.Sprintf("%s:%d", m.cfg.Addr, m.cfg.MetricsExporter.Port)}

	labels := copyMap(m.cfg.Tags)
	labels["instance_id"] = m.cfg.InstanceID

	targets := []target.Target{{
		Targets: metricsExporterTarget,
		Source:  fmt.Sprintf("metrics_agent_exporter__%s", m.cfg.InstanceID),
		Labels:  labels,
	}}

	for _, sc := range m.scrapeConfigs {
		targetLabels := appendScrapeConfigLabels(labels, sc)

		targets = append(targets, target.Target{
			Targets: metricsExporterTarget,
			Labels:  targetLabels,
			Source:  fmt.Sprintf("%s__%s", sc.SourceID, m.cfg.InstanceID),
		})
	}

	f, err := os.Create(m.cfg.MetricsTargetFile)
	if err != nil {
		m.log.Fatalf("unable to create metrics target file at %s: %s", m.cfg.MetricsTargetFile, err)
	}
	defer f.Close()

	err = yaml.NewEncoder(f).Encode(targets)
	if err != nil {
		m.log.Fatalf("unable to marshal metrics target file: %s", err)
	}
}

func copyMap(original map[string]string) map[string]string {
	copied := map[string]string{}

	if original != nil {
		for k, v := range original {
			copied[k] = v
		}
	}

	return copied
}

func appendScrapeConfigLabels(labels map[string]string, sc scraper.PromScraperConfig) map[string]string {
	targetLabels := copyMap(labels)

	targetLabels["__param_id"] = sc.SourceID
	targetLabels["source_id"] = sc.SourceID

	for k, v := range sc.Labels {
		targetLabels[k] = v
	}

	return targetLabels
}
