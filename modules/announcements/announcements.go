package announcements

import (
	"context"
	"log"
	agg "vsc-node/modules/aggregate"
	"vsc-node/modules/config"

	"github.com/robfig/cron/v3"

	"github.com/chebyrash/promise"
)

// ===== types =====

type announcementsManager struct {
	conf *config.Config[announcementsConfig]
	cron *cron.Cron
	stop chan struct{}
}

// ===== intetface assertions =====

var _ agg.Plugin = &announcementsManager{}

// ===== constructor =====

func New(conf *config.Config[announcementsConfig]) *announcementsManager {
	return &announcementsManager{
		cron: cron.New(),
		conf: conf,
		stop: make(chan struct{}),
	}
}

// ===== implementing plugin interface =====

func (a *announcementsManager) Init() error {
	return nil
}

func (a *announcementsManager) task(ctx context.Context) {
	log.Println("task")
}

func (a *announcementsManager) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		// create a ctx that cancels when the stop chan is closed
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-a.stop
			cancel()
		}()

		// run task immediately
		go a.task(ctx)

		// schedule the task to run every 24 hours starting from now
		_, err := a.cron.AddFunc("@every 24h", func() {
			// check if stop signal has been received before running the task
			select {
			case <-a.stop:
				return
			default:
				go a.task(ctx)
			}
		})
		if err != nil {
			reject(err)
			return
		}
		a.cron.Start()
		resolve(nil)
	})
}

func (a *announcementsManager) Stop() error {
	// safely close the stop channel
	select {
	case <-a.stop:
		// do nothing, already stopped
	default:
		close(a.stop)
	}
	// stop cron scheduler
	a.cron.Stop()
	return nil
}
