/*
Copyright 2017 The Kubernetes Authors.

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
	"flag"
	"fmt"
	"os"

	"github.com/huaweicloud/huaweicloud-csi-driver/pkg/version"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"

	"github.com/huaweicloud/huaweicloud-csi-driver/pkg/config"
	"github.com/huaweicloud/huaweicloud-csi-driver/pkg/evs"
	"github.com/huaweicloud/huaweicloud-csi-driver/pkg/utils/metadatas"
	"github.com/huaweicloud/huaweicloud-csi-driver/pkg/utils/mounts"
)

var (
	endpoint    string
	nodeID      string
	cloudConfig string
	cluster     string
)

func init() {
	//nolint:errcheck
	flag.Set("logtostderr", "true")
}

//nolint:errcheck
func main() {

	flag.CommandLine.Parse([]string{})

	cmd := &cobra.Command{
		Use:   "evs-csi-plugin",
		Short: fmt.Sprintf("HuaweiCloud EVS CSI plugin %s", version.Version),
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			// Glog requires this otherwise it complains.
			flag.CommandLine.Parse(nil)

			// This is a temporary hack to enable proper logging until upstream dependencies
			// are migrated to fully utilize klog instead of glog.
			klogFlags := flag.NewFlagSet("klog", flag.ExitOnError)
			klog.InitFlags(klogFlags)

			// Sync the glog and klog flags.
			cmd.Flags().VisitAll(func(f1 *pflag.Flag) {
				f2 := klogFlags.Lookup(f1.Name)
				if f2 != nil {
					value := f1.Value.String()
					f2.Value.Set(value)
				}
			})
		},
		Run: func(cmd *cobra.Command, args []string) {
			handle()
		},
	}

	cmd.Flags().AddGoFlagSet(flag.CommandLine)

	cmd.PersistentFlags().StringVar(&nodeID, "nodeid", "", "node id")
	cmd.PersistentFlags().MarkDeprecated("nodeid", "This flag would be removed in future. Currently, the value is ignored by the driver")

	cmd.PersistentFlags().StringVar(&endpoint, "endpoint", "", "CSI endpoint")
	cmd.MarkPersistentFlagRequired("endpoint")

	cmd.PersistentFlags().StringVar(&cloudConfig, "cloud-config", "", "CSI driver cloud config. This option can be given multiple times")
	cmd.MarkPersistentFlagRequired("cloud-config")

	cmd.PersistentFlags().StringVar(&cluster, "cluster", "", "The identifier of the cluster that the plugin is running in.")

	logs.InitLogs()
	defer logs.FlushLogs()

	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%s", err.Error())
		os.Exit(1)
	}

	os.Exit(0)
}

func handle() {
	cloud, err := config.LoadConfig(cloudConfig)
	if err != nil {
		klog.Fatalf("Failed to load cloud config: %v", err)
	}

	d := evs.NewDriver(cloud, endpoint, cluster, nodeID)

	mount := mounts.GetMountProvider()
	metadata := metadatas.GetMetadataProvider(metadatas.MetadataID)
	d.SetupDriver(mount, metadata)
	d.Run()
}
