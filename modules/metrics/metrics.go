package metrics

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"vsc-node/lib/utils"
	"vsc-node/lib/vsclog"
	a "vsc-node/modules/aggregate"

	"github.com/chebyrash/promise"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var log = vsclog.Module("metrics")

const shutdownTimeout = 5 * time.Second

type metricsManager struct {
	conf      MetricsConfig
	server    *http.Server
	started   atomic.Bool
	disabled  bool
	registrar prometheus.Registerer
	gatherer  prometheus.Gatherer
}

var _ a.Plugin = &metricsManager{}

var registerProcessCollectorsOnce = registerProcessCollectorsFunc()

func registerProcessCollectorsFunc() func(prometheus.Registerer) {
	var done atomic.Bool
	return func(r prometheus.Registerer) {
		if done.Swap(true) {
			return
		}
		_ = r.Register(collectors.NewGoCollector())
		_ = r.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	}
}

func New(conf MetricsConfig) *metricsManager {
	return &metricsManager{
		conf:      conf,
		registrar: prometheus.DefaultRegisterer,
		gatherer:  prometheus.DefaultGatherer,
	}
}

func (m *metricsManager) Init() error {
	if !m.conf.IsEnabled() {
		log.Info("metrics endpoint disabled")
		m.disabled = true
		return nil
	}

	registerProcessCollectorsOnce(m.registrar)

	mux := http.NewServeMux()
	mux.Handle(m.conf.GetPath(), promhttp.HandlerFor(m.gatherer, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))

	m.server = &http.Server{
		Addr:              m.conf.GetHostAddr(),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
	}

	return nil
}

func (m *metricsManager) Start() *promise.Promise[any] {
	if m.disabled {
		return utils.PromiseResolve[any](nil)
	}
	return promise.New(func(resolve func(any), reject func(error)) {
		log.Info("metrics endpoint available", "addr", m.conf.GetHostAddr()+m.conf.GetPath())
		m.started.Store(true)
		if err := m.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			reject(err)
			return
		}
		resolve(nil)
	})
}

func (m *metricsManager) Stop() error {
	if m.disabled || m.server == nil {
		return nil
	}
	log.Info("shutting down metrics server")
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := m.server.Shutdown(ctx); err != nil {
		return err
	}
	log.Info("metrics server shut down successfully")
	return nil
}
