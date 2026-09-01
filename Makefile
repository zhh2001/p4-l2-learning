P4C ?= p4c-bm2-ss
PYTHON ?= python3
GO ?= go

BUILD_DIR := build
P4_SOURCE := p4/learning_switch.p4
BMV2_JSON := $(BUILD_DIR)/learning_switch.json
P4INFO_TEXT := $(BUILD_DIR)/learning_switch.p4info.txtpb
P4INFO_JSON := $(BUILD_DIR)/learning_switch.p4info.json
CONTROLLER := $(BUILD_DIR)/learning-controller

.PHONY: build p4 controller test clean

build: p4 controller

p4:
	mkdir -p $(BUILD_DIR)
	$(P4C) --std p4-16 --Werror \
		--p4runtime-files $(P4INFO_TEXT),$(P4INFO_JSON) \
		-o $(BMV2_JSON) $(P4_SOURCE)

controller:
	mkdir -p $(BUILD_DIR)
	$(GO) build -o $(CONTROLLER) ./controller

test: build
	$(GO) test ./...
	$(GO) vet ./...
	$(PYTHON) -m compileall -q mininet tests
	$(PYTHON) -m unittest discover -s tests -p 'test_*.py'

clean:
	$(RM) -r $(BUILD_DIR) mininet/__pycache__ tests/__pycache__
