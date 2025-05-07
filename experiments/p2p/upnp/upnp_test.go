package upnp_test

import (
	"testing"
	"vsc-node/experiments/p2p/config"
	"vsc-node/experiments/p2p/upnp"
	"vsc-node/lib/test_utils"
)

func Test(t *testing.T) {
	conf := config.Config{
		Port: 36000,
	}
	u := upnp.New(&conf)

	test_utils.RunPlugin(t, u)
	select {}
}
