import json
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
BUILD = ROOT / "build"


def named(items):
    return {item["preamble"]["name"]: item for item in items}


def tree_values(value, key):
    if isinstance(value, dict):
        if key in value:
            yield value[key]
        for child in value.values():
            yield from tree_values(child, key)
    elif isinstance(value, list):
        for child in value:
            yield from tree_values(child, key)


def expression_fields(expression):
    fields = set()
    for item in tree_values(expression, "value"):
        if isinstance(item, list) and len(item) == 2:
            fields.add(tuple(item))
    return fields


def action_for_id(program, action_id):
    return next(action for action in program["actions"] if action["id"] == action_id)


def evaluate_expression(node, fields):
    node_type = node["type"]
    value = node["value"]
    if node_type == "field":
        return fields[tuple(value)]
    if node_type == "hexstr":
        return int(value, 16)
    if node_type != "expression":
        raise AssertionError(f"unsupported expression node: {node_type}")

    operation = value["op"]
    right = evaluate_expression(value["right"], fields)
    if operation == "d2b":
        return bool(right)

    left = evaluate_expression(value["left"], fields)
    operations = {
        "&": lambda: left & right,
        ">>": lambda: left >> right,
        "==": lambda: left == right,
        "or": lambda: bool(left) or bool(right),
    }
    try:
        return operations[operation]()
    except KeyError as error:
        raise AssertionError(f"unsupported expression operation: {operation}") from error


class P4InfoStructureTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.info = json.loads(
            (BUILD / "learning_switch.p4info.json").read_text(encoding="utf-8")
        )
        cls.tables = named(cls.info["tables"])
        cls.actions = named(cls.info["actions"])

    def test_exposes_only_learning_tables_and_actions(self):
        self.assertEqual(self.info["pkgInfo"]["arch"], "v1model")
        self.assertEqual(
            set(self.tables),
            {"IngressImpl.source_location", "IngressImpl.destination_mac"},
        )
        self.assertEqual(
            set(self.actions),
            {
                "IngressImpl.learn_source",
                "IngressImpl.source_known",
                "IngressImpl.forward",
                "IngressImpl.flood",
            },
        )
        self.assertEqual(
            {table["preamble"]["alias"] for table in self.info["tables"]},
            {"source_location", "destination_mac"},
        )
        self.assertEqual(
            {action["preamble"]["alias"] for action in self.info["actions"]},
            {"learn_source", "source_known", "forward", "flood"},
        )

    def test_source_location_identity_includes_ingress_port(self):
        table = self.tables["IngressImpl.source_location"]
        fields = [
            (field["name"], field["bitwidth"], field["matchType"])
            for field in table["matchFields"]
        ]
        self.assertEqual(
            fields,
            [
                ("src_mac", 48, "EXACT"),
                ("ingress_port", 9, "EXACT"),
            ],
        )
        learn_id = self.actions["IngressImpl.learn_source"]["preamble"]["id"]
        known_id = self.actions["IngressImpl.source_known"]["preamble"]["id"]
        self.assertEqual(table["constDefaultActionId"], learn_id)
        self.assertEqual({ref["id"] for ref in table["actionRefs"]}, {learn_id, known_id})
        learn_ref = next(ref for ref in table["actionRefs"] if ref["id"] == learn_id)
        known_ref = next(ref for ref in table["actionRefs"] if ref["id"] == known_id)
        self.assertEqual(learn_ref["scope"], "DEFAULT_ONLY")
        self.assertEqual(known_ref["scope"], "TABLE_ONLY")

    def test_destination_table_forwards_and_floods_on_miss(self):
        table = self.tables["IngressImpl.destination_mac"]
        fields = [
            (field["name"], field["bitwidth"], field["matchType"])
            for field in table["matchFields"]
        ]
        self.assertEqual(fields, [("dst_mac", 48, "EXACT")])
        forward = self.actions["IngressImpl.forward"]
        flood = self.actions["IngressImpl.flood"]
        self.assertEqual(forward["params"], [{"id": 1, "name": "port", "bitwidth": 9}])
        self.assertEqual(table["constDefaultActionId"], flood["preamble"]["id"])
        self.assertEqual(
            {ref["id"] for ref in table["actionRefs"]},
            {forward["preamble"]["id"], flood["preamble"]["id"]},
        )
        forward_ref = next(
            ref for ref in table["actionRefs"] if ref["id"] == forward["preamble"]["id"]
        )
        flood_ref = next(
            ref for ref in table["actionRefs"] if ref["id"] == flood["preamble"]["id"]
        )
        self.assertEqual(forward_ref["scope"], "TABLE_ONLY")
        self.assertEqual(flood_ref["scope"], "DEFAULT_ONLY")

    def test_digest_contains_only_source_and_port(self):
        self.assertEqual(len(self.info["digests"]), 1)
        digest = self.info["digests"][0]
        self.assertEqual(digest["preamble"]["name"], "mac_learn_digest_t")
        self.assertEqual(digest["typeSpec"]["struct"]["name"], "mac_learn_digest_t")
        members = self.info["typeInfo"]["structs"]["mac_learn_digest_t"]["members"]
        decoded = [
            (member["name"], member["typeSpec"]["bitstring"]["bit"]["bitwidth"])
            for member in members
        ]
        self.assertEqual(decoded, [("src_mac", 48), ("ingress_port", 9)])


class BMv2StructureTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.program = json.loads(
            (BUILD / "learning_switch.json").read_text(encoding="utf-8")
        )
        cls.pipelines = {pipeline["name"]: pipeline for pipeline in cls.program["pipelines"]}
        cls.ingress = cls.pipelines["ingress"]
        cls.egress = cls.pipelines["egress"]
        cls.ingress_tables = {table["name"]: table for table in cls.ingress["tables"]}

    def test_parser_extracts_only_ethernet(self):
        ethernet = next(
            header for header in self.program["header_types"] if header["name"] == "ethernet_t"
        )
        self.assertEqual(
            ethernet["fields"],
            [
                ["dst_mac", 48, False],
                ["src_mac", 48, False],
                ["ether_type", 16, False],
            ],
        )
        packet_headers = [
            header for header in self.program["headers"] if not header["metadata"]
        ]
        self.assertEqual(len(packet_headers), 1)
        self.assertEqual(
            (packet_headers[0]["name"], packet_headers[0]["header_type"]),
            ("ethernet", "ethernet_t"),
        )
        extracts = [
            operation["parameters"]
            for parser in self.program["parsers"]
            for state in parser["parse_states"]
            for operation in state["parser_ops"]
            if operation["op"] == "extract"
        ]
        self.assertEqual(
            extracts,
            [[{"type": "regular", "value": "ethernet"}]],
        )

    def test_digest_primitive_uses_two_field_learn_list(self):
        self.assertEqual(len(self.program["learn_lists"]), 1)
        learn_list = self.program["learn_lists"][0]
        self.assertEqual(learn_list["name"], "mac_learn_digest_t")
        self.assertEqual(len(learn_list["elements"]), 2)
        learn_action = next(
            action
            for action in self.program["actions"]
            if action["name"] == "IngressImpl.learn_source"
        )
        assignments = {
            tuple(primitive["parameters"][0]["value"]): tuple(
                primitive["parameters"][1]["value"]
            )
            for primitive in learn_action["primitives"]
            if primitive["op"] == "assign"
        }
        digest_sources = [
            assignments[tuple(element["value"])] for element in learn_list["elements"]
        ]
        self.assertEqual(
            digest_sources,
            [
                ("ethernet", "src_mac"),
                ("standard_metadata", "ingress_port"),
            ],
        )
        digest_ops = [
            primitive
            for action in self.program["actions"]
            for primitive in action["primitives"]
            if primitive["op"] == "generate_digest"
        ]
        self.assertEqual(len(digest_ops), 1)
        list_id = int(digest_ops[0]["parameters"][1]["value"], 16)
        self.assertEqual(list_id, learn_list["id"])

    def test_source_known_action_has_no_dataplane_side_effects(self):
        action = next(
            action
            for action in self.program["actions"]
            if action["name"] == "IngressImpl.source_known"
        )
        self.assertEqual(action["primitives"], [])

    def test_learning_continues_to_destination_lookup(self):
        source = self.ingress_tables["IngressImpl.source_location"]
        destination_condition = next(
            condition
            for condition in self.ingress["conditionals"]
            if ("ethernet", "dst_mac") in expression_fields(condition["expression"])
        )
        self.assertEqual(source["base_default_next"], destination_condition["name"])
        self.assertEqual(
            set(source["next_tables"].values()),
            {destination_condition["name"]},
        )

    def test_known_destination_sets_one_egress_port(self):
        forward = next(
            action
            for action in self.program["actions"]
            if action["name"] == "IngressImpl.forward"
        )
        self.assertEqual(len(forward["primitives"]), 1)
        assignment = forward["primitives"][0]
        self.assertEqual(assignment["op"], "assign")
        target, value = assignment["parameters"]
        self.assertEqual(target["value"], ["standard_metadata", "egress_spec"])
        self.assertEqual(value, {"type": "runtime_data", "value": 0})

    def test_unknown_and_multicast_destinations_select_pre_group(self):
        destination = self.ingress_tables["IngressImpl.destination_mac"]
        default_action = action_for_id(self.program, destination["default_entry"]["action_id"])
        self.assertEqual(default_action["name"], "IngressImpl.flood")

        flood_actions = [
            action for action in self.program["actions"] if action["name"] == "IngressImpl.flood"
        ]
        self.assertGreaterEqual(len(flood_actions), 1)
        for action in flood_actions:
            assignments = [op for op in action["primitives"] if op["op"] == "assign"]
            self.assertEqual(len(assignments), 1)
            target, value = assignments[0]["parameters"]
            self.assertEqual(target["value"], ["standard_metadata", "mcast_grp"])
            self.assertEqual(int(value["value"], 16), 1)

        multicast_condition = next(
            condition
            for condition in self.ingress["conditionals"]
            if ("ethernet", "dst_mac") in expression_fields(condition["expression"])
        )
        for destination_mac in (0x01005E000001, 0xFFFFFFFFFFFF):
            self.assertTrue(
                evaluate_expression(
                    multicast_condition["expression"],
                    {("ethernet", "dst_mac"): destination_mac},
                )
            )
        self.assertFalse(
            evaluate_expression(
                multicast_condition["expression"],
                {("ethernet", "dst_mac"): 0x020000000001},
            )
        )
        multicast_table = self.ingress_tables[multicast_condition["true_next"]]
        multicast_action = action_for_id(
            self.program, multicast_table["default_entry"]["action_id"]
        )
        self.assertEqual(multicast_action["name"], "IngressImpl.flood")
        self.assertEqual(multicast_condition["false_next"], destination["name"])

    def test_invalid_sources_are_dropped_before_learning(self):
        condition = next(
            condition
            for condition in self.ingress["conditionals"]
            if ("ethernet", "src_mac") in expression_fields(condition["expression"])
        )
        self.assertEqual(condition["expression"]["value"]["op"], "or")
        for source_mac in (0x000000000000, 0x010000000001):
            self.assertTrue(
                evaluate_expression(
                    condition["expression"],
                    {("ethernet", "src_mac"): source_mac},
                )
            )
        self.assertFalse(
            evaluate_expression(
                condition["expression"],
                {("ethernet", "src_mac"): 0x020000000001},
            )
        )
        self.assertEqual(condition["false_next"], "IngressImpl.source_location")
        drop_table = next(
            table
            for table in self.ingress["tables"]
            if table["name"] == condition["true_next"]
        )
        drop_action = action_for_id(self.program, drop_table["default_entry"]["action_id"])
        self.assertEqual([op["op"] for op in drop_action["primitives"]], ["mark_to_drop"])

    def test_incomplete_ethernet_header_is_dropped(self):
        condition = next(
            condition
            for condition in self.ingress["conditionals"]
            if ("ethernet", "$valid$") in expression_fields(condition["expression"])
        )
        self.assertEqual(condition["expression"]["value"]["op"], "d2b")
        drop_table = next(
            table
            for table in self.ingress["tables"]
            if table["name"] == condition["false_next"]
        )
        drop_action = action_for_id(self.program, drop_table["default_entry"]["action_id"])
        self.assertEqual([op["op"] for op in drop_action["primitives"]], ["mark_to_drop"])

    def test_egress_prunes_the_ingress_port(self):
        condition = next(
            condition
            for condition in self.egress["conditionals"]
            if expression_fields(condition["expression"])
            == {
                ("standard_metadata", "egress_port"),
                ("standard_metadata", "ingress_port"),
            }
        )
        self.assertEqual(condition["expression"]["value"]["op"], "==")
        self.assertEqual(self.egress["init_table"], condition["name"])
        drop_table = next(
            table
            for table in self.egress["tables"]
            if table["name"] == condition["true_next"]
        )
        drop_action = action_for_id(self.program, drop_table["default_entry"]["action_id"])
        self.assertEqual([op["op"] for op in drop_action["primitives"]], ["mark_to_drop"])

    def test_bridge_does_not_rewrite_packet_headers(self):
        for action in self.program["actions"]:
            for primitive in action["primitives"]:
                if primitive["op"] != "assign":
                    continue
                target = primitive["parameters"][0]
                if target["type"] == "field":
                    self.assertNotEqual(target["value"][0], "ethernet")
        self.assertEqual(self.program["checksums"], [])
        self.assertEqual(self.program["calculations"], [])
        self.assertEqual(self.program["deparsers"][0]["order"], ["ethernet"])

    def test_no_registers_or_static_learned_entries(self):
        self.assertEqual(self.program["register_arrays"], [])
        for table_name in (
            "IngressImpl.source_location",
            "IngressImpl.destination_mac",
        ):
            self.assertFalse(self.ingress_tables[table_name].get("entries"))


if __name__ == "__main__":
    unittest.main()
