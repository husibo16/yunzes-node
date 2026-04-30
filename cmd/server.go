package cmd

import (
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"

	"github.com/husibo16/yunzes-node/conf"
	vCore "github.com/husibo16/yunzes-node/core"
	"github.com/husibo16/yunzes-node/limiter"
	"github.com/husibo16/yunzes-node/node"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	config string
	watch  bool
)

var serverCommand = cobra.Command{
	Use:   "server",
	Short: "Run node server",
	Run:   serverHandle,
	Args:  cobra.NoArgs,
}

func init() {
	serverCommand.PersistentFlags().
		StringVarP(&config, "config", "c",
			"/etc/yunzes-node/config.json", "config file path")
	serverCommand.PersistentFlags().
		BoolVarP(&watch, "watch", "w",
			true, "watch file path change")
	command.AddCommand(&serverCommand)
}

func serverHandle(_ *cobra.Command, _ []string) {
	showVersion()
	c := conf.New()
	err := c.LoadFromPath(config)
	if err != nil {
		log.WithField("err", err).Error("Load config file failed")
		return
	}
	switch c.LogConfig.Level {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "warn", "warning":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	}
	if c.LogConfig.Output != "" {
		f, err := os.OpenFile(c.LogConfig.Output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.WithField("err", err).Error("Open log file failed, using stdout instead")
		}
		log.SetOutput(f)
	}
	limiter.Init()
	log.Info("Start yunzes-node...")
	vc, err := vCore.NewCore(c.CoresConfig)
	if err != nil {
		log.WithField("err", err).Error("new core failed")
		return
	}
	err = vc.Start()
	if err != nil {
		log.WithField("err", err).Error("Start core failed")
		return
	}
	defer vc.Close()
	log.Info("Core ", vc.Type(), " started")
	nodes := node.New()
	if c.ApiConfig.ApiHost != "" && c.ApiConfig.ServerId != 0 && c.ApiConfig.SecretKey != "" && len(c.NodeConfig) == 0 {
		err = nodes.StartNodes(&c.ApiConfig, vc)
	} else {
		err = nodes.Start(c.NodeConfig, vc)
	}
	if err != nil {
		log.WithField("err", err).Error("Run nodes failed")
		return
	}
	log.Info("Nodes started")
	if watch {
		// reloadMu serializes overlapping reload events. conf.Watch's
		// 10s debounce only collapses events INSIDE the window; events
		// 11s+ apart spawn separate goroutines that would otherwise
		// race on (vc, nodes, c).
		var reloadMu sync.Mutex
		builders := realRuntimeBuilders()

		err = c.Watch(config, func(newC *conf.Conf) {
			reloadMu.Lock()
			defer reloadMu.Unlock()

			// Snapshot the currently-live config for the rollback path.
			// Shallow copy of the struct value is enough — the
			// orchestrator only reads CoresConfig / NodeConfig / ApiConfig
			// from it, all of which are slices/structs the rebuild path
			// re-walks (it doesn't mutate them).
			oldC := *c

			le := log.WithField("phase", "watch-reload")
			// *node.Node satisfies nodeRunner via its existing Close
			// method; the interface is only meant for reload's testable
			// orchestration. The runner returned on the success / restore
			// paths is built by realRuntimeBuilders.newNodes which always
			// hands back *node.Node, so the type assertion below is safe
			// in those branches.
			newCore, newNodes, outcome := reloadProcess(le, builders, &oldC, newC, vc, nodes)

			switch outcome {
			case reloadSucceeded:
				vc = newCore
				nodes = newNodes.(*node.Node)
				*c = *newC
				runtime.GC()
			case reloadKeptOld:
				// (vc, nodes) are still the old ones — orchestrator
				// returned them unchanged. *c is NOT updated so the
				// in-memory config still matches what is actually
				// serving traffic.
			case reloadRestoredOld:
				// Old was torn down then rebuilt; rebind to the rebuilt
				// instances. *c stays at oldC (== current *c) because
				// the new config never went live.
				vc = newCore
				nodes = newNodes.(*node.Node)
				runtime.GC()
			case reloadOffline:
				// Process has no inbound. (vc, nodes) get cleared so a
				// subsequent reload event can attempt a fresh boot.
				vc = nil
				nodes = nil
			}
		})
		if err != nil {
			log.WithField("err", err).Error("start watch failed")
			return
		}
	}
	// clear memory
	runtime.GC()
	// wait exit signal
	{
		osSignals := make(chan os.Signal, 1)
		signal.Notify(osSignals, syscall.SIGINT, syscall.SIGTERM)
		<-osSignals
	}
}
