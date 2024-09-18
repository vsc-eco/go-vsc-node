package js_ipc

import (
	"os"
	"os/exec"
	"sync/atomic"

	"vsc-node/lib/utils"
	"vsc-node/modules/aggregate"

	"github.com/zealic/go2node"
)

type JsIpc struct {
	cmd       *exec.Cmd
	channel   go2node.NodeChannel
	giveStdin bool

	err  error
	done atomic.Bool

	listeners []func(*go2node.NodeMessage)
}

var _ aggregate.Plugin = &JsIpc{}

// Requires Node.JS to be installed and included in the $PATH envirnoment variable
func New(giveStdin bool, nodeArgs ...string) *JsIpc {
	return &JsIpc{
		cmd:       exec.Command("node", nodeArgs...),
		giveStdin: giveStdin,
	}
}

// Init implements aggregate.Plugin.
func (j *JsIpc) Init() error {
	if j.giveStdin {
		j.cmd.Stdin = os.Stdin
	}
	j.cmd.Stdout = os.Stdout
	j.cmd.Stderr = os.Stderr
	return nil
}

// Start implements aggregate.Plugin.
func (j *JsIpc) Start() error {
	c, err := go2node.ExecNode(j.cmd)
	if err != nil {
		return err
	}

	go func() {
		defer j.cmd.Process.Kill()
		for !j.done.Load() {
			msg, err := c.Read()
			if err != nil {
				j.err = err
				return
			}

			for _, listener := range j.listeners {
				go listener(msg)
			}
		}
	}()

	j.channel = c
	return nil
}

// Stop implements aggregate.Plugin.
func (j *JsIpc) Stop() error {
	j.done.Store(true)
	return j.err
}

func (j *JsIpc) AddMessageListener(listener func(*go2node.NodeMessage)) {
	j.listeners = append(j.listeners, listener)
}

func (j *JsIpc) RemoveMessageListener(listener func(*go2node.NodeMessage)) {
	j.listeners = utils.Remove(j.listeners, listener)
}

func (j *JsIpc) SendMessage(msg []byte) {
	j.channel.Write(&go2node.NodeMessage{Message: msg})
}

func (j *JsIpc) WaitFinished() error {
	res, err := j.cmd.Process.Wait()
	println("res:", res.String())
	return err
}
