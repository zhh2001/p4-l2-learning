P4C ?= p4c-bm2-ss
PYTHON ?= python3

BUILD_DIR := build
P4_SOURCE := p4/learning_switch.p4
BMV2_JSON := $(BUILD_DIR)/learning_switch.json
P4INFO_TEXT := $(BUILD_DIR)/learning_switch.p4info.txtpb
P4INFO_JSON := $(BUILD_DIR)/learning_switch.p4info.json

.PHONY: build p4 test clean

build: p4

p4:
	mkdir -p $(BUILD_DIR)
	$(P4C) --std p4-16 --Werror \
		--p4runtime-files $(P4INFO_TEXT),$(P4INFO_JSON) \
		-o $(BMV2_JSON) $(P4_SOURCE)

test: build
	$(PYTHON) -m compileall -q tests
	$(PYTHON) -m unittest discover -s tests -p 'test_*.py'

clean:
	$(RM) -r $(BUILD_DIR) tests/__pycache__
