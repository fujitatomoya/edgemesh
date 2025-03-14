package main

import (
	"github.com/containernetworking/cni/pkg/skel"
	cni "github.com/containernetworking/cni/pkg/version"

	"github.com/kubeedge/edgemesh/cmd/edgemesh-cni/cmd"
)

var (
	version string
)

func main() {
	skel.PluginMain(cmd.CmdAdd, nil, cmd.CmdDel, cni.All, "EdgeMesh CNI plugin "+version)
}
