#include <core.p4>
#include <v1model.p4>

typedef bit<48> mac_addr_t;
typedef bit<9> port_t;

const bit<16> FLOOD_GROUP = 1;

header ethernet_t {
    mac_addr_t dst_mac;
    mac_addr_t src_mac;
    bit<16> ether_type;
}

struct headers_t {
    ethernet_t ethernet;
}

struct metadata_t { }

struct mac_learn_digest_t {
    mac_addr_t src_mac;
    port_t ingress_port;
}

parser ParserImpl(
        packet_in packet,
        out headers_t hdr,
        inout metadata_t meta,
        inout standard_metadata_t standard_metadata) {
    state start {
        packet.extract(hdr.ethernet);
        transition accept;
    }
}

control VerifyChecksumImpl(
        inout headers_t hdr,
        inout metadata_t meta) {
    apply { }
}

control IngressImpl(
        inout headers_t hdr,
        inout metadata_t meta,
        inout standard_metadata_t standard_metadata) {
    action learn_source() {
        digest<mac_learn_digest_t>(1,
            { hdr.ethernet.src_mac, standard_metadata.ingress_port });
    }

    action source_known() { }

    table source_location {
        key = {
            hdr.ethernet.src_mac: exact @name("src_mac");
            standard_metadata.ingress_port: exact @name("ingress_port");
        }
        actions = {
            @tableonly source_known;
            @defaultonly learn_source;
        }
        size = 1024;
        const default_action = learn_source();
    }

    action forward(port_t port) {
        standard_metadata.egress_spec = port;
    }

    action flood() {
        standard_metadata.mcast_grp = FLOOD_GROUP;
    }

    table destination_mac {
        key = {
            hdr.ethernet.dst_mac: exact @name("dst_mac");
        }
        actions = {
            @tableonly forward;
            @defaultonly flood;
        }
        size = 1024;
        const default_action = flood();
    }

    apply {
        if (!hdr.ethernet.isValid()) {
            mark_to_drop(standard_metadata);
        } else if (hdr.ethernet.src_mac[40:40] == 1 ||
                   hdr.ethernet.src_mac == 0) {
            mark_to_drop(standard_metadata);
        } else {
            source_location.apply();
            if (hdr.ethernet.dst_mac[40:40] == 1) {
                flood();
            } else {
                destination_mac.apply();
            }
        }
    }
}

control EgressImpl(
        inout headers_t hdr,
        inout metadata_t meta,
        inout standard_metadata_t standard_metadata) {
    apply {
        if (standard_metadata.egress_port ==
            standard_metadata.ingress_port) {
            mark_to_drop(standard_metadata);
        }
    }
}

control ComputeChecksumImpl(
        inout headers_t hdr,
        inout metadata_t meta) {
    apply { }
}

control DeparserImpl(
        packet_out packet,
        in headers_t hdr) {
    apply {
        packet.emit(hdr.ethernet);
    }
}

V1Switch(
    ParserImpl(),
    VerifyChecksumImpl(),
    IngressImpl(),
    EgressImpl(),
    ComputeChecksumImpl(),
    DeparserImpl()
) main;
