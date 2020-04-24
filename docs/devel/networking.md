# VMI Networking
TODO

## VMI networking configuration
To be able to follow this section, a minimal understanding of the
[KubeVirt architecture](arhitecture.png) is needed, especially the separation of concerns
between the virt-handler and virt-launcher processes.

In this section we'll explain how VM networking is configured. In order to
follow the principle of least privilege (where each component is limited
to only its required privileges), the configuration of a KubeVirt VM interfaces
is split into two distint phases:
- [privileged networking configuration](#privileged-vmi-networking-configuration): occurs in the virt-handler process
- [unprivileged networking configuration](#unprivileged-vmi-networking-configuration): occurs in the virt-launcher process

### Privileged VMI networking configuration
Virt-handler is a trusted component of KubeVirt; it runs on a privileged
daemonset. It is responsible for creating & configuring any required network
infrastructure (or configuration) required by the
[binding mechanisms](#binding-mechanisms).

It is important to refer that while this step is performed by virt-handler, it
is performed in the *target* virt-launcher's net namespace, as can be seen in
the implementation of the [setPodNetworkPhase1 function](https://github.com/kubevirt/kubevirt/blob/8e7f2a411f5d06f1734e96c7bc08d0bb9ec1d500/pkg/virt-handler/vm.go#L340).

This will in turn call the
[PlugPhase1](https://github.com/kubevirt/kubevirt/blob/5995d2164faefdcc7a59a857e12f8ed4cdb7e094/pkg/virt-launcher/virtwrap/network/podinterface.go#L84),
which performs the following operations:
- `getPhase1Binding`: this function initializes the interface structures, and
  selects the correct `driver` to use. The driver, is essentially a
  `BindMechanism`. The next operations will all be of specific implementations
  of [BindMechanisms](#binding-mechanisms).

- `discoverPodNetworkInterface`: Each `BindMechanism` requires different
   information about the pod interface - slirp, for instance, doesn't require
   any info. The others, gather the following information:
   - IP address
   - Routes (**only** bridge)
   - Gateway
   - MAC address (**only** bridge)
   - Link MTU

- `preparePodNetworkInterfaces`: this function will make use of the
  aforementioned information, performing actions specific to each `BindMechanism`.
  See each `BindMechanism` section for more details.

- `setCachedInterface`: caches the interface in memory

- `setCachedVIF`: this will persist the
  [VIF](https://github.com/kubevirt/kubevirt/blob/51ce9d8e6def33c0c260f0cdb994f08ae5b9938e/pkg/virt-launcher/virtwrap/network/common.go#L48)
  object in the file system, making the configured pod interface available to
  the virt-launcher pod. The VIF is cached in
  `/proc/<virt-launcher-pid>/root/var/run/kubevirt-private/vif-cache-<iface_name>.json`.

### Unprivileged VMI networking configuration
The virt-launcher is an untrusted component of KubeVirt (since it wraps the
libvirt process that will run third party workloads). As a result, it must be
run with as little privileges as required. As of now, the only capability
required by virt-launcher to configure networking is the `CAP_NET_ADMIN`
capability.

In this second phase, virt-launcher retrieves the configuration data previously
gathered in phase #1 (loads the cached VIF object), and decorates the domain
xml configuration of the VM it will encapsulate. This *decoration* is specific
to each [binding mechanism](#binding-mechanisms), as each of those can involve
different libvirt configurations.

## Binding Mechanisms
A binding mechanism can be seen as the translation service between KubeVirt's
API and Libvirt's domain xml. Each interface type has a different binding
mechanism, since it will lead to a different libvirt domain xml specification.

As of now, the existent binding mechanisms are:
- [bridge](#bridge-binding-mechanism)
- [masquerade](#masquerade-binding-mechanism)
- [slirp](#slirp-binding-mechanism)

### Bridge binding mechanism

### Masquerade binding mechanism

#### Masquerade IPv6 binding mechanism

### Slirp binding mechanism

## Attaching to multiple networks

### Multus
