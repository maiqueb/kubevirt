package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/network"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/network/ndp"
)

func NewCreateNDPConnectionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "create-ndp-connection",
		Short: "create an NDP connection listening to RouterSolicitations on a given interface",
		RunE: func(cmd *cobra.Command, args []string) error {
			serverIface := cmd.Flag("listen-on-iface").Value.String()
			launcherPID := cmd.Flag("launcher-pid").Value.String()

			ndpConnection, err := ndp.NewNDPConnection(serverIface)
			if err != nil {
				return fmt.Errorf("failed to create the RouterAdvertisement daemon: %v", err)
			}
			return ndpConnection.Export(network.GetNDPConnectionUnixSocketPath(launcherPID, serverIface))
		},
	}
}
