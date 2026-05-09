package metrics

import "vsc-node/modules/config"

type metricsConfig struct {
	Enabled  bool
	HostAddr string
	Path     string
}

type metricsConfigStruct struct {
	*config.Config[metricsConfig]
}

type MetricsConfig = *metricsConfigStruct

const (
	DefaultHostAddr = "0.0.0.0:8081"
	DefaultPath     = "/metrics"
)

func NewMetricsConfig(dataDir ...string) MetricsConfig {
	var dataDirPtr *string
	if len(dataDir) > 0 {
		dataDirPtr = &dataDir[0]
	}

	return &metricsConfigStruct{config.New(metricsConfig{
		Enabled:  true,
		HostAddr: DefaultHostAddr,
		Path:     DefaultPath,
	}, dataDirPtr)}
}

func (mc *metricsConfigStruct) IsEnabled() bool {
	return mc.Get().Enabled
}

func (mc *metricsConfigStruct) GetHostAddr() string {
	addr := mc.Get().HostAddr
	if addr == "" {
		return DefaultHostAddr
	}
	return addr
}

func (mc *metricsConfigStruct) GetPath() string {
	p := mc.Get().Path
	if p == "" {
		return DefaultPath
	}
	return p
}

func (mc *metricsConfigStruct) SetEnabled(enabled bool) error {
	return mc.Update(func(c *metricsConfig) {
		c.Enabled = enabled
	})
}

func (mc *metricsConfigStruct) SetHostAddr(addr string) error {
	return mc.Update(func(c *metricsConfig) {
		c.HostAddr = addr
	})
}

func (mc *metricsConfigStruct) SetPath(p string) error {
	return mc.Update(func(c *metricsConfig) {
		c.Path = p
	})
}
