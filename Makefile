.PHONY: test build vet proto test-e2e cluster-start cluster-stop cluster-verify

CLUSTER_DIR = /tmp/gookvs-cluster
CLUSTER_NODES = 5
CLUSTER_TOPOLOGY = 1=127.0.0.1:20160,2=127.0.0.1:20161,3=127.0.0.1:20162,4=127.0.0.1:20163,5=127.0.0.1:20164

test:
	go test ./pkg/... ./internal/... -v -count=1

test-e2e:
	go test ./e2e/... -v -count=1 -timeout 120s

build:
	go build -o gookvs-server ./cmd/gookvs-server
	go build -o gookvs-ctl ./cmd/gookvs-ctl

vet:
	go vet ./...

proto:
	@echo "TODO: protoc generation for kvproto"

cluster-start: build
	@echo "Starting $(CLUSTER_NODES)-node gookvs cluster..."
	@mkdir -p $(CLUSTER_DIR)
	@for i in $$(seq 1 $(CLUSTER_NODES)); do \
		GRPC_PORT=$$((20159 + $$i)); \
		STATUS_PORT=$$((20179 + $$i)); \
		DATA_DIR=$(CLUSTER_DIR)/node$$i; \
		PID_FILE=$(CLUSTER_DIR)/node$$i.pid; \
		LOG_FILE=$(CLUSTER_DIR)/node$$i.log; \
		mkdir -p $$DATA_DIR; \
		./gookvs-server \
			--store-id $$i \
			--addr 127.0.0.1:$$GRPC_PORT \
			--status-addr 127.0.0.1:$$STATUS_PORT \
			--data-dir $$DATA_DIR \
			--initial-cluster $(CLUSTER_TOPOLOGY) \
			> $$LOG_FILE 2>&1 & \
		echo $$! > $$PID_FILE; \
		echo "  Node $$i: gRPC=127.0.0.1:$$GRPC_PORT status=127.0.0.1:$$STATUS_PORT pid=$$(cat $$PID_FILE)"; \
	done
	@echo "Cluster started. Use 'make cluster-stop' to shut down."

cluster-stop:
	@echo "Stopping gookvs cluster..."
	@for i in $$(seq 1 $(CLUSTER_NODES)); do \
		PID_FILE=$(CLUSTER_DIR)/node$$i.pid; \
		if [ -f $$PID_FILE ]; then \
			PID=$$(cat $$PID_FILE); \
			if kill -0 $$PID 2>/dev/null; then \
				kill $$PID; \
				echo "  Node $$i (pid $$PID): stopped"; \
			else \
				echo "  Node $$i (pid $$PID): already stopped"; \
			fi; \
			rm -f $$PID_FILE; \
		fi; \
	done
	@rm -rf $(CLUSTER_DIR)
	@echo "Cluster stopped and data cleaned up."

cluster-verify:
	@echo "Verifying cluster replication..."
	@go run scripts/cluster-verify.go
