package announcements_test

import (
	"testing"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/announcements"
)

func TestExecutionTime(t *testing.T) {
	conf := announcements.NewAnnouncementsConfig()
	a := announcements.New(conf)
	test_utils.RunPlugin(t, a)

	select {}
}
