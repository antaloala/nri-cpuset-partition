/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/containerd/nri/pkg/stub"

	"github.com/antaloala/nri-cpuset-partition/internal/cgroup"
	"github.com/antaloala/nri-cpuset-partition/internal/partition"
)

// Command nri-cpuset-partition runs the NRI plugin that turns the cgroups of
// containers holding exclusively assigned cpus into cgroup v2 cpuset
// partitions.
func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "nri-cpuset-partition: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	defaults := partition.DefaultConfig()

	var (
		pluginName  = flag.String("plugin-name", "cpuset-partition", "NRI plugin name")
		pluginIdx   = flag.String("plugin-index", "10", "NRI plugin index, two digits, decides the plugin ordering")
		socketPath  = flag.String("nri-socket", "", "NRI socket path (empty: the NRI default)")
		cgroupRoot  = flag.String("cgroup-root", "", "cgroup v2 mount point (empty: autodetect)")
		kubepods    = flag.String("kubepods-cgroup", defaults.KubepodsCgroup, "name of the pod cgroup tree below the cgroup root")
		resync      = flag.Duration("resync-period", defaults.ResyncPeriod, "how often to re-check that the partitions are still in place (0 disables)")
		cleanup     = flag.Bool("cleanup-on-exit", defaults.CleanupOnExit, "release every partition when the plugin stops")
		dryRun      = flag.Bool("dry-run", false, "log the cgroup writes instead of performing them")
		logLevel    = flag.String("log-level", "info", "debug, info, warn or error")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	level, err := parseLevel(*logLevel)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	onWrite := func(path, value string) {
		if *dryRun {
			log.Info("DRY RUN: would write", "path", path, "value", value)
			return
		}
		log.Debug("writing", "path", path, "value", value)
	}

	cg, err := cgroup.NewManager(*cgroupRoot, *dryRun, onWrite)
	if err != nil {
		return err
	}
	log.Info("starting", "version", version, "cgroupRoot", cg.Root(), "dryRun", *dryRun)

	cfg := partition.Config{
		KubepodsCgroup: *kubepods,
		ResyncPeriod:   *resync,
		CleanupOnExit:  *cleanup,
	}
	plugin := partition.New(cfg, cg, log)

	opts := []stub.Option{
		stub.WithPluginName(*pluginName),
		stub.WithPluginIdx(*pluginIdx),
	}
	if *socketPath != "" {
		opts = append(opts, stub.WithSocketPath(*socketPath))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *resync > 0 {
		go resyncLoop(ctx, plugin, *resync)
	}

	// containerd restarts drop the connection; reconnect rather than letting
	// the DaemonSet crash-loop us, which would lose the in-memory state that
	// Synchronize then has to rebuild anyway.
	const retryDelay = 5 * time.Second
	for {
		s, err := stub.New(plugin, opts...)
		if err != nil {
			return fmt.Errorf("cannot create plugin stub: %w", err)
		}

		err = s.Run(ctx)
		switch {
		case ctx.Err() != nil:
			log.Info("stopping")
			if *cleanup {
				plugin.Cleanup(context.Background())
			}
			return nil
		case err == nil:
			log.Warn("runtime closed the connection, reconnecting", "in", retryDelay)
		default:
			log.Error("plugin stopped", "err", err, "retryIn", retryDelay)
		}

		select {
		case <-ctx.Done():
		case <-time.After(retryDelay):
		}
	}
}

func resyncLoop(ctx context.Context, plugin *partition.Plugin, period time.Duration) {
	ticker := time.NewTicker(period)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			plugin.Resync(ctx)
		}
	}
}

func parseLevel(s string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("invalid log level %q", s)
	}
	return level, nil
}
