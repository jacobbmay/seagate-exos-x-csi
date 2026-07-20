package main

import (
	"flag"
	"fmt"

	"github.com/Seagate/seagate-exos-x-csi/pkg/common"
	"github.com/Seagate/seagate-exos-x-csi/pkg/controller"
	"k8s.io/klog/v2"
)

var bind = flag.String("bind", fmt.Sprintf("unix:///var/run/%s/csi-controller.sock", common.PluginName), "RPC bind URI (can be a UNIX socket path or any URI)")
var insecureSkipTLSVerify = flag.Bool("insecure-skip-tls-verify", false, "disable TLS certificate verification for storage API HTTPS endpoints (insecure)")
var caBundle = flag.String("ca-bundle", "", "path to a PEM-encoded CA bundle used to verify storage API HTTPS endpoints")

func main() {
	klog.InitFlags(nil)
	flag.Set("logtostderr", "true")
	flag.Parse()
	klog.Infof("starting storage controller (%s)", common.Version)
	c, err := controller.NewWithTLSConfig(controller.TLSConfig{
		InsecureSkipVerify: *insecureSkipTLSVerify,
		CABundlePath:       *caBundle,
	})
	if err != nil {
		klog.Fatalf("failed to configure storage API TLS: %v", err)
	}
	defer c.Stop()
	c.Start(*bind)
}
