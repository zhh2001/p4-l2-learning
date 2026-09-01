# P4 reactive L2 learning switch

This project is a single-switch Ethernet learning bridge built with P4_16, BMv2 `simple_switch_grpc`, P4Runtime, Mininet, and a Go controller. MAC state is learned reactively from P4 digests. Normal packets remain in the data plane; learning does not use PacketIn, and forwarding does not use PacketOut.

## Topology

```text
             h2
              |
              2
              |
    h1 --1-- s1 --3-- h3
              |
              4
              |
             h4
```

| Host | Switch port | MAC address | IPv4 address |
| --- | ---: | --- | --- |
| h1 | 1 | `00:00:00:00:00:01` | `10.0.0.1/24` |
| h2 | 2 | `00:00:00:00:00:02` | `10.0.0.2/24` |
| h3 | 3 | `00:00:00:00:00:03` | `10.0.0.3/24` |
| h4 | 4 | `00:00:00:00:00:04` | `10.0.0.4/24` |

The hosts share one broadcast domain. Mininet does not install static ARP entries, so an interactive first ping naturally exercises flooding and learning.

## Data plane

The parser extracts only the Ethernet destination, source, and EtherType. Ethernet addresses are not rewritten, and the remaining frame is carried unchanged for IPv4, IPv6, ARP, custom EtherTypes, and arbitrary payloads. This is an L2 bridge: it does not parse IP, decrement TTL, or update IP checksums.

Two P4Runtime tables hold learned state:

- `source_location` matches `(src_mac, ingress_port)`. A hit suppresses another digest; a miss emits `mac_learn_digest_t { src_mac, ingress_port }`. Including the ingress port makes a source appearing on a new port generate a digest.
- `destination_mac` matches `dst_mac`. A hit selects one egress port, while the default action floods an unknown unicast destination.

Broadcast and Ethernet multicast destinations are always flooded. Flooding sets BMv2 PRE multicast group 1, whose replicas are exactly ports 1, 2, 3, and 4. The egress pipeline drops a replica when its egress port equals the ingress port. The same check drops a known destination learned on the packet's ingress port, so frames are never reflected to their source segment.

A multicast source address or the all-zero source address is invalid. Such a frame is dropped before destination processing and does not generate a digest.

## Reactive learning

At startup the controller becomes P4Runtime primary, installs and reads back the pipeline, programs and verifies the PRE group, confirms that both learned tables are empty, and configures the digest. The DigestEntry uses `max_list_size=1`, `max_timeout_ns=0`, and `ack_timeout_ns=1000000000` for low-latency single-item delivery.

For each DigestList, the controller verifies the P4Info digest ID, decodes every member, and validates the unicast source and bridge port. A new source installs both `(MAC, port)` in `source_location` and `MAC -> port` in `destination_mac`. The controller reads the entries back, then sends a DigestListAck containing the exact digest ID and list ID.

Repeated learning on the same port is idempotent. When a MAC moves, the controller updates its destination entry, removes the old source-location entry, installs the new source-location entry, and verifies that only the new port remains authoritative.

A basic exchange converges as follows:

```text
first h1 -> h2:
    unknown destination
    flood
    learn h1

h2 -> h1:
    known h1
    unicast
    learn h2

second h1 -> h2:
    known h2
    unicast
```

## Prerequisites

- Linux with root privileges for Mininet namespaces and raw packet sockets
- GNU Make
- `p4c-bm2-ss`
- BMv2 `simple_switch_grpc`
- Mininet
- Python 3 with Scapy
- Go 1.25 as selected by `go.mod`
- `ethtool` and `sysctl`

Go modules pin P4Runtime v1.5.0 and `github.com/zhh2001/p4runtime-go-controller` v1.1.1. The runtime uses P4Runtime at `127.0.0.1:50052`, BMv2 Thrift port 9091, and device ID 2.

## Build and run

Build the P4 pipeline and controller:

```sh
make build
```

Generated P4Info, BMv2 JSON, and the controller binary are placed in the ignored `build/` directory.

Start the complete environment and enter the Mininet CLI:

```sh
make run
```

The command starts BMv2 and the controller, verifies static startup state, then enables the host links. For example:

```text
mininet> net
mininet> h1 ping -c 1 10.0.0.2
mininet> h1 ping -c 1 10.0.0.2
mininet> exit
```

Exiting the CLI stops only the BMv2, controller, Mininet processes, interfaces, and temporary files owned by that run.

## Tests

Run the complete suite with:

```sh
make test
```

The target compiles P4 with warnings treated as errors, builds and tests the Go controller, runs `go vet`, performs Python compile and structural checks, and then invokes the privileged BMv2/Mininet integration suite through `sudo`.

Packet tests verify exact output multiplicity and frame bytes for unknown unicast, broadcast, Ethernet multicast, known unicast, same-port pruning, MAC moves, invalid sources, IPv4, ARP, and custom EtherTypes. Learning convergence is synchronized by polling actual P4Runtime table readback. The suite also checks duplicate suppression, stale source-location removal, DigestListAck logic, exact PRE and DigestEntry state, normal cleanup, and startup-failure cleanup.

Remove project-generated build output and Python caches with:

```sh
make clean
```

## Limitations

This is a v1model/BMv2 single-switch reference implementation with one fixed four-port broadcast domain. MAC aging is not implemented; learned entries remain until a move or runtime reset. VLANs, 802.1Q, STP, and other loop prevention are not implemented. It does not route IP or provide multi-switch operation.
