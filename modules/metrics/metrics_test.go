package metrics_test

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/metrics"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func waitForServer(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("metrics server at %s never came up", url)
}

func TestMetricsServerExposesCustomCollector(t *testing.T) {
	dataDir := t.TempDir()
	conf := metrics.NewMetricsConfig(dataDir)
	require.NoError(t, conf.Init())
	require.NoError(t, conf.SetHostAddr(freePort(t)))

	probe := promauto.With(prometheus.DefaultRegisterer).NewCounter(prometheus.CounterOpts{
		Namespace: "magi", Subsystem: "test",
		Name: "probe_total",
		Help: "Test counter for the metrics endpoint smoke test",
	})
	probe.Add(7)

	mgr := metrics.New(conf)
	require.NoError(t, mgr.Init())

	startErr := make(chan error, 1)
	go func() {
		_, err := mgr.Start().Await(t.Context())
		startErr <- err
	}()
	defer func() {
		require.NoError(t, mgr.Stop())
		select {
		case err := <-startErr:
			require.NoError(t, err)
		case <-time.After(3 * time.Second):
			t.Fatal("Start goroutine did not exit after Stop")
		}
	}()

	url := "http://" + conf.GetHostAddr() + conf.GetPath()
	waitForServer(t, url)

	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	text := string(body)
	assert.Contains(t, text, "magi_test_probe_total 7")
	assert.Contains(t, text, "go_goroutines")
}

func TestMetricsServerDisabled(t *testing.T) {
	dataDir := t.TempDir()
	conf := metrics.NewMetricsConfig(dataDir)
	require.NoError(t, conf.Init())
	require.NoError(t, conf.SetEnabled(false))

	mgr := metrics.New(conf)
	require.NoError(t, mgr.Init())

	done := make(chan error, 1)
	go func() {
		_, err := mgr.Start().Await(t.Context())
		done <- err
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("disabled Start should resolve immediately")
	}

	require.NoError(t, mgr.Stop())
}

func TestConfigDefaults(t *testing.T) {
	dataDir := t.TempDir()
	conf := metrics.NewMetricsConfig(dataDir)
	require.NoError(t, conf.Init())

	assert.True(t, conf.IsEnabled())
	assert.Equal(t, metrics.DefaultHostAddr, conf.GetHostAddr())
	assert.Equal(t, metrics.DefaultPath, conf.GetPath())

	require.NoError(t, conf.SetHostAddr(""))
	assert.Equal(t, metrics.DefaultHostAddr, conf.GetHostAddr(), "empty host should fall back to default")

	require.NoError(t, conf.SetPath(""))
	assert.Equal(t, metrics.DefaultPath, conf.GetPath(), "empty path should fall back to default")

	require.True(t, strings.HasPrefix(metrics.DefaultPath, "/"))
}
