# VFIO passthrough devices (proposal)

Status: proposal + MVP implementation. Tracking issue:
[kubernetes-sigs/dranet#375](https://github.com/kubernetes-sigs/dranet/issues/375).

## Problem

Some workloads need a whole PCI network function in user space: a VMM passing it
to a guest (KubeVirt `HostDevicesWithDRA`), or a DPDK application. On the same
nodes, other workloads use functions as netdevs through DRANET as usual.

DRANET can only give a function as a netdev. A function bound to `vfio-pci` is not
published at all:

```
PCI network device 0000:5e:00.2 is bound to driver "vfio-pci" which does not
provide a netdev; not publishing it
```

Operators therefore split their VFs statically per node and run a second driver
(`dra-driver-sriov`) for the VFIO half, which also means two drivers, two address
allocation paths and two upgrade cycles.

## Device model

There is no VM in this design, only a pod and a device. From DRANET's point of
view the only difference between a netdev and a VFIO device is **how the device
is handed over**:

* netdev: the interface is moved into the pod network namespace;
* VFIO: the function stays bound to `vfio-pci` on the host and the pod receives
  `/dev/vfio/vfio` and `/dev/vfio/<iommu-group>`, which is what QEMU or a DPDK
  EAL opens.

### Discovery

PCI network functions bound to `vfio-pci` are published like any other function,
with two extra attributes:

| Attribute | Meaning |
|---|---|
| `dra.net/driver` | the kernel driver currently bound (e.g. `vfio-pci`, `mlx5_core`) |
| `dra.net/iommuGroup` | IOMMU group of the function, resolved from sysfs |

`dra.net/iommuGroup` is only set for passthrough functions: it is required to
build the device node path, and it is what a DeviceClass selector uses to pin a
claim to an isolated group. A `vfio-pci` function whose group cannot be resolved
is **not** published, because DRANET could not hand it over.

### Claim

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
spec:
  devices:
    config:
      - requests: ["vfio-nic"]
        opaque:
          driver: dra.net
          parameters:
            vfio: {}
```

`vfio` is mutually exclusive with `interface`, `routes`, `rules`, `neighbors`,
`ethtool` and `profile`: DRANET does not own the addressing or the offloads of a
device it cannot see as a netdev. The IOMMU group is derived from the device at
prepare time rather than put in the claim, so a claim stays portable between
nodes and a later driver-rebinding feature does not need an API change.

### Prepare / create

* `NodePrepareResources`: validate the config, resolve the IOMMU group, record
  `/dev/vfio/vfio` and `/dev/vfio/<group>` (major/minor read from the host
  nodes) in the checkpointed device config.
* `RunPodSandbox`: nothing to move; the device is marked ready.
* `CreateContainer`: the two character devices are injected through NRI next to
  the RDMA devices DRANET already injects.
* `NodeUnprepareResources`: nothing to undo, because the host state was not
  modified.

## What this deliberately does not do (yet)

* **Binding the function at prepare time.** The MVP requires the function to be
  bound to `vfio-pci` beforehand. Rebinding the current driver to `vfio-pci` on
  prepare and restoring it on unprepare is the natural next step (the stateful
  checkpoint already stores enough to remember the original driver), but it turns
  a read-only operation into a host-mutating one and needs its own failure
  handling: a crash between unbind and bind leaves a function with no driver.
* **Creating VFs.** VF creation stays an operator/node concern, as it is today
  for netdev VFs.
* **CDI instead of NRI.** NRI device injection is used because the RDMA path
  already does it; CDI would additionally let non-NRI runtimes consume the
  device.

## Tests

* unit: publishing `vfio-pci` functions (with and without a resolvable IOMMU
  group), `dra.net/driver` and `dra.net/iommuGroup` attributes, claim
  validation, char-device resolution, NRI injection and de-duplication.
* opt-in hardware test (`DRANET_REAL_VFIO=1`): asserts that every `vfio-pci`
  network function on the host is published with the group sysfs reports, and
  that its `/dev/vfio` nodes resolve to real character devices.

Verified on a node with a ConnectX-5 IB controller bound to `vfio-pci`
(`0000:5e:00.0`, IOMMU group 1):

```
published vfio device: name=pci-0000-5e-00-0 addr=0000:5e:00.0 group=1
injected device: /dev/vfio/vfio type=c major=10 minor=196
injected device: /dev/vfio/1 type=c major=238 minor=1
```

## Open questions

1. Should publishing `vfio-pci` functions be behind a feature flag? Clients that
   match on `sriov` or on any device of the node would start seeing devices they
   cannot use as netdevs — the `dra.net/driver` attribute lets them filter, but
   existing DeviceClasses are not written with that in mind.
2. `uio_pci_generic`, `igb_uio` and `pci-stub` stay unpublished. Is there a
   supported way to hand those to a workload, or is `vfio-pci` the only contract?
3. Rebind-on-prepare: should the original driver live in the checkpoint, or be
   re-derived (the device's vendor/device ID maps back to its usual driver)?
4. For KubeVirt specifically: is the expected flow a `HostDevice` with
   `externalResourceProvider: true` pointing at `dra.net`, and does the guest
   need any DRANET-side annotation (e.g. the function's MAC) that the type-2/
   netdev path provides today?
